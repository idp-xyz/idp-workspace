package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"idp-workspace/adapters/identity"
	"idp-workspace/internal/api"
	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/observability"
)

// fakeCMI is idp-cmi as the member sees it: records per subject, and the Authorization it was
// called with. The fake token endpoint turns a member's token into "exchanged:<name>".
type fakeCMI struct {
	mu      sync.Mutex
	records map[string][]string // subject -> record ids (newest first as listed)
	calls   []recorded
	srv     *httptest.Server
	sts     *httptest.Server
}

const (
	bobRec1  = "11111111-1111-4111-8111-111111111111"
	bobRec2  = "22222222-2222-4222-8222-222222222222"
	carolRec = "33333333-3333-4333-8333-333333333333"
)

func newFakeCMI(t *testing.T, e *env) *fakeCMI {
	f := &fakeCMI{records: map[string][]string{"bob": {bobRec2, bobRec1}, "carol": {carolRec}}}
	f.sts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		who := ""
		for name, tok := range e.tokens {
			if tok == r.PostForm.Get("subject_token") {
				who = name
			}
		}
		if r.PostForm.Get("audience") != "idp-cmi" || who == "" {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"invalid_target"}`)
			return
		}
		_, _ = io.WriteString(w, fmt.Sprintf(`{"access_token":"exchanged:%s","expires_in":300}`, who))
	}))
	t.Cleanup(f.sts.Close)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		x := recorded{Path: r.URL.RequestURI(), Auth: r.Header.Get("Authorization"), Raw: raw}
		who := strings.TrimPrefix(x.Auth, "Bearer exchanged:")
		f.mu.Lock()
		f.calls = append(f.calls, x)
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if who == x.Auth || f.records[who] == nil && who != "dan" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"code":"UNAUTHENTICATED"}}`)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/memory/records":
			var recs []string
			for i, id := range f.records[who] {
				recs = append(recs, fmt.Sprintf(`{"recordId":%q,"content":"secret fact %s","tags":["conversation"],"provenance":{"source":"idp-host","ref":"run:ws_%d","observedAt":"2026-11-0%dT10:00:00Z"},"validity":{"validUntil":"2026-12-0%dT10:00:00Z"},"status":"active","createdAt":"2026-11-0%dT10:00:00Z"}`, id, id[:2], i, 5-i, 5-i, 5-i))
			}
			_, _ = io.WriteString(w, `{"records":[`+strings.Join(recs, ",")+`]}`)
		case r.Method == "POST" && r.URL.Path == "/v1/memory/revoke":
			var body struct {
				RecordID string `json:"recordId"`
			}
			_ = json.Unmarshal(raw, &body)
			for i, id := range f.records[who] {
				if id == body.RecordID {
					f.records[who] = append(append([]string{}, f.records[who][:i]...), f.records[who][i+1:]...)
					_, _ = io.WriteString(w, `{"revoked":1}`)
					return
				}
			}
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"code":"NOT_FOUND","message":"no such record"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCMI) revokeCalls() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, c := range f.calls {
		if strings.HasPrefix(c.Path, "/v1/memory/revoke") {
			out = append(out, c)
		}
	}
	return out
}

func (e *env) apiWithCMI(t *testing.T, cmi *dispatch.Downstream) *httptest.Server {
	verifier, _ := identity.NewDevVerifier(profile, []byte(devSecret))
	a := &api.API{Store: e.store, Auth: verifier, CMI: cmi, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: observability.NewMetrics(), Version: "it"}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func TestMyMemory(t *testing.T) {
	e := newEnv(t)
	cmi := newFakeCMI(t, e)
	ds := &dispatch.Downstream{Endpoint: cmi.srv.URL, Auth: dispatch.AuthExchange, Exchange: &dispatch.Exchanger{TokenURL: cmi.sts.URL, ClientID: "idp-workspace",
		ClientSecret: func() (string, error) { return "s", nil }, Audience: "idp-cmi"}}
	main := e.api
	e.api = e.apiWithCMI(t, ds)
	t.Cleanup(func() { e.api = main })

	audited := func() []map[string]any {
		var out []map[string]any
		for _, ev := range e.must(200, "GET", "/api/v1/audit", "alice", nil)["events"].([]any) {
			if m := ev.(map[string]any); m["action"] == "memory.delete" {
				out = append(out, m)
			}
		}
		return out
	}

	t.Run("list-uses-the-exchanged-token", func(t *testing.T) {
		out := e.must(200, "GET", "/v1/me/memory", "bob", nil)
		ms := out["memories"].([]any)
		if len(ms) != 2 {
			t.Fatalf("%v", ms)
		}
		first := ms[0].(map[string]any)
		if first["id"] != bobRec2 || first["source"] != "idp-host" || first["ref"] != "run:ws_0" || first["validUntil"] == "" || first["content"] != "secret fact 22" {
			t.Fatalf("%v", first)
		}
		c := cmi.calls[len(cmi.calls)-1]
		if c.Path != "/v1/memory/records?limit=100" || c.Auth != "Bearer exchanged:bob" || strings.Contains(c.Auth, e.tokens["bob"]) {
			t.Fatalf("%+v", c)
		}
		if ms := e.must(200, "GET", "/v1/me/memory", "carol", nil)["memories"].([]any); len(ms) != 1 {
			t.Fatalf("carol sees %v", ms)
		}
		e.must(403, "GET", "/v1/me/memory", "svc-eer", nil) // a producer is not a member
	})

	t.Run("another-member-cannot-delete-by-guessing", func(t *testing.T) {
		before := len(cmi.revokeCalls())
		st, out := e.call("DELETE", "/v1/me/memory/"+carolRec, "bob", nil)
		if st != 404 || code(out) != "NOT_FOUND" || strings.Contains(fmt.Sprint(out), "secret") {
			t.Fatalf("%d %v", st, out)
		}
		calls := cmi.revokeCalls()
		if len(calls) != before+1 { // asked once, never retried
			t.Fatalf("%d calls", len(calls)-before)
		}
		if last := calls[len(calls)-1]; last.Auth != "Bearer exchanged:bob" {
			t.Fatalf("%+v", last)
		}
		if got := cmi.records["carol"]; len(got) != 1 {
			t.Fatalf("carol's record is gone: %v", got)
		}
		if len(audited()) != 0 {
			t.Fatal("a refused deletion was audited as done")
		}
		e.must(422, "DELETE", "/v1/me/memory/not-a-uuid", "bob", nil)
		e.must(422, "DELETE", "/v1/me/memory/"+url.PathEscape("../x"), "bob", nil)
	})

	t.Run("delete-one", func(t *testing.T) {
		before := len(cmi.revokeCalls())
		if out := e.must(200, "DELETE", "/v1/me/memory/"+bobRec1, "bob", nil); out["revoked"] != float64(1) {
			t.Fatalf("%v", out)
		}
		c := cmi.revokeCalls()[before]
		if c.Auth != "Bearer exchanged:bob" || !strings.Contains(string(c.Raw), `"recordId":"`+bobRec1+`"`) || !strings.Contains(string(c.Raw), `"reason"`) {
			t.Fatalf("%+v %s", c, c.Raw)
		}
		evs := audited()
		if len(evs) != 1 || evs[0]["subject"] != "bob" || evs[0]["resourceId"] != bobRec1 || fmt.Sprint(evs[0]["detail"]) != "map[count:1]" {
			t.Fatalf("%v", evs)
		}
		e.must(404, "DELETE", "/v1/me/memory/"+bobRec1, "bob", nil) // already gone: CMI 404 passes through
	})

	t.Run("delete-all", func(t *testing.T) {
		cmi.mu.Lock()
		cmi.records["bob"] = []string{bobRec2, bobRec1}
		cmi.mu.Unlock()
		before := len(cmi.revokeCalls())
		if out := e.must(200, "DELETE", "/v1/me/memory", "bob", nil); out["revoked"] != float64(2) {
			t.Fatalf("%v", out)
		}
		calls := cmi.revokeCalls()[before:]
		if len(calls) != 2 {
			t.Fatalf("%d revoke calls", len(calls))
		}
		for i, id := range []string{bobRec2, bobRec1} {
			if calls[i].Auth != "Bearer exchanged:bob" || !strings.Contains(string(calls[i].Raw), id) {
				t.Fatalf("%+v", calls[i])
			}
		}
		if len(cmi.records["carol"]) != 1 {
			t.Fatal("deleting bob's memory touched carol's")
		}
		if out := e.must(200, "DELETE", "/v1/me/memory", "bob", nil); out["revoked"] != float64(0) {
			t.Fatalf("%v", out)
		}
		evs := audited()
		if len(evs) < 2 {
			t.Fatalf("%v", evs)
		}
		for _, ev := range evs {
			if strings.Contains(fmt.Sprint(ev), "secret fact") {
				t.Fatalf("content in the audit: %v", ev)
			}
		}
		if ev := evs[1]; ev["resourceId"] != nil || fmt.Sprint(ev["detail"]) != "map[count:2]" {
			t.Fatalf("%v", evs)
		}
	})

	t.Run("cmi-unreachable-and-exchange-refused", func(t *testing.T) {
		bad := &dispatch.Downstream{Endpoint: "http://127.0.0.1:1", Auth: dispatch.AuthForward}
		e.api = e.apiWithCMI(t, bad)
		if st, out := e.call("GET", "/v1/me/memory", "bob", nil); st != 502 || code(out) != "TARGET_UNAVAILABLE" {
			t.Fatalf("%d %v", st, out)
		}
		refused := &dispatch.Downstream{Endpoint: cmi.srv.URL, Auth: dispatch.AuthExchange, Exchange: &dispatch.Exchanger{TokenURL: cmi.sts.URL, ClientID: "c",
			ClientSecret: func() (string, error) { return "s", nil }, Audience: "other"}}
		e.api = e.apiWithCMI(t, refused)
		if st, out := e.call("DELETE", "/v1/me/memory/"+bobRec1, "bob", nil); st != 502 || code(out) != "TARGET_UNAVAILABLE" {
			t.Fatalf("%d %v", st, out)
		}
	})

	t.Run("not-configured", func(t *testing.T) {
		e.api = e.apiWithCMI(t, nil)
		for _, c := range [][2]string{{"GET", "/v1/me/memory"}, {"DELETE", "/v1/me/memory"}, {"DELETE", "/v1/me/memory/" + bobRec1}} {
			if st, out := e.call(c[0], c[1], "bob", nil); st != 404 || code(out) != "NOT_CONFIGURED" {
				t.Fatalf("%s %s: %d %v", c[0], c[1], st, out)
			}
		}
	})
}
