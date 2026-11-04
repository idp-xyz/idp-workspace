// Package integration runs the workspace in process - API, store and dispatcher - against a real
// PostgreSQL, with fake idp-host, idp-eer and app backends. Set IDP_TEST_PG_OWNER_URL to a superuser
// URL of a scratch cluster (make test-integration).
package integration

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"

	"idp-workspace/adapters/identity"
	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/api"
	"idp-workspace/internal/apptoken"
	"idp-workspace/internal/authorization"
	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/observability"
	"idp-workspace/internal/runs"
)

func scratchDB(t *testing.T) (ownerURL, serviceURL string) {
	t.Helper()
	base := os.Getenv("IDP_TEST_PG_OWNER_URL")
	if base == "" {
		t.Skip("IDP_TEST_PG_OWNER_URL not set (make test-integration)")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "idp_ws_it_" + hex.EncodeToString(b)
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	ownerURL = u.String()
	if _, err := postgres.Migrate(ctx, ownerURL); err != nil {
		t.Fatal(err)
	}
	if err := postgres.EnsureLogin(ctx, ownerURL, "idp_ws_it_svc", "it-service-password"); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("idp_ws_it_svc", "it-service-password")
	return ownerURL, u.String()
}

// recorder is a fake downstream: it remembers every request and answers with the next scripted reply.
type recorder struct {
	mu      sync.Mutex
	got     []recorded
	status  int
	answer  string
	srv     *httptest.Server
	replies func(r recorded) (int, string)
}

type recorded struct {
	Path, Auth, Key, Signature string
	Body                       map[string]any
	Raw                        []byte
}

func newRecorder(t *testing.T, status int, answer string) *recorder {
	rec := &recorder{status: status, answer: answer}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		x := recorded{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Key: r.Header.Get("Idempotency-Key"), Signature: r.Header.Get("Idp-Signature"), Raw: raw}
		_ = json.Unmarshal(raw, &x.Body)
		rec.mu.Lock()
		rec.got = append(rec.got, x)
		status, answer := rec.status, rec.answer
		if rec.replies != nil {
			status, answer = rec.replies(x)
		}
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) last() recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.got) == 0 {
		return recorded{}
	}
	return r.got[len(r.got)-1]
}

// find returns the latest request whose path matches.
func (r *recorder) find(match func(path string) bool) recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.got) - 1; i >= 0; i-- {
		if match(r.got[i].Path) {
			return r.got[i]
		}
	}
	return recorded{}
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

type env struct {
	t       *testing.T
	store   *postgres.Store
	api     *httptest.Server
	tokens  map[string]string
	tenantA string
	tenantB string
	host    *recorder
	eer     *recorder
	hook    *recorder
	secret  []byte
}

var profile = identity.Profile{Issuer: "idp-workspace-it", Audience: "idp-workspace", TenantClaim: "tenant_id", RolesClaim: "roles"}

const devSecret = "integration-test-secret-at-least-32-bytes-long"

func newEnv(t *testing.T) *env {
	t.Helper()
	ownerURL, serviceURL := scratchDB(t)
	ctx := context.Background()
	tenantA, err := postgres.SeedTenant(ctx, ownerURL, "", "acme")
	if err != nil {
		t.Fatal(err)
	}
	tenantB, _ := postgres.SeedTenant(ctx, ownerURL, "", "globex")
	store, err := postgres.Open(ctx, serviceURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if _, err := postgres.Open(ctx, ownerURL); err == nil {
		t.Fatal("the superuser connection must be refused")
	}
	e := &env{t: t, store: store, tenantA: tenantA, tenantB: tenantB, secret: []byte("webhook-secret-of-at-least-32-bytes!!")}
	e.tokens = map[string]string{}
	for name, spec := range map[string]struct {
		tenant string
		roles  []string
	}{
		"alice":   {tenantA, []string{authorization.RoleAdmin}},
		"bob":     {tenantA, []string{authorization.RoleMember}},
		"carol":   {tenantA, []string{authorization.RoleMember}},
		"dan":     {tenantA, []string{authorization.RoleMember}}, // not a member of the workspace
		"svc-eer": {tenantA, []string{authorization.RoleProducer}},
		"erin":    {tenantB, []string{authorization.RoleAdmin}},
	} {
		tok, err := identity.DevToken([]byte(devSecret), profile, name, spec.tenant, spec.roles, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		e.tokens[name] = tok
	}
	e.host = newRecorder(t, 202, `{"runId":"x","threadId":"thr_1","status":"QUEUED"}`)
	e.host.replies = func(r recorded) (int, string) {
		if strings.HasSuffix(r.Path, "/events") { // D11: the run's event stream
			return 200, "data: {\"type\":\"RUN_STARTED\"}\n\n" +
				"data: {\"type\":\"TEXT_MESSAGE_START\",\"messageId\":\"m1\"}\n\n" +
				"data: {\"type\":\"TEXT_MESSAGE_CONTENT\",\"messageId\":\"m1\",\"delta\":\"Order 1001 waits for customs.\"}\n\n" +
				"data: {\"type\":\"TEXT_MESSAGE_END\",\"messageId\":\"m1\"}\n\n" +
				"data: {\"type\":\"RUN_FINISHED\"}\n\n"
		}
		return 202, fmt.Sprintf(`{"runId":%q,"threadId":"thr_1","status":"QUEUED"}`, r.Body["runId"])
	}
	e.eer = newRecorder(t, 202, `{"tenant_id":"eer-acme","execution_id":"execution:7f3a","decision":"approved","accepted":true}`)
	e.hook = newRecorder(t, 200, `{"accepted":true,"ticket":"T-7"}`)
	verifier, _ := identity.NewDevVerifier(profile, []byte(devSecret))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &dispatch.Dispatcher{
		Host:          &dispatch.Downstream{Endpoint: e.host.srv.URL, Auth: dispatch.AuthForward},
		EER:           &dispatch.Downstream{Endpoint: e.eer.srv.URL, Auth: dispatch.AuthStatic, Token: func() (string, error) { return "eer-service-token", nil }},
		EERTenant:     "eer-acme",
		WebhookSecret: e.secret,
	}
	web := t.TempDir()
	_ = os.MkdirAll(filepath.Join(web, "assets"), 0o755)
	_ = os.WriteFile(filepath.Join(web, "index.html"), []byte("<!doctype html><title>IDP Workspace</title>"), 0o644)
	_ = os.WriteFile(filepath.Join(web, "assets", "app-1234.js"), []byte("console.log(1)"), 0o644)
	signer, err := apptoken.Generate("https://workspace.it.example")
	if err != nil {
		t.Fatal(err)
	}
	a := &api.API{Store: store, Auth: verifier, Dispatcher: d, Log: log, Metrics: observability.NewMetrics(), Version: "it", WebDir: web, DevMode: true,
		AppTokens: signer, OIDC: &api.ShellOIDC{Issuer: "http://127.0.0.1:7080/realms/idp", ClientID: "workspace-shell", Scope: "openid"},
		Runs: &runs.Follower{Store: store, Endpoint: e.host.srv.URL, Log: log}}
	tp, _, err := observability.Setup("idp-workspace-it", "it", log)
	if err != nil {
		t.Fatal(err)
	}
	e.api = httptest.NewServer(observability.HTTPMiddleware(tp, "idp-workspace/it", a.Handler()))
	t.Cleanup(e.api.Close)
	return e
}

func (e *env) callH(method, path, who string, body any, headers ...string) (int, map[string]any, http.Header) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		var b []byte
		if raw, ok := body.(string); ok {
			b = []byte(raw)
		} else {
			b, _ = json.Marshal(body)
		}
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, e.api.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.tokens[who])
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			e.t.Fatalf("%s %s: %d non-JSON body %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, out, resp.Header
}

func (e *env) call(method, path, who string, body any, headers ...string) (int, map[string]any) {
	e.t.Helper()
	st, out, _ := e.callH(method, path, who, body, headers...)
	return st, out
}

func (e *env) must(status int, method, path, who string, body any, headers ...string) map[string]any {
	e.t.Helper()
	st, out := e.call(method, path, who, body, headers...)
	if st != status {
		e.t.Fatalf("%s %s as %s: got %d, want %d: %v", method, path, who, st, status, out)
	}
	return out
}

func code(out map[string]any) string {
	if er, ok := out["error"].(map[string]any); ok {
		return fmt.Sprint(er["code"])
	}
	return ""
}

const ordersManifest = `{
  "schemaVersion": "idp.workspace.app/v1", "name": "orders", "version": "1.2.0", "title": "Orders", "publisher": "IDP Parcel",
  "entry": {"url": "https://orders.apps.example/workspace"},
  "capabilities": ["context.read", "actions.invoke"],
  "actions": [
    {"name": "order.review", "title": "Ask the order assistant", "target": {"kind": "host.run", "agent": "order-assistant"},
     "input": {"type": "object", "required": ["message"], "properties": {"message": {"type": "string", "minLength": 1}}, "additionalProperties": false}},
    {"name": "approval.approve", "title": "Approve", "roles": ["approver"], "confirm": true, "target": {"kind": "eer.decision", "decision": "approve"},
     "input": {"type": "object", "properties": {"executionId": {"type": "string"}, "note": {"type": "string"}}, "additionalProperties": false}},
    {"name": "approval.reject", "title": "Reject", "roles": ["approver"], "target": {"kind": "eer.decision", "decision": "reject"},
     "input": {"type": "object", "properties": {"executionId": {"type": "string"}}, "additionalProperties": false}},
    {"name": "order.hold", "title": "Hold an order", "target": {"kind": "webhook", "url": "WEBHOOK"},
     "input": {"type": "object", "required": ["orderId", "reason"], "properties": {"orderId": {"type": "string", "pattern": "^[0-9]+$"}, "reason": {"type": "string"}}, "additionalProperties": false}}
  ],
  "config": {"schema": {"type": "object", "required": ["region"], "properties": {"region": {"enum": ["us", "eu"]}}, "additionalProperties": false}}
}`

func TestWorkspaceEndToEnd(t *testing.T) {
	e := newEnv(t)
	// webhook targets must be https except on loopback; the fake runs on 127.0.0.1
	manifest := strings.Replace(ordersManifest, "WEBHOOK", e.hook.srv.URL+"/hooks/hold", 1)
	var wsID, itemID string

	t.Run("workspaces-and-members", func(t *testing.T) {
		e.must(403, "POST", "/api/v1/workspaces", "bob", map[string]any{"name": "us-ops", "displayName": "US operations"})
		e.must(422, "POST", "/api/v1/workspaces", "alice", map[string]any{"name": "US Ops", "displayName": "x"})
		ws := e.must(201, "POST", "/api/v1/workspaces", "alice", map[string]any{"name": "us-ops", "displayName": "US operations"})
		wsID = ws["workspaceId"].(string)
		e.must(409, "POST", "/api/v1/workspaces", "alice", map[string]any{"name": "us-ops", "displayName": "again"})
		e.must(200, "PUT", "/api/v1/workspaces/us-ops/members/bob", "alice", map[string]any{"roles": []string{"approver"}})
		e.must(200, "PUT", "/api/v1/workspaces/us-ops/members/carol", "alice", map[string]any{"roles": []string{}})
		e.must(422, "PUT", "/api/v1/workspaces/us-ops/members/carol", "alice", map[string]any{"roles": []string{"Approver"}})
		if ms := e.must(200, "GET", "/api/v1/workspaces/us-ops/members", "alice", nil)["members"].([]any); len(ms) != 3 {
			t.Fatalf("members (alice as owner, bob, carol): %v", ms)
		}
		if ws := e.must(200, "GET", "/v1/workspaces", "bob", nil)["workspaces"].([]any); len(ws) != 1 {
			t.Fatalf("%v", ws)
		}
		if ws := e.must(200, "GET", "/v1/workspaces", "dan", nil)["workspaces"].([]any); len(ws) != 0 {
			t.Fatalf("non-member sees %v", ws)
		}
		e.must(404, "GET", "/v1/workspaces/us-ops", "dan", nil)
		e.must(404, "GET", "/v1/workspaces/"+wsID, "erin", nil) // another tenant: RLS
		if me := e.must(200, "GET", "/v1/workspaces/us-ops", "bob", nil); fmt.Sprint(me["roles"]) != "[approver]" {
			t.Fatalf("%v", me)
		}
	})

	t.Run("app-registry", func(t *testing.T) {
		rel := e.must(201, "POST", "/api/v1/apps", "alice", manifest)
		if rel["status"] != "published" || !strings.HasPrefix(rel["digest"].(string), "sha256:") {
			t.Fatalf("%v", rel)
		}
		// the same manifest again (other whitespace, other key order) is an idempotent retry
		var m map[string]any
		_ = json.Unmarshal([]byte(manifest), &m)
		if again := e.must(200, "POST", "/api/v1/apps", "alice", m); again["digest"] != rel["digest"] {
			t.Fatalf("digest changed: %v", again["digest"])
		}
		// releases are immutable
		changed := strings.Replace(manifest, `"title": "Orders"`, `"title": "Orders desk"`, 1)
		if st, out := e.call("POST", "/api/v1/apps", "alice", changed); st != 409 {
			t.Fatalf("%d %v", st, out)
		}
		for name, bad := range map[string]string{
			"unknown target":      strings.Replace(manifest, `"kind": "host.run"`, `"kind": "email"`, 1),
			"input not an object": strings.Replace(manifest, `"input": {"type": "object", "required": ["message"]`, `"input": {"type": "string", "required": ["message"]`, 1),
			"external $ref":       strings.Replace(manifest, `"reason": {"type": "string"}`, `"reason": {"$ref": "https://schemas.example/reason.json"}`, 1),
			"http entry":          strings.Replace(manifest, `https://orders.apps.example/workspace`, `http://orders.apps.example/workspace`, 1),
			"unknown capability":  strings.Replace(manifest, `"context.read"`, `"token.read"`, 1),
		} {
			if st, out := e.call("POST", "/api/v1/apps", "alice", strings.Replace(bad, `"version": "1.2.0"`, `"version": "9.9.9"`, 1)); st != 422 || code(out) != "INVALID_MANIFEST" {
				t.Errorf("%s: %d %v", name, st, out)
			}
		}
		e.must(403, "POST", "/api/v1/apps", "bob", manifest)
		if rs := e.must(200, "GET", "/api/v1/apps?name=orders", "alice", nil)["releases"].([]any); len(rs) != 1 {
			t.Fatalf("%v", rs)
		}
		if rs := e.must(200, "GET", "/api/v1/apps", "erin", nil)["releases"].([]any); len(rs) != 0 {
			t.Fatalf("other tenant sees releases: %v", rs)
		}
	})

	t.Run("installations", func(t *testing.T) {
		if st, out := e.call("PUT", "/api/v1/workspaces/us-ops/installations/orders", "alice", map[string]any{"version": "1.2.0", "config": map[string]any{"region": "mars"}}); st != 422 || code(out) != "INVALID_CONFIG" {
			t.Fatalf("%d %v", st, out)
		}
		e.must(404, "PUT", "/api/v1/workspaces/us-ops/installations/orders", "alice", map[string]any{"version": "7.0.0", "config": map[string]any{"region": "us"}})
		in := e.must(201, "PUT", "/api/v1/workspaces/us-ops/installations/orders", "alice", map[string]any{"version": "1.2.0", "config": map[string]any{"region": "us"}})
		if in["revision"] != 1.0 || in["releaseStatus"] != "published" {
			t.Fatalf("%v", in)
		}
		e.must(409, "PUT", "/api/v1/workspaces/us-ops/installations/orders", "alice", map[string]any{"version": "1.2.0", "config": map[string]any{"region": "eu"}}, "If-Match", "7")
		if in := e.must(200, "PUT", "/api/v1/workspaces/us-ops/installations/orders", "alice", map[string]any{"version": "1.2.0", "config": map[string]any{"region": "eu"}}, "If-Match", "1"); in["revision"] != 2.0 {
			t.Fatalf("%v", in)
		}
		apps := e.must(200, "GET", "/v1/workspaces/us-ops/apps", "carol", nil)["apps"].([]any)
		if len(apps) != 1 || apps[0].(map[string]any)["origin"] != "https://orders.apps.example" {
			t.Fatalf("%v", apps)
		}
		e.must(404, "GET", "/v1/workspaces/us-ops/apps", "dan", nil)
		// the administrator's list keeps what the member's list hides (revoked releases) - checked after revocation below
		ins := e.must(200, "GET", "/api/v1/workspaces/us-ops/installations", "alice", nil)["installations"].([]any)
		if len(ins) != 1 || ins[0].(map[string]any)["app"] != "orders" {
			t.Fatalf("%v", ins)
		}
		e.must(403, "GET", "/api/v1/workspaces/us-ops/installations", "bob", nil)
		if me := e.must(200, "GET", "/v1/me", "bob", nil); me["subject"] != "bob" || fmt.Sprint(me["roles"]) != "[ws-member]" {
			t.Fatalf("%v", me)
		}
	})

	t.Run("inbox-is-a-view", func(t *testing.T) {
		item := map[string]any{"externalRef": "execution:7f3a/approval", "kind": "approval", "title": "Approve the refund for order 1001",
			"ref": map[string]any{"system": "eer", "kind": "execution", "id": "execution:7f3a"}, "assignees": map[string]any{"roles": []string{"approver"}},
			"priority": "high", "entities": []map[string]string{{"type": "order", "id": "1001"}},
			"actions": []map[string]any{{"app": "orders", "action": "approval.approve", "label": "Approve"}, {"app": "orders", "action": "approval.reject"}}}
		e.must(403, "POST", "/v1/workspaces/us-ops/inbox/items", "bob", item) // members do not create items
		it := e.must(201, "POST", "/v1/workspaces/us-ops/inbox/items", "svc-eer", item)
		itemID = it["itemId"].(string)
		item["summary"] = "Refund of 420.00 USD exceeds the agent's limit."
		if again := e.must(200, "POST", "/v1/workspaces/us-ops/inbox/items", "svc-eer", item); again["itemId"] != itemID || again["version"] != 2.0 {
			t.Fatalf("upsert by externalRef: %v", again)
		}
		bad := map[string]any{"externalRef": "x", "kind": "approval", "title": "t", "ref": map[string]any{"system": "erp", "kind": "k", "id": "1"}}
		e.must(422, "POST", "/v1/workspaces/us-ops/inbox/items", "svc-eer", bad)
		// a notification for everybody
		e.must(201, "POST", "/v1/workspaces/us-ops/inbox/items", "svc-eer", map[string]any{"externalRef": "notice:1", "kind": "notification", "title": "Carrier delay in Ohio",
			"ref": map[string]any{"system": "external", "kind": "notice", "id": "1"}})
		if items := e.must(200, "GET", "/v1/workspaces/us-ops/inbox", "bob", nil)["items"].([]any); len(items) != 2 || items[0].(map[string]any)["itemId"] != itemID {
			t.Fatalf("bob (approver) sees the approval first: %v", items)
		}
		if items := e.must(200, "GET", "/v1/workspaces/us-ops/inbox", "carol", nil)["items"].([]any); len(items) != 1 {
			t.Fatalf("carol sees only the notification: %v", items)
		}
		e.must(404, "GET", "/v1/workspaces/us-ops/inbox/"+itemID, "carol", nil)
		// marks are the member's own
		marked := e.must(200, "POST", "/v1/workspaces/us-ops/inbox/"+itemID+"/marks", "bob", map[string]any{"read": true})
		if marked["readAt"] == nil || marked["status"] != "open" {
			t.Fatalf("%v", marked)
		}
		notice := e.must(200, "GET", "/v1/workspaces/us-ops/inbox?kind=notification", "carol", nil)["items"].([]any)[0].(map[string]any)["itemId"].(string)
		e.must(200, "POST", "/v1/workspaces/us-ops/inbox/"+notice+"/marks", "carol", map[string]any{"archived": true})
		if items := e.must(200, "GET", "/v1/workspaces/us-ops/inbox", "carol", nil)["items"].([]any); len(items) != 0 {
			t.Fatalf("archived item still listed: %v", items)
		}
		if items := e.must(200, "GET", "/v1/workspaces/us-ops/inbox", "bob", nil)["items"].([]any); len(items) != 2 {
			t.Fatalf("carol's archive changed bob's inbox: %v", items)
		}
		if items := e.must(200, "GET", "/v1/workspaces/us-ops/inbox/produced", "svc-eer", nil)["items"].([]any); len(items) != 2 {
			t.Fatalf("%v", items)
		}
	})

	t.Run("work-context", func(t *testing.T) {
		if c := e.must(200, "GET", "/v1/workspaces/us-ops/context", "bob", nil); c["version"] != 0.0 {
			t.Fatalf("%v", c)
		}
		body := map[string]any{"entities": []map[string]string{{"type": "order", "id": "1001"}}, "app": "orders", "thread": "thr_ops_42", "itemId": itemID}
		if c := e.must(200, "PUT", "/v1/workspaces/us-ops/context", "bob", body, "If-Match", "0"); c["version"] != 1.0 {
			t.Fatalf("%v", c)
		}
		e.must(409, "PUT", "/v1/workspaces/us-ops/context", "bob", body, "If-Match", "0")
		e.must(404, "PUT", "/v1/workspaces/us-ops/context", "carol", body) // carol cannot see the item
		e.must(422, "PUT", "/v1/workspaces/us-ops/context", "carol", map[string]any{"entities": []map[string]string{{"type": "Order", "id": "1"}}})
	})

	t.Run("actions-go-to-the-owner", func(t *testing.T) {
		invoke := func(who string, body map[string]any, headers ...string) (int, map[string]any, http.Header) {
			return e.callH("POST", "/v1/workspaces/us-ops/actions/invoke", who, body, headers...)
		}
		// carol lacks the approver role of the contract; administrators are not exempt either
		if st, out, _ := invoke("carol", map[string]any{"app": "orders", "action": "approval.approve", "input": map[string]any{"executionId": "execution:7f3a"}}); st != 403 {
			t.Fatalf("%d %v", st, out)
		}
		if st, out, _ := invoke("alice", map[string]any{"app": "orders", "action": "approval.approve", "input": map[string]any{"executionId": "execution:7f3a"}}); st != 403 {
			t.Fatalf("admin bypassed the contract's roles: %d %v", st, out)
		}
		// bob approves from the item: idp-eer receives the decision for the item's execution
		st, out, _ := invoke("bob", map[string]any{"app": "orders", "action": "approval.approve", "itemId": itemID, "input": map[string]any{"note": "ok"}}, "Idempotency-Key", "k-approve-1")
		if st != 201 || out["status"] != "succeeded" {
			t.Fatalf("%d %v", st, out)
		}
		got := e.eer.last()
		if got.Path != "/v1/executions/execution:7f3a/approval" || got.Body["decision"] != "approved" || got.Body["actor_id"] != "bob" || got.Body["tenant_id"] != "eer-acme" || got.Auth != "Bearer eer-service-token" {
			t.Fatalf("%+v", got)
		}
		// the inbox does not decide: the item stays open until the producer closes it
		if it := e.must(200, "GET", "/v1/workspaces/us-ops/inbox/"+itemID, "bob", nil); it["status"] != "open" {
			t.Fatalf("%v", it)
		}
		// a retry with the same key replays; another body under that key is a conflict
		calls := e.eer.count()
		st, again, h := invoke("bob", map[string]any{"app": "orders", "action": "approval.approve", "itemId": itemID, "input": map[string]any{"note": "ok"}}, "Idempotency-Key", "k-approve-1")
		if st != 200 || h.Get("Idempotent-Replayed") != "true" || again["invocationId"] != out["invocationId"] || e.eer.count() != calls {
			t.Fatalf("%d %v %v", st, again, h)
		}
		if st, _, _ := invoke("bob", map[string]any{"app": "orders", "action": "approval.reject", "itemId": itemID}, "Idempotency-Key", "k-approve-1"); st != 409 {
			t.Fatalf("%d", st)
		}
		// an action the item does not offer
		if st, out, _ := invoke("bob", map[string]any{"app": "orders", "action": "order.review", "itemId": itemID, "input": map[string]any{"message": "x"}}); st != 409 || code(out) != "ACTION_NOT_OFFERED" {
			t.Fatalf("%d %v", st, out)
		}
		// the producer closes the item; further decisions from it are refused
		e.must(403, "POST", "/v1/workspaces/us-ops/inbox/close", "bob", map[string]any{"itemId": itemID, "status": "resolved", "outcome": "approved"})
		closed := e.must(200, "POST", "/v1/workspaces/us-ops/inbox/close", "svc-eer", map[string]any{"externalRef": "execution:7f3a/approval", "status": "resolved", "outcome": "approved", "note": "approved by bob"})
		if closed["status"] != "resolved" || closed["resolution"].(map[string]any)["outcome"] != "approved" {
			t.Fatalf("%v", closed)
		}
		e.must(409, "POST", "/v1/workspaces/us-ops/inbox/close", "svc-eer", map[string]any{"itemId": itemID, "status": "withdrawn"})
		e.must(409, "POST", "/v1/workspaces/us-ops/inbox/items", "svc-eer", map[string]any{"externalRef": "execution:7f3a/approval", "kind": "approval", "title": "again",
			"ref": map[string]any{"system": "eer", "kind": "execution", "id": "execution:7f3a"}})
		if st, out, _ := invoke("bob", map[string]any{"app": "orders", "action": "approval.reject", "itemId": itemID}); st != 409 || code(out) != "ITEM_CLOSED" {
			t.Fatalf("%d %v", st, out)
		}
		// idp-eer refuses (not waiting for approval): recorded as failed, its code kept
		e.eer.status, e.eer.answer = 409, `{"error":{"code":"approval_state_conflict","message":"not waiting"}}`
		st, out, _ = invoke("bob", map[string]any{"app": "orders", "action": "approval.reject", "input": map[string]any{"executionId": "execution:other"}})
		if st != 409 || code(out) != "TARGET_REJECTED" || out["invocation"].(map[string]any)["status"] != "failed" || !strings.Contains(fmt.Sprint(out), "approval_state_conflict") {
			t.Fatalf("%d %v", st, out)
		}
		// host.run with the caller's token and the work context's entities
		st, out, _ = invoke("bob", map[string]any{"app": "orders", "action": "order.review", "input": map[string]any{"message": "Why is order 1001 late?"}})
		if st != 201 {
			t.Fatalf("%d %v", st, out)
		}
		run := e.host.find(func(p string) bool { return strings.HasSuffix(p, "/runs") }) // the follower may already be reading events
		if run.Path != "/v1/agents/order-assistant/runs" || run.Auth != "Bearer "+e.tokens["bob"] || run.Body["threadId"] != "thr_ops_42" || !strings.Contains(string(run.Raw), `\"type\":\"order\"`) {
			t.Fatalf("%+v %s", run, run.Raw)
		}
		if res := out["result"].(map[string]any); res["system"] != "host" || res["id"] != run.Body["runId"] {
			t.Fatalf("%v", res)
		}
		// D11: the run's result comes back into bob's inbox (only bob's)
		runID := run.Body["runId"].(string)
		var result map[string]any
		for i := 0; i < 100 && result == nil; i++ {
			for _, it := range e.must(200, "GET", "/v1/workspaces/us-ops/inbox?kind=run", "bob", nil)["items"].([]any) {
				if x := it.(map[string]any); x["externalRef"] == "run:"+runID && strings.HasSuffix(x["title"].(string), "finished") {
					result = x
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		if result == nil || result["summary"] != "Order 1001 waits for customs." || result["producer"] != "idp-workspace" || result["ref"].(map[string]any)["id"] != runID {
			t.Fatalf("run result: %v", result)
		}
		if got := e.host.find(func(p string) bool { return strings.HasSuffix(p, "/events") }); got.Path != "/v1/runs/"+runID+"/events" || got.Auth != "Bearer "+e.tokens["bob"] {
			t.Fatalf("followed with %+v", got)
		}
		if items := e.must(200, "GET", "/v1/workspaces/us-ops/inbox?kind=run", "carol", nil)["items"].([]any); len(items) != 0 {
			t.Fatalf("carol sees bob's run: %v", items)
		}
		// input schema
		if st, out, _ := invoke("bob", map[string]any{"app": "orders", "action": "order.hold", "input": map[string]any{"orderId": "A-1"}}); st != 422 || code(out) != "INVALID_INPUT" {
			t.Fatalf("%d %v", st, out)
		}
		// webhook: signed, never carries the user's token
		st, out, _ = invoke("carol", map[string]any{"app": "orders", "action": "order.hold", "input": map[string]any{"orderId": "1001", "reason": "address check"}})
		if st != 201 {
			t.Fatalf("%d %v", st, out)
		}
		hook := e.hook.last()
		if hook.Auth != "" || dispatch.Verify(e.secret, hook.Signature, hook.Raw, time.Now(), time.Minute) != nil || hook.Body["subject"] != "carol" {
			t.Fatalf("%+v", hook)
		}
		if !strings.Contains(fmt.Sprint(out["result"]), "T-7") {
			t.Fatalf("%v", out)
		}
		// unknown action, app not installed
		if st, out, _ := invoke("bob", map[string]any{"app": "orders", "action": "order.delete"}); st != 404 || code(out) != "ACTION_NOT_FOUND" {
			t.Fatalf("%d %v", st, out)
		}
		if st, out, _ := invoke("bob", map[string]any{"app": "billing", "action": "x"}); st != 404 || code(out) != "APP_NOT_INSTALLED" {
			t.Fatalf("%d %v", st, out)
		}
		e.must(404, "POST", "/v1/workspaces/us-ops/actions/invoke", "dan", map[string]any{"app": "orders", "action": "order.review", "input": map[string]any{"message": "x"}})
		// the record: bob's own invocations, all of them for the administrator
		if mine := e.must(200, "GET", "/v1/workspaces/us-ops/invocations", "bob", nil)["invocations"].([]any); len(mine) != 3 {
			t.Fatalf("bob: %v", mine)
		}
		if all := e.must(200, "GET", "/api/v1/workspaces/us-ops/invocations", "alice", nil)["invocations"].([]any); len(all) != 4 {
			t.Fatalf("all: %v", all)
		}
	})

	t.Run("inbox-stream", func(t *testing.T) {
		// D10: the stream sends what bob sees, then live changes; carol's stream does not get bob's items
		api.StreamPoll = 50 * time.Millisecond
		open := func(who, after string) (*http.Response, *bufio.Reader) {
			req, _ := http.NewRequest("GET", e.api.URL+"/v1/workspaces/us-ops/inbox/stream?"+after, nil)
			req.Header.Set("Authorization", "Bearer "+e.tokens[who])
			resp, err := http.DefaultClient.Do(req)
			if err != nil || resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("%v %v", resp, err)
			}
			return resp, bufio.NewReader(resp.Body)
		}
		next := func(rd *bufio.Reader) (string, map[string]any) {
			var id string
			for {
				line, err := rd.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				line = strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(line, "id: "):
					id = strings.TrimPrefix(line, "id: ")
				case strings.HasPrefix(line, "data: "):
					var it map[string]any
					_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &it)
					return id, it
				}
			}
		}
		resp, rd := open("bob", "replay=open")
		defer resp.Body.Close()
		seen := map[string]bool{}
		var lastID string
		deadline := time.Now().Add(5 * time.Second)
		e.must(201, "POST", "/v1/workspaces/us-ops/inbox/items", "svc-eer", map[string]any{"externalRef": "stream:1", "kind": "task", "title": "Check the Ohio lane",
			"ref": map[string]any{"system": "external", "kind": "task", "id": "s1"}, "assignees": map[string]any{"subjects": []string{"bob"}}})
		for !seen["stream:1"] && time.Now().Before(deadline) {
			id, it := next(rd)
			seen[it["externalRef"].(string)], lastID = true, id
		}
		if !seen["stream:1"] || !seen["notice:1"] {
			t.Fatalf("stream: %v", seen)
		}
		// resume after the last event: only later changes arrive
		resp.Body.Close()
		resp2, rd2 := open("bob", "after="+url.QueryEscape(lastID))
		defer resp2.Body.Close()
		e.must(201, "POST", "/v1/workspaces/us-ops/inbox/items", "svc-eer", map[string]any{"externalRef": "stream:2", "kind": "task", "title": "Second",
			"ref": map[string]any{"system": "external", "kind": "task", "id": "s2"}, "assignees": map[string]any{"subjects": []string{"bob"}}})
		for {
			_, it := next(rd2)
			if it["externalRef"] == "stream:2" {
				break
			}
			if it["externalRef"] == "notice:1" {
				t.Fatal("resumed stream replayed an old item")
			}
		}
		// carol's stream never carries bob's task
		resp3, rd3 := open("carol", "replay=open")
		defer resp3.Body.Close()
		e.must(201, "POST", "/v1/workspaces/us-ops/inbox/items", "svc-eer", map[string]any{"externalRef": "stream:3", "kind": "notification", "title": "Everyone",
			"ref": map[string]any{"system": "external", "kind": "n", "id": "s3"}})
		for {
			_, it := next(rd3)
			if strings.HasPrefix(it["externalRef"].(string), "stream:1") || it["externalRef"] == "stream:2" {
				t.Fatalf("carol got %v", it)
			}
			if it["externalRef"] == "stream:3" {
				break
			}
		}
		e.must(404, "GET", "/v1/workspaces/us-ops/inbox/stream", "dan", nil)
	})

	t.Run("web-shell", func(t *testing.T) {
		get := func(path string) (*http.Response, string) {
			resp, err := http.Get(e.api.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp, string(b)
		}
		resp, body := get("/w/us-ops/inbox") // a client-side route: index.html, no token needed
		if resp.StatusCode != 200 || !strings.Contains(body, "IDP Workspace") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
		}
		if resp, _ := get("/assets/app-1234.js"); resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
			t.Fatalf("%v", resp.Header)
		}
		if resp, body := get("/../../etc/passwd"); resp.StatusCode != 200 || !strings.Contains(body, "IDP Workspace") {
			t.Fatalf("path traversal: %d %s", resp.StatusCode, body)
		}
		if resp, body := get("/shell-config.json"); resp.StatusCode != 200 || !strings.Contains(body, `"devMode":true`) {
			t.Fatalf("%d %s", resp.StatusCode, body)
		}
		if resp, _ := get("/v1/workspaces"); resp.StatusCode != 401 {
			t.Fatalf("the API stays authenticated: %d", resp.StatusCode)
		}
		if resp, _ := get("/api/v1/unknown"); resp.StatusCode != 401 {
			t.Fatalf("%d", resp.StatusCode)
		}
	})

	t.Run("app-tokens", func(t *testing.T) {
		// D15: an app that requested identity.token gets a short-lived token for its own backend
		desk := `{"schemaVersion":"idp.workspace.app/v1","name":"desk","version":"1.0.0","title":"Desk","publisher":"IDP","entry":{"url":"https://desk.apps.example/"},"capabilities":["identity.token"],"actions":[]}`
		e.must(201, "POST", "/api/v1/apps", "alice", desk)
		e.must(201, "PUT", "/api/v1/workspaces/us-ops/installations/desk", "alice", map[string]any{"version": "1.0.0"})
		out := e.must(201, "POST", "/v1/workspaces/us-ops/apps/desk/token", "bob", nil)
		if out["audience"] != "idp-workspace-app:desk" || out["token"] == nil {
			t.Fatalf("%v", out)
		}
		// verified with nothing but the published JWKS
		resp, err := http.Get(e.api.URL + "/.well-known/jwks.json")
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%v %v", resp, err)
		}
		var set struct {
			Keys []struct{ Kid, X, Y string }
		}
		_ = json.NewDecoder(resp.Body).Decode(&set)
		resp.Body.Close()
		x, _ := base64.RawURLEncoding.DecodeString(set.Keys[0].X)
		y, _ := base64.RawURLEncoding.DecodeString(set.Keys[0].Y)
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		claims := jwt.MapClaims{}
		if _, err := jwt.ParseWithClaims(out["token"].(string), claims, func(*jwt.Token) (any, error) { return pub, nil },
			jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer("https://workspace.it.example"), jwt.WithAudience("idp-workspace-app:desk")); err != nil {
			t.Fatal(err)
		}
		if claims["sub"] != "bob" || claims["tenant_id"] != e.tenantA || fmt.Sprint(claims["roles"]) != "[approver]" || claims["workspace"].(map[string]any)["name"] != "us-ops" {
			t.Fatalf("%v", claims)
		}
		if strings.Contains(out["token"].(string), e.tokens["bob"]) {
			t.Fatal("the user's token leaked into the app token")
		}
		// an app that did not ask, an app not installed, a non-member
		if st, out := e.call("POST", "/v1/workspaces/us-ops/apps/orders/token", "bob", nil); st != 403 || code(out) != "CAPABILITY_DENIED" {
			t.Fatalf("%d %v", st, out)
		}
		e.must(404, "POST", "/v1/workspaces/us-ops/apps/billing/token", "bob", nil)
		e.must(404, "POST", "/v1/workspaces/us-ops/apps/desk/token", "dan", nil)
		e.must(403, "POST", "/v1/workspaces/us-ops/apps/desk/token", "svc-eer", nil)
		// the shell is told where to sign in, and its CSP lets it reach the issuer
		resp, _ = http.Get(e.api.URL + "/shell-config.json")
		var cfg map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&cfg)
		resp.Body.Close()
		if cfg["oidc"].(map[string]any)["clientId"] != "workspace-shell" {
			t.Fatalf("%v", cfg)
		}
		resp, _ = http.Get(e.api.URL + "/")
		resp.Body.Close()
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "connect-src 'self' http://127.0.0.1:7080;") {
			t.Fatalf("%s", resp.Header.Get("Content-Security-Policy"))
		}
		e.must(204, "DELETE", "/api/v1/workspaces/us-ops/installations/desk", "alice", nil)
	})

	t.Run("revoked-and-uninstalled", func(t *testing.T) {
		e.must(422, "POST", "/api/v1/apps/orders/1.2.0/revoke", "alice", map[string]any{"reason": ""})
		rev := e.must(200, "POST", "/api/v1/apps/orders/1.2.0/revoke", "alice", map[string]any{"reason": "the hold webhook skips the address check"})
		if rev["status"] != "revoked" || rev["revocation"] == nil {
			t.Fatalf("%v", rev)
		}
		e.must(409, "POST", "/api/v1/apps/orders/1.2.0/revoke", "alice", map[string]any{"reason": "again"})
		if apps := e.must(200, "GET", "/v1/workspaces/us-ops/apps", "bob", nil)["apps"].([]any); len(apps) != 0 {
			t.Fatalf("a revoked release is still offered: %v", apps)
		}
		if ins := e.must(200, "GET", "/api/v1/workspaces/us-ops/installations", "alice", nil)["installations"].([]any); len(ins) != 1 || ins[0].(map[string]any)["releaseStatus"] != "revoked" {
			t.Fatalf("the administrator must still see the revoked installation: %v", ins)
		}
		if st, out := e.call("POST", "/v1/workspaces/us-ops/actions/invoke", "bob", map[string]any{"app": "orders", "action": "order.review", "input": map[string]any{"message": "x"}}); st != 409 || code(out) != "APP_REVOKED" {
			t.Fatalf("%d %v", st, out)
		}
		e.must(409, "PUT", "/api/v1/workspaces/us-ops/installations/orders", "alice", map[string]any{"version": "1.2.0", "config": map[string]any{"region": "us"}})
		e.must(204, "DELETE", "/api/v1/workspaces/us-ops/installations/orders", "alice", nil)
		e.must(404, "DELETE", "/api/v1/workspaces/us-ops/installations/orders", "alice", nil)
	})

	t.Run("audit", func(t *testing.T) {
		seen := map[string]bool{}
		for _, ev := range e.must(200, "GET", "/api/v1/audit", "alice", nil)["events"].([]any) {
			seen[ev.(map[string]any)["action"].(string)] = true
		}
		for _, want := range []string{"workspace.create", "member.put", "app.publish", "app.install", "app.revoke", "app.uninstall"} {
			if !seen[want] {
				t.Errorf("no %s audit event", want)
			}
		}
		if ev := e.must(200, "GET", "/api/v1/audit", "erin", nil)["events"].([]any); len(ev) != 0 {
			t.Fatalf("other tenant sees audit: %v", ev)
		}
	})
}
