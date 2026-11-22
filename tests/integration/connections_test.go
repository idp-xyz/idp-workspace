package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-workspace/adapters/identity"
	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/api"
	"idp-workspace/internal/authorization"
	"idp-workspace/internal/connections"
	"idp-workspace/internal/connections/connectionstest"
	"idp-workspace/internal/observability"
	"idp-workspace/internal/workspace"
)

// The connected services on PostgreSQL (ADR-0001 D30): the member's pending connect is a row of their own, one use, and the callback
// of anybody else's session finds nothing.

func TestConnectedServicesOnPostgres(t *testing.T) {
	e := newEnv(t)
	bridge := connectionstest.New(t)
	clock := &connectionstest.Clock{T: time.Now()}
	svc := &connections.Service{Bridge: bridge.Downstream(), PublicURL: "https://workspace.example", Pending: e.store, Now: clock.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit: func(ctx context.Context, p authorization.Principal, action, resource string, detail map[string]any) error {
			return e.store.InTenant(ctx, p.Tenant, func(tx pgx.Tx) error {
				return postgres.Audit(ctx, tx, p.Tenant, p.Subject, action, "", map[string]any{"connection": resource, "detail": detail})
			})
		}}
	verifier, _ := identity.NewDevVerifier(profile, []byte(devSecret))
	a := &api.API{Store: e.store, Auth: verifier, Connections: svc, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: observability.NewMetrics(), Version: "it"}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	main := e.api
	e.api = srv
	t.Cleanup(func() { e.api = main })
	// bob of the other tenant: the same subject name, another tenant
	tok, err := identity.DevToken([]byte(devSecret), profile, "bob", e.tenantB, []string{authorization.RoleMember}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e.tokens["bob-globex"] = tok

	const state = "it-state-0123456789abcdefghijklmnopqrstuvwxyz"
	bridge.Respond = func(c connectionstest.Call) (int, string) {
		switch {
		case strings.HasSuffix(c.Path, "/authorization"):
			return 200, `{"url":"https://github.example/authorize?state=` + state + `"}`
		case strings.HasSuffix(c.Path, "/callback"):
			return 200, `{"connection":"github","status":"connected"}`
		}
		return 204, ""
	}
	callbacks := func() int {
		n := 0
		for _, c := range bridge.Calls() {
			if strings.HasSuffix(c.Path, "/callback") {
				n++
			}
		}
		return n
	}
	e.must(200, "POST", "/v1/me/connections/github/authorization", "bob", map[string]any{"returnPath": "/connections/github"})
	cb := map[string]string{"code": "CODE", "state": state, "iss": "https://as.example", "error": ""}
	for _, who := range []string{"carol", "bob-globex", "alice"} { // another member, the same name in another tenant, an administrator
		out := e.must(400, "POST", "/v1/me/connections/callback", who, cb)
		if code(out) != connections.CodeStateInvalid {
			t.Errorf("%s: %v", who, out)
		}
	}
	if callbacks() != 0 {
		t.Fatal("the bridge received a callback of a session that did not start it")
	}
	out := e.must(200, "POST", "/v1/me/connections/callback", "bob", cb)
	if out["connection"] != "github" || out["returnPath"] != "/connections/github" {
		t.Fatalf("%v", out)
	}
	if code(e.must(400, "POST", "/v1/me/connections/callback", "bob", cb)) != connections.CodeStateInvalid || callbacks() != 1 {
		t.Fatalf("the second use reached the bridge (%d callbacks)", callbacks())
	}
	// the audit record has the connection and no code or state
	for _, ev := range e.must(200, "GET", "/api/v1/audit", "alice", nil)["events"].([]any) {
		if m := ev.(map[string]any); m["action"] == "connection.connect" {
			raw := strings.ToLower(fmt.Sprint(m))
			if !strings.Contains(raw, "github") || strings.Contains(raw, strings.ToLower(state)) {
				t.Errorf("audit: %s", raw)
			}
		}
	}

	// expired: the row is consumed and the bridge not asked
	e.must(200, "POST", "/v1/me/connections/github/authorization", "bob", nil)
	clock.Advance(10*time.Minute + time.Second)
	out = e.must(400, "POST", "/v1/me/connections/callback", "bob", cb)
	if code(out) != connections.CodeStateExpired || callbacks() != 1 {
		t.Fatalf("%v (%d callbacks)", out, callbacks())
	}
	if code(e.must(400, "POST", "/v1/me/connections/callback", "bob", cb)) != connections.CodeStateInvalid {
		t.Fatal("an expired state stayed")
	}

	ctx := context.Background()
	rec := func(h string) workspace.PendingConnect {
		return workspace.PendingConnect{StateHash: strings.Repeat(h, 64)[:64], Connection: "github", ReturnPath: "/connections", ExpiresAt: time.Now().Add(time.Hour)}
	}
	// the database keeps rows per tenant and subject: a take names both
	if err := e.store.PutPendingConnect(ctx, e.tenantA, "bob", rec("a")); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutPendingConnect(ctx, e.tenantA, "bob", rec("a")); err == nil {
		t.Error("the same state twice")
	}
	if got, _ := e.store.TakePendingConnect(ctx, e.tenantB, "bob", rec("a").StateHash); got != nil {
		t.Error("another tenant took it")
	}
	if got, _ := e.store.TakePendingConnect(ctx, e.tenantA, "carol", rec("a").StateHash); got != nil {
		t.Error("another subject took it")
	}
	if got, err := e.store.TakePendingConnect(ctx, e.tenantA, "bob", rec("a").StateHash); err != nil || got == nil || got.Connection != "github" || got.ReturnPath != "/connections" {
		t.Errorf("the owner: %+v %v", got, err)
	}
	if got, _ := e.store.TakePendingConnect(ctx, e.tenantA, "bob", rec("a").StateHash); got != nil {
		t.Error("taken twice")
	}
	// row-level security: a connection of the service role that names no tenant sees nothing
	if err := e.store.PutPendingConnect(ctx, e.tenantA, "bob", rec("b")); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.store.Pool.QueryRow(ctx, `SELECT count(*) FROM connection_pending`).Scan(&n); err != nil || n != 0 {
		t.Errorf("without a tenant the service role sees %d rows (%v)", n, err)
	}
	// the database refuses a return path that is not a path of the Workspace
	for _, bad := range []string{"//evil.example", "/\\evil.example", "https://evil.example", "evil", "", "/a\x01b", "/a\\b"} {
		r := rec("c")
		r.ReturnPath = bad
		if err := e.store.PutPendingConnect(ctx, e.tenantA, "bob", r); err == nil {
			t.Errorf("stored the return path %q", bad)
		}
	}
	// what has expired goes at the next start, and a member keeps at most MaxPendingConnects
	old := rec("d")
	old.ExpiresAt = time.Now().Add(-time.Minute)
	if err := e.store.PutPendingConnect(ctx, e.tenantA, "erin2", old); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutPendingConnect(ctx, e.tenantA, "erin2", rec("e")); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.store.TakePendingConnect(ctx, e.tenantA, "erin2", old.StateHash); got != nil {
		t.Error("an expired row was kept")
	}
	hash := func(i int) string { return fmt.Sprintf("%064x", i+1) }
	for i := 0; i < postgres.MaxPendingConnects+5; i++ {
		r := rec("f")
		r.StateHash = hash(i)
		if err := e.store.PutPendingConnect(ctx, e.tenantA, "dan", r); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // created_at orders them
	}
	if got, _ := e.store.TakePendingConnect(ctx, e.tenantA, "dan", hash(0)); got != nil {
		t.Error("the oldest connect was kept beyond the bound")
	}
	if got, _ := e.store.TakePendingConnect(ctx, e.tenantA, "dan", hash(postgres.MaxPendingConnects+4)); got == nil {
		t.Error("the newest was dropped")
	}
}
