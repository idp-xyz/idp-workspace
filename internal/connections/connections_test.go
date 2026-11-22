package connections_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"idp-workspace/internal/authorization"
	"idp-workspace/internal/connections"
	"idp-workspace/internal/connections/connectionstest"
	"idp-workspace/internal/transport"
	"idp-workspace/internal/workspace"
)

// The connected services without a database or a browser: what the bridge receives, what the member's session records, what the
// member is told.

var (
	bob     = authorization.Principal{Subject: "bob", Tenant: "t1", Token: "tok-bob"}
	alice   = authorization.Principal{Subject: "alice", Tenant: "t1", Token: "tok-alice"}
	bobT2   = authorization.Principal{Subject: "bob", Tenant: "t2", Token: "tok-bob-t2"}
	t0      = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	authURL = func(state string) string {
		return "https://github.example/login/oauth/authorize?response_type=code&client_id=x&state=" + url.QueryEscape(state)
	}
)

const stateBob = "bob-state-0123456789abcdefghijklmnopqrstuvw"

type fixture struct {
	t       *testing.T
	bridge  *connectionstest.Bridge
	pending *connectionstest.Pending
	clock   *connectionstest.Clock
	svc     *connections.Service
	logs    *bytes.Buffer
	audits  []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, bridge: connectionstest.New(t), pending: &connectionstest.Pending{}, clock: &connectionstest.Clock{T: t0}, logs: &bytes.Buffer{}}
	f.svc = &connections.Service{Bridge: f.bridge.Downstream(), PublicURL: "https://workspace.example", Pending: f.pending, Now: f.clock.Now,
		Log: slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Audit: func(_ context.Context, p authorization.Principal, action, resource string, _ map[string]any) error {
			f.audits = append(f.audits, p.Subject+" "+action+" "+resource)
			return nil
		}}
	return f
}

func (f *fixture) respond(fn func(c connectionstest.Call) (int, string)) { f.bridge.Respond = fn }

// startsWith is the bridge that answers every authorization with this state.
func (f *fixture) authorizes(state string) {
	f.respond(func(c connectionstest.Call) (int, string) {
		if strings.HasSuffix(c.Path, "/authorization") {
			return 200, connectionstest.JSON(map[string]string{"url": authURL(state)})
		}
		if strings.HasSuffix(c.Path, "/callback") {
			return 200, `{"connection":"x","status":"connected"}`
		}
		if c.Method == "DELETE" {
			return 204, ""
		}
		if c.Method == "GET" {
			return 200, `{"connections":[]}`
		}
		return 404, `{"error":"not_found"}`
	})
}

func problem(t *testing.T, err error) *transport.Problem {
	t.Helper()
	var pb *transport.Problem
	if !errors.As(err, &pb) {
		t.Fatalf("want a problem, got %v", err)
	}
	return pb
}

func (f *fixture) callsTo(suffix string) int {
	n := 0
	for _, c := range f.bridge.Calls() {
		if strings.HasSuffix(c.Path, suffix) {
			n++
		}
	}
	return n
}

func TestEveryCallToTheBridgeCarriesTheMembersTokenExchangedForTheBridgesAudience(t *testing.T) {
	f := newFixture(t)
	f.authorizes(stateBob)
	ctx := context.Background()
	if _, err := f.svc.List(ctx, bob); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Connect(ctx, bob, "github", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Finish(ctx, bob, connections.Callback{Code: "c", State: stateBob}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Disconnect(ctx, bob, "github", false); err != nil {
		t.Fatal(err)
	}
	calls := f.bridge.Calls()
	if len(calls) != 4 {
		t.Fatalf("%d calls: %+v", len(calls), calls)
	}
	for _, c := range calls {
		// the double answers 401 to anything but "Bearer exchanged.<its audience>.<the member's token>": reaching here means the token
		// was exchanged; and it is the member's, not another's, and not the member's own token forwarded
		if c.Member != "tok-bob" || strings.Contains(c.Authorization, "Bearer tok-bob") && !strings.Contains(c.Authorization, "exchanged.") {
			t.Errorf("%s %s: bearer %q names %q", c.Method, c.Path, c.Authorization, c.Member)
		}
	}
	if aud := f.bridge.Audiences(); len(aud) == 0 || aud[0] != connectionstest.Audience {
		t.Errorf("exchanged for %v", aud)
	}
	if f.audits[0] != "bob connection.connect github" || f.audits[1] != "bob connection.disconnect github" {
		t.Errorf("audit: %v", f.audits)
	}
}

func TestConnectRecordsThePendingConnectOfThisMemberAndOnlyThenReturnsTheAddress(t *testing.T) {
	f := newFixture(t)
	f.authorizes(stateBob)
	out, err := f.svc.Connect(context.Background(), bob, "github", false, "/connections/github")
	if err != nil {
		t.Fatal(err)
	}
	if out.URL != authURL(stateBob) {
		t.Fatalf("the address handed on changed: %s", out.URL)
	}
	rec, ok := f.pending.Get("t1", "bob", sha(stateBob))
	if !ok {
		t.Fatal("nothing recorded for bob under the hash of the state in the address")
	}
	if rec.Connection != "github" || rec.Reviewer || rec.ReturnPath != "/connections/github" || !rec.ExpiresAt.Equal(t0.Add(10*time.Minute)) || rec.StateHash != sha(stateBob) {
		t.Fatalf("record: %+v", rec)
	}
	if f.pending.Len() != 1 {
		t.Fatalf("%d records", f.pending.Len())
	}
	if _, ok := f.pending.Get("t1", "alice", sha(stateBob)); ok {
		t.Fatal("recorded for another member")
	}
	if c := f.bridge.Calls()[0]; c.Method != "POST" || c.Path != "/internal/v1/connections/github/authorization" || c.Body != "" {
		t.Fatalf("the bridge saw %+v", c)
	}
	// the default return path
	f.authorizes("another-state-0123456789abcdefghijklmnopqrstu")
	if _, err := f.svc.Connect(context.Background(), bob, "github", false, ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.pending.Get("t1", "bob", sha("another-state-0123456789abcdefghijklmnopqrstu")); r.ReturnPath != "/connections" {
		t.Fatalf("default return path %q", r.ReturnPath)
	}
}

func TestConnectReviewerAccount(t *testing.T) {
	f := newFixture(t)
	f.authorizes(stateBob)
	if _, err := f.svc.Connect(context.Background(), bob, "github", true, ""); err != nil {
		t.Fatal(err)
	}
	if c := f.bridge.Calls()[0]; c.Path != "/internal/v1/connections/github/reviewer/authorization" {
		t.Fatalf("path %s", c.Path)
	}
	if r, _ := f.pending.Get("t1", "bob", sha(stateBob)); !r.Reviewer {
		t.Fatal("the pending connect does not say it is for the reviewer account")
	}
	// the bridge refuses a caller without its role: 403, nothing recorded
	g := newFixture(t)
	g.respond(func(connectionstest.Call) (int, string) { return 403, `{"error":"forbidden"}` })
	_, err := g.svc.Connect(context.Background(), bob, "github", true, "")
	if pb := problem(t, err); pb.Status != 403 || pb.Code != "FORBIDDEN" || g.pending.Len() != 0 {
		t.Fatalf("%+v, %d recorded", pb, g.pending.Len())
	}
}

func TestTheBrowserIsSentOnlyToAnHTTPSAddressTheBridgeGave(t *testing.T) {
	good := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef0123456789"
	for name, raw := range map[string]string{
		"plain http":      "http://github.example/authorize?state=" + good,
		"javascript":      "javascript:alert(1)//?state=" + good,
		"data":            "data:text/html,x?state=" + good,
		"relative":        "/authorize?state=" + good,
		"scheme relative": "//evil.example/authorize?state=" + good,
		"user info":       "https://trusted.example@evil.example/authorize?state=" + good,
		"no host":         "https:///authorize?state=" + good,
		"with a fragment": "https://github.example/authorize?state=" + good + "#x",
		"no state":        "https://github.example/authorize?code_challenge=abc",
		"two states":      "https://github.example/authorize?state=" + good + "&state=" + good + "2",
		"a short state":   "https://github.example/authorize?state=abc",
		"a space":         "https://github.example/authorize?state=" + good + " x",
		"a newline":       "https://github.example/authorize?state=" + good + "\nLocation: x",
		"empty":           "",
		"another scheme":  "ftp://github.example/authorize?state=" + good,
		"http for a test origin without the setting": "http://127.0.0.1:9/authorize?state=" + good,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.respond(func(connectionstest.Call) (int, string) {
				return 200, connectionstest.JSON(map[string]string{"url": raw})
			})
			_, err := f.svc.Connect(context.Background(), bob, "github", false, "")
			if pb := problem(t, err); pb.Status != 502 || pb.Code != connections.CodeUnavailable {
				t.Fatalf("%+v", pb)
			}
			if f.pending.Len() != 0 {
				t.Fatal("a connect was recorded for an address that is not followed")
			}
			if strings.Contains(f.logs.String(), good) {
				t.Fatal("the address (it carries the state) was logged")
			}
		})
	}
	// the test settings allow one http origin, exactly
	f := newFixture(t)
	f.svc.TestOrigins = []string{"127.0.0.1:9"}
	for raw, ok := range map[string]bool{"http://127.0.0.1:9/authorize?state=" + good: true, "http://127.0.0.1:10/authorize?state=" + good: false, "http://127.0.0.1/authorize?state=" + good: false} {
		f.respond(func(connectionstest.Call) (int, string) {
			return 200, connectionstest.JSON(map[string]string{"url": raw})
		})
		_, err := f.svc.Connect(context.Background(), bob, "github", false, "")
		if (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestOpenRedirectAttemptsInTheReturnPathAreRefusedBeforeAnythingHappens(t *testing.T) {
	bad := []string{
		"https://evil.example/", "http://evil.example", "//evil.example", "//evil.example/x", "/\\evil.example", "\\\\evil.example", "\\/evil.example",
		"evil.example", "connections", "javascript:alert(1)", "data:text/html,x", "/ /evil.example", "/\tevil.example", "/\nevil.example", "/%0d%0aSet-Cookie:x",
		"/a\\b", "/é", "/" + strings.Repeat("a", 200), "", " /connections", "/connections ", "///evil.example", "/\x00x",
		"/oauth/callback", "/oauth/callback?x=1", "/%2F/evil.example", "/%5Cevil.example", "/%2f%2fevil.example", "mailto:a@b.c", "//\\evil.example",
	}
	for _, p := range bad {
		if p == "" {
			continue // "" means the default; checked below
		}
		f := newFixture(t)
		f.authorizes(stateBob)
		_, err := f.svc.Connect(context.Background(), bob, "github", false, p)
		if err == nil {
			t.Errorf("%q: accepted", p)
			continue
		}
		if pb := problem(t, err); pb.Status != 422 || pb.Code != "INVALID" {
			t.Errorf("%q: %+v", p, pb)
		}
		if len(f.bridge.Calls()) != 0 || f.pending.Len() != 0 {
			t.Errorf("%q: the bridge was called or a connect recorded", p)
		}
		if connections.ValidReturnPath(p) {
			t.Errorf("%q: ValidReturnPath accepts it", p)
		}
	}
	for _, p := range []string{"/", "/connections", "/connections/github", "/connections/github?from=chat", "/?view=connections", "/a/b/c#frag"} {
		if !connections.ValidReturnPath(p) {
			t.Errorf("%q: refused", p)
		}
	}
}

func TestACallbackWhoseStateThisMemberDidNotStartNeverReachesTheBridge(t *testing.T) {
	f := newFixture(t)
	f.authorizes(stateBob)
	if _, err := f.svc.Connect(context.Background(), bob, "github", false, ""); err != nil {
		t.Fatal(err)
	}
	before := len(f.bridge.Calls())
	for name, c := range map[string]struct {
		who authorization.Principal
		cb  connections.Callback
	}{
		"alice brings bob's state":             {alice, connections.Callback{Code: "x", State: stateBob}},
		"bob of another tenant":                {bobT2, connections.Callback{Code: "x", State: stateBob}},
		"a state nobody started":               {bob, connections.Callback{Code: "x", State: "never-started-0123456789abcdefghijklmnopqrstu"}},
		"no state":                             {bob, connections.Callback{Code: "x"}},
		"the code in place of the state":       {bob, connections.Callback{Code: stateBob}},
		"an error answer with a foreign state": {alice, connections.Callback{Error: "access_denied", State: stateBob}},
		"the state with other letter case":     {bob, connections.Callback{Code: "x", State: strings.ToUpper(stateBob)}},
		"the state with a space":               {bob, connections.Callback{Code: "x", State: stateBob + " "}},
	} {
		_, err := f.svc.Finish(context.Background(), c.who, c.cb)
		if pb := problem(t, err); pb.Status != 400 || pb.Code != connections.CodeStateInvalid {
			t.Errorf("%s: %+v", name, pb)
		}
	}
	if len(f.bridge.Calls()) != before {
		t.Fatalf("the bridge received %d calls it should not have", len(f.bridge.Calls())-before)
	}
	// the refusals consumed nothing of bob's: his own callback still works
	if _, err := f.svc.Finish(context.Background(), bob, connections.Callback{Code: "x", State: stateBob}); err != nil {
		t.Fatalf("bob's own callback after the others': %v", err)
	}
	if f.callsTo("/callback") != 1 {
		t.Fatalf("%d callbacks reached the bridge", f.callsTo("/callback"))
	}
}

func TestAStateIsUsedOnce(t *testing.T) {
	f := newFixture(t)
	f.authorizes(stateBob)
	ctx := context.Background()
	if _, err := f.svc.Connect(ctx, bob, "github", false, "/connections"); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Finish(ctx, bob, connections.Callback{Code: "c1", State: stateBob})
	if err != nil || res.Connection != "github" || res.Status != "connected" || res.ReturnPath != "/connections" {
		t.Fatalf("%+v %v", res, err)
	}
	_, err = f.svc.Finish(ctx, bob, connections.Callback{Code: "c1", State: stateBob})
	if pb := problem(t, err); pb.Code != connections.CodeStateInvalid {
		t.Fatalf("the second use: %+v", pb)
	}
	if f.callsTo("/callback") != 1 {
		t.Fatalf("%d callbacks reached the bridge", f.callsTo("/callback"))
	}
	// a use that the bridge refused is a use too: the state is consumed before the bridge is asked
	g := newFixture(t)
	g.authorizes(stateBob)
	_, _ = g.svc.Connect(ctx, bob, "github", false, "")
	g.respond(func(connectionstest.Call) (int, string) {
		return 400, `{"error":"authorization_failed","reason":"iss_mismatch"}`
	})
	if _, err := g.svc.Finish(ctx, bob, connections.Callback{Code: "c", State: stateBob}); err == nil {
		t.Fatal("the failure was not passed on")
	}
	g.authorizes(stateBob)
	if _, err := g.svc.Finish(ctx, bob, connections.Callback{Code: "c", State: stateBob}); problem(t, err).Code != connections.CodeStateInvalid {
		t.Fatal("a state the bridge refused was usable again")
	}
}

func TestAnExpiredStateIsRefusedAndConsumed(t *testing.T) {
	f := newFixture(t)
	f.authorizes(stateBob)
	ctx := context.Background()
	_, _ = f.svc.Connect(ctx, bob, "github", false, "")
	f.clock.Advance(10*time.Minute - time.Second)
	// still valid at 9:59
	g := newFixture(t)
	g.authorizes(stateBob)
	_, _ = g.svc.Connect(ctx, bob, "github", false, "")
	g.clock.Advance(10*time.Minute - time.Second)
	if _, err := g.svc.Finish(ctx, bob, connections.Callback{Code: "c", State: stateBob}); err != nil {
		t.Fatalf("just before the expiry: %v", err)
	}
	f.clock.Advance(time.Second)
	_, err := f.svc.Finish(ctx, bob, connections.Callback{Code: "c", State: stateBob})
	if pb := problem(t, err); pb.Status != 400 || pb.Code != connections.CodeStateExpired {
		t.Fatalf("%+v", pb)
	}
	if f.callsTo("/callback") != 0 {
		t.Fatal("the bridge was asked about an expired state")
	}
	if _, err := f.svc.Finish(ctx, bob, connections.Callback{Code: "c", State: stateBob}); problem(t, err).Code != connections.CodeStateInvalid {
		t.Fatal("an expired state stayed")
	}
}

func TestTheCallbackPassesCodeStateIssAndErrorExactlyAsReceivedForTheRecordedConnection(t *testing.T) {
	cases := []connections.Callback{
		{Code: "a/b+c=d%20e é\"x\"\\", State: stateBob, Iss: "https://as.example/realms/x"},
		{State: stateBob, Error: "access_denied"},
		{Code: "c", State: stateBob, Iss: ""},
		{Code: " spaced ", State: stateBob, Iss: "https://as.example/?a=b&c=d"},
	}
	for _, cb := range cases {
		f := newFixture(t)
		f.authorizes(stateBob)
		_, _ = f.svc.Connect(context.Background(), bob, "notion", false, "")
		if _, err := f.svc.Finish(context.Background(), bob, cb); err != nil {
			t.Fatal(err)
		}
		var got connectionstest.Call
		for _, c := range f.bridge.Calls() {
			if strings.HasSuffix(c.Path, "/callback") {
				got = c
			}
		}
		if got.Method != "POST" || got.Path != "/internal/v1/connections/notion/callback" {
			t.Fatalf("%+v", got)
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"code": cb.Code, "state": cb.State, "iss": cb.Iss, "error": cb.Error}
		if len(body) != 4 || body["code"] != want["code"] || body["state"] != want["state"] || body["iss"] != want["iss"] || body["error"] != want["error"] {
			t.Errorf("sent %v, received %v", body, want)
		}
	}
	// the connection is the one recorded at the start, whatever else says: the callback has no connection field to name another
	f := newFixture(t)
	f.authorizes(stateBob)
	_, _ = f.svc.Connect(context.Background(), bob, "github", true, "/connections/github")
	res, _ := f.svc.Finish(context.Background(), bob, connections.Callback{Code: "c", State: stateBob})
	if res == nil || res.Connection != "github" || !res.Reviewer || res.ReturnPath != "/connections/github" {
		t.Fatalf("%+v", res)
	}
}

func TestWhatTheMemberIsToldComesFromFixedCodesOnly(t *testing.T) {
	hostile := `<script>alert(1)</script> https://phish.example/login Authorization: Bearer abc`
	table := []struct {
		status        int
		body          string
		wantStatus    int
		wantCode, msg string
	}{
		{400, `{"error":"invalid_state"}`, 400, connections.CodeStateInvalid, ""},
		{400, `{"error":"state_expired"}`, 400, connections.CodeStateExpired, ""},
		{400, `{"error":"invalid_request"}`, 400, connections.CodeRequestInvalid, ""},
		{400, `{"error":"authorization_failed","reason":"iss_mismatch"}`, 400, connections.CodeFailed, "iss_mismatch"},
		{400, `{"error":"authorization_failed","reason":"` + hostile + `"}`, 400, connections.CodeFailed, "authorization_failed"},
		{400, `{"error":"authorization_denied","reason":"access_denied"}`, 400, connections.CodeDenied, "access_denied"},
		{400, `{"error":"authorization_denied","reason":"` + hostile + `"}`, 400, connections.CodeDenied, "authorization_failed"},
		{409, `{"error":"configuration_changed"}`, 409, connections.CodeChanged, ""},
		{409, `{"error":"authorization_server_refused","reason":"issuer_mismatch"}`, 409, connections.CodeRefused, "issuer_mismatch"},
		{409, `{"error":"authorization_server_refused","reason":"` + hostile + `"}`, 409, connections.CodeRefused, "refused"},
		{502, `{"error":"authorization_server_unavailable"}`, 502, connections.CodeServer, ""},
		{404, `{"error":"not_found"}`, 404, "NOT_FOUND", ""},
		// what is not in the bridge's fixed set is "not available", whatever it says
		{400, `{"error":"` + hostile + `"}`, 502, connections.CodeUnavailable, ""},
		{500, `{"error":"internal"}`, 502, connections.CodeUnavailable, ""},
		{401, `{"error":"unauthenticated"}`, 502, connections.CodeUnavailable, ""}, // the member is signed in: the workspace's credential at the bridge failed
		{400, hostile, 502, connections.CodeUnavailable, ""},
	}
	for i, c := range table {
		f := newFixture(t)
		f.authorizes(stateBob)
		_, _ = f.svc.Connect(context.Background(), bob, "github", false, "")
		f.respond(func(connectionstest.Call) (int, string) { return c.status, c.body })
		_, err := f.svc.Finish(context.Background(), bob, connections.Callback{Code: "c", State: stateBob})
		pb := problem(t, err)
		if pb.Status != c.wantStatus || pb.Code != c.wantCode || (c.msg != "" && pb.Message != c.msg) {
			t.Errorf("%d %s: got %d %s %q", i, c.body, pb.Status, pb.Code, pb.Message)
		}
		shown, _ := json.Marshal(pb)
		for _, leak := range []string{"script", "phish", "Bearer", "abc"} {
			if strings.Contains(string(shown), leak) {
				t.Errorf("%d: a third party's words reach the member: %s", i, shown)
			}
		}
		if pb.Code == "" || len(pb.Message) == 0 || len(pb.Message) > 100 {
			t.Errorf("%d: %+v", i, pb)
		}
	}
	// the same for a refusal at the start of a connect
	g := newFixture(t)
	g.respond(func(connectionstest.Call) (int, string) {
		return 409, `{"error":"authorization_server_refused","reason":"` + hostile + `"}`
	})
	_, err := g.svc.Connect(context.Background(), bob, "github", false, "")
	if pb := problem(t, err); pb.Code != connections.CodeRefused || pb.Message != "refused" {
		t.Fatalf("%+v", pb)
	}
	// not JSON where JSON is promised
	g.respond(func(connectionstest.Call) (int, string) { return 200, hostile })
	if _, err := g.svc.List(context.Background(), bob); problem(t, err).Code != connections.CodeUnavailable {
		t.Fatal("a list that is not JSON")
	}
	// an unreachable bridge is unavailable, and says nothing of its address
	g.bridge.Server.Close()
	_, err = g.svc.List(context.Background(), bob)
	if pb := problem(t, err); pb.Code != connections.CodeUnavailable || strings.Contains(pb.Message, "127.0.0.1") {
		t.Fatalf("%+v", pb)
	}
}

func TestNeitherCodeNorStateNorTokensAreEverLogged(t *testing.T) {
	const code, state = "SECRET-CODE-7f3a91", "SECRET-STATE-0123456789abcdefghijklmnopqrstu"
	f := newFixture(t)
	f.authorizes(state)
	ctx := context.Background()
	_, _ = f.svc.Connect(ctx, bob, "github", false, "")
	_, _ = f.svc.Finish(ctx, bob, connections.Callback{Code: code, State: state, Iss: "https://as.example"})
	_, _ = f.svc.Finish(ctx, bob, connections.Callback{Code: code, State: state}) // the second use
	_, _ = f.svc.Finish(ctx, alice, connections.Callback{Code: code, State: state})
	_, _ = f.svc.Finish(ctx, bob, connections.Callback{Code: code, State: "other-" + state})
	f.authorizes(state + "2")
	_, _ = f.svc.Connect(ctx, bob, "github", false, "")
	f.respond(func(connectionstest.Call) (int, string) {
		return 400, `{"error":"authorization_failed","reason":"iss_mismatch"}`
	})
	_, _ = f.svc.Finish(ctx, bob, connections.Callback{Code: code, State: state + "2", Error: "access_denied"})
	f.respond(func(connectionstest.Call) (int, string) { return 502, `{"error":"` + code + `"}` })
	_, _ = f.svc.List(ctx, bob)
	f.respond(func(connectionstest.Call) (int, string) {
		return 200, connectionstest.JSON(map[string]string{"url": "http://evil.example/?state=" + state})
	})
	_, _ = f.svc.Connect(ctx, bob, "github", false, "")
	_ = f.svc.Disconnect(ctx, bob, "github", false)
	if f.logs.Len() == 0 {
		t.Fatal("the test logged nothing: it proves nothing")
	}
	for _, secret := range []string{code, state, "tok-bob", "tok-alice", "exchanged.", "s3cret", sha(state)} {
		if strings.Contains(f.logs.String(), secret) {
			t.Errorf("%q is in the logs:\n%s", secret, f.logs.String())
		}
	}
}

func TestListShowsWhatTheBridgeReportsChecked(t *testing.T) {
	f := newFixture(t)
	f.respond(func(c connectionstest.Call) (int, string) {
		return 200, `{"connections":[
		 {"id":"github","title":"GitHub","status":"connected","scope":"repo read:user"},
		 {"id":"notion","title":"Notion","status":"reauthorization_required","reason":"insufficient_scope"},
		 {"id":"linear","title":"Linear","status":"not_connected","reason":"insufficient_scope","reviewer":{"status":"reauthorization_required","reason":"expired","scope":"read"}},
		 {"id":"open","title":"Docs","status":"not_required"},
		 {"id":"bad/id","title":"x","status":"connected"},
		 {"id":"https://evil.example","title":"x","status":"connected"},
		 {"id":"weird","title":"x","status":"approved"},
		 {"id":"mark","title":"‮Evil\u0007 <b>title</b>","status":"connected","scope":"ok ‮bad  tok\n"},
		 {"id":"untitled","title":"  ","status":"not_connected","reviewer":{"status":"bogus"}}]}`
	})
	out, err := f.svc.List(context.Background(), bob)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]workspace.Connection{}
	for _, c := range out.Connections {
		got[c.ID] = c
	}
	if len(out.Connections) != 6 || got["bad/id"].ID != "" || got["https://evil.example"].ID != "" || got["weird"].ID != "" {
		t.Fatalf("what is not shown is shown: %+v", out.Connections)
	}
	if c := got["notion"]; c.Status != "reauthorization_required" || c.Reason != "insufficient_scope" {
		t.Errorf("notion %+v", c)
	}
	if c := got["linear"]; c.Reason != "" || c.Reviewer == nil || c.Reviewer.Status != "reauthorization_required" || c.Reviewer.Reason != "expired" || c.Reviewer.Scope != "read" {
		t.Errorf("a reason without a reauthorization is dropped, the reviewer is passed on: %+v %+v", c, c.Reviewer)
	}
	if c := got["github"]; c.Reviewer != nil || c.Scope != "repo read:user" {
		t.Errorf("no reviewer unless the bridge gives one: %+v", c)
	}
	if c := got["mark"]; strings.ContainsAny(c.Title, "‮\u0007") || c.Scope != "ok tok" {
		t.Errorf("title %q scope %q", c.Title, c.Scope)
	}
	if c := got["untitled"]; c.Title != "untitled" || c.Reviewer != nil {
		t.Errorf("untitled %+v", c)
	}
	if c := f.bridge.Calls()[0]; c.Method != "GET" || c.Path != "/internal/v1/connections" || c.Body != "" {
		t.Errorf("%+v", c)
	}
}

func TestDisconnect(t *testing.T) {
	f := newFixture(t)
	f.respond(func(connectionstest.Call) (int, string) { return 204, "" })
	ctx := context.Background()
	if err := f.svc.Disconnect(ctx, bob, "github", false); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Disconnect(ctx, bob, "github", true); err != nil {
		t.Fatal(err)
	}
	cs := f.bridge.Calls()
	if cs[0].Method != "DELETE" || cs[0].Path != "/internal/v1/connections/github/grant" || cs[1].Path != "/internal/v1/connections/github/reviewer/grant" {
		t.Fatalf("%+v", cs)
	}
	for _, id := range []string{"../x", "a/b", "", "a b", strings.Repeat("a", 65), "a?b", "é"} {
		before := len(f.bridge.Calls())
		if err := f.svc.Disconnect(ctx, bob, id, false); problem(t, err).Status != 404 {
			t.Errorf("%q: %v", id, err)
		}
		if _, err := f.svc.Connect(ctx, bob, id, false, ""); problem(t, err).Status != 404 {
			t.Errorf("%q: %v", id, err)
		}
		if len(f.bridge.Calls()) != before {
			t.Errorf("%q: the bridge was called", id)
		}
	}
}

func TestClientDocumentIsServedOnlyUnderItsOwnClientID(t *testing.T) {
	f := newFixture(t)
	own := "https://workspace.example/oauth/client/github.json"
	f.bridge.Metadata = map[string]string{
		"github":    `{"client_id":"` + own + `","client_name":"IDP","redirect_uris":["https://workspace.example/oauth/callback"],"token_endpoint_auth_method":"none"}`,
		"other":     `{"client_id":"https://workspace.example/oauth/client/github.json","client_name":"copy of another"}`, // another connection's document
		"foreign":   `{"client_id":"https://evil.example/oauth/client/foreign.json"}`,
		"http":      `{"client_id":"http://workspace.example/oauth/client/http.json"}`,
		"prefix":    `{"client_id":"https://workspace.example/oauth/client/prefix.json/"}`,
		"noid":      `{"client_name":"x"}`,
		"numeric":   `{"client_id":5}`,
		"array":     `[{"client_id":"https://workspace.example/oauth/client/array.json"}]`,
		"garbage":   `{"client_id":"https://workspace.example/oauth/client/garbage.json"} trailing`,
		"notjson":   `<html>`,
		"duplicate": `{"client_id":"https://evil.example/x","client_id":"https://workspace.example/oauth/client/duplicate.json"}`,
		"big":       `{"client_id":"https://workspace.example/oauth/client/big.json","x":"` + strings.Repeat("a", 70<<10) + `"}`,
	}
	ctx := context.Background()
	doc, ok := f.svc.ClientDocument(ctx, "github")
	if !ok {
		t.Fatal("its own document is not served")
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil || m["client_id"] != own || m["client_name"] != "IDP" {
		t.Fatalf("%s %v", doc, err)
	}
	for _, id := range []string{"other", "foreign", "http", "prefix", "noid", "numeric", "array", "garbage", "notjson", "big", "missing", "bad/id", "../github", ""} {
		if d, ok := f.svc.ClientDocument(ctx, id); ok {
			t.Errorf("%s: served %s", id, d)
		}
	}
	// the document of a connection whose own client_id is the address it is served at, once re-encoded: one value for a key
	if d, ok := f.svc.ClientDocument(ctx, "duplicate"); !ok || strings.Count(string(d), "client_id") != 1 || !strings.Contains(string(d), "/duplicate.json") {
		t.Errorf("duplicate keys: %s %v", d, ok)
	}
	// public: no credential reaches the bridge for it, and nothing is exchanged
	for _, c := range f.bridge.Calls() {
		if c.Authorization != "" {
			t.Errorf("a credential went with %s", c.Path)
		}
	}
	if len(f.bridge.Audiences()) != 0 {
		t.Error("a token was exchanged for a public document")
	}
}

func TestClientDocumentIsCachedBrieflyAndAMissLess(t *testing.T) {
	f := newFixture(t)
	f.bridge.Metadata["github"] = `{"client_id":"https://workspace.example/oauth/client/github.json"}`
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, ok := f.svc.ClientDocument(ctx, "github"); !ok {
			t.Fatal("not served")
		}
	}
	if n := f.callsTo("/client-metadata"); n != 1 {
		t.Fatalf("%d fetches within the cache time", n)
	}
	f.clock.Advance(61 * time.Second)
	_, _ = f.svc.ClientDocument(ctx, "github")
	if n := f.callsTo("/client-metadata"); n != 2 {
		t.Fatalf("%d fetches after it", n)
	}
	// a document the bridge stops giving stops being served once the cache time is over
	delete(f.bridge.Metadata, "github")
	f.clock.Advance(61 * time.Second)
	if _, ok := f.svc.ClientDocument(ctx, "github"); ok {
		t.Fatal("a document the bridge no longer has is still served")
	}
	// a miss is remembered shortly, so that a loop of requests for an id does not become a loop of calls
	before := f.callsTo("/client-metadata")
	for i := 0; i < 5; i++ {
		_, _ = f.svc.ClientDocument(ctx, "nothing")
	}
	if n := f.callsTo("/client-metadata") - before; n != 1 {
		t.Fatalf("%d fetches for a missing id", n)
	}
	// the cache is bounded
	for i := 0; i < 600; i++ {
		_, _ = f.svc.ClientDocument(ctx, "id"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+strings.Repeat("y", i/26))
	}
}

func TestCallbackAndClientURLs(t *testing.T) {
	s := &connections.Service{PublicURL: "https://workspace.example"}
	if s.CallbackURL() != "https://workspace.example/oauth/callback" || s.ClientURL("github") != "https://workspace.example/oauth/client/github.json" {
		t.Fatal(s.CallbackURL(), s.ClientURL("github"))
	}
	_ = http.StatusOK
}

func sha(s string) string {
	// the hash the service takes of a state: SHA-256, hex (the test states it independently of the code under test)
	return sha256hex(s)
}
