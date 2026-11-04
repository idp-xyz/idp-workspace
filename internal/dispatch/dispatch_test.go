package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"idp-workspace/internal/authorization"
	"idp-workspace/internal/workspace"
)

type seen struct {
	path, auth, key string
	body            map[string]any
	sig             string
	raw             []byte
}

func fake(t *testing.T, status int, answer string, got *seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.raw, _ = io.ReadAll(r.Body)
		got.path, got.auth, got.key, got.sig = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.Header.Get("Idp-Signature")
		_ = json.Unmarshal(got.raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func req(target workspace.Target, input string) Request {
	return Request{InvocationID: "11111111-2222-4333-8444-555555555555", Principal: authorization.Principal{Subject: "alice", Tenant: "t1", Token: "user-token"},
		Workspace: workspace.Workspace{ID: "w1", Name: "ops"}, App: "orders", AppVersion: "1.0.0",
		Action: workspace.ActionContract{Name: "order.review", Title: "Review", Target: target}, Input: json.RawMessage(input)}
}

func TestHostRun(t *testing.T) {
	var got seen
	srv := fake(t, 202, `{"runId":"ws_11111111222243338444555555555555","threadId":"thr_1","status":"QUEUED"}`, &got)
	d := &Dispatcher{Host: &Downstream{Endpoint: srv.URL, Auth: AuthForward}}
	r := req(workspace.Target{Kind: workspace.TargetHostRun, Agent: "order-assistant"}, `{"message":"Why is order 1001 late?"}`)
	r.Context = &workspace.WorkContext{Entities: []workspace.Entity{{Type: "order", ID: "1001"}}, Thread: "thr_1"}
	res, f := d.Dispatch(context.Background(), r)
	if f != nil {
		t.Fatal(f)
	}
	if got.path != "/v1/agents/order-assistant/runs" || got.auth != "Bearer user-token" || got.key != "ws_11111111222243338444555555555555" {
		t.Fatalf("%+v", got)
	}
	msgs := got.body["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "Why is order 1001 late?" || got.body["threadId"] != "thr_1" {
		t.Fatalf("%v", got.body)
	}
	if !strings.Contains(string(got.raw), `idp.workspace/entities`) || !strings.Contains(string(res), `"id":"ws_1111`) {
		t.Fatalf("%s %s", got.raw, res)
	}
	// static credentials replace the caller's token
	d.Host.Auth, d.Host.Token = AuthStatic, func() (string, error) { return "svc", nil }
	if _, f := d.Dispatch(context.Background(), r); f != nil || got.auth != "Bearer svc" {
		t.Fatalf("%v %s", f, got.auth)
	}
}

func TestEERDecision(t *testing.T) {
	var got seen
	srv := fake(t, 202, `{"tenant_id":"eer-t","execution_id":"execution:9","decision":"approved","accepted":true}`, &got)
	d := &Dispatcher{EER: &Downstream{Endpoint: srv.URL, Auth: AuthForward}, EERTenant: "eer-t"}
	r := req(workspace.Target{Kind: workspace.TargetEERDecision, Decision: "approve"}, `{}`)
	r.Item = &workspace.InboxItem{ID: "i1", Ref: workspace.Ref{System: "eer", Kind: "execution", ID: "execution:9"}}
	res, f := d.Dispatch(context.Background(), r)
	if f != nil {
		t.Fatal(f)
	}
	if got.path != "/v1/executions/execution:9/approval" || got.body["decision"] != "approved" || got.body["actor_id"] != "alice" || got.body["tenant_id"] != "eer-t" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(string(res), `"accepted":true`) {
		t.Fatalf("%s", res)
	}
	// an input naming another execution than the item's is refused before anything is sent
	r.Input = json.RawMessage(`{"executionId":"execution:other"}`)
	if _, f := d.Dispatch(context.Background(), r); f == nil || f.Code != "INVALID_INPUT" {
		t.Fatalf("%v", f)
	}
	// no item and no executionId
	r.Item, r.Input = nil, json.RawMessage(`{}`)
	if _, f := d.Dispatch(context.Background(), r); f == nil || f.Code != "INVALID_INPUT" {
		t.Fatalf("%v", f)
	}
	// EER's own refusal keeps its code
	conflict := fake(t, 409, `{"error":{"code":"approval_state_conflict","message":"not waiting"}}`, &got)
	d.EER.Endpoint = conflict.URL
	r.Input = json.RawMessage(`{"executionId":"execution:9"}`)
	_, f = d.Dispatch(context.Background(), r)
	if f == nil || f.Code != "TARGET_REJECTED" || f.Status != 409 || !strings.Contains(f.Message, "approval_state_conflict") {
		t.Fatalf("%v", f)
	}
	// unconfigured
	if _, f := (&Dispatcher{}).Dispatch(context.Background(), r); f == nil || f.Code != "TARGET_UNAVAILABLE" {
		t.Fatalf("%v", f)
	}
}

func TestWebhookSigned(t *testing.T) {
	var got seen
	srv := fake(t, 200, `{"accepted":true,"ticket":"T-7"}`, &got)
	now := time.Unix(1_790_000_000, 0)
	secret := []byte("0123456789abcdef0123456789abcdef")
	d := &Dispatcher{WebhookSecret: secret, WebhookHTTP: srv.Client(), Now: func() time.Time { return now }}
	res, f := d.Dispatch(context.Background(), req(workspace.Target{Kind: workspace.TargetWebhook, URL: srv.URL + "/hooks/hold"}, `{"orderId":"1001"}`))
	if f != nil {
		t.Fatal(f)
	}
	if got.path != "/hooks/hold" || got.auth != "" {
		t.Fatalf("%+v (the user's token must never reach an app)", got)
	}
	if err := Verify(secret, got.sig, got.raw, now.Add(30*time.Second), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if Verify(secret, got.sig, append(got.raw, ' '), now, 5*time.Minute) == nil {
		t.Fatal("tampered body verified")
	}
	if Verify(secret, got.sig, got.raw, now.Add(time.Hour), 5*time.Minute) == nil {
		t.Fatal("stale signature verified")
	}
	if !strings.Contains(string(res), `"ticket":"T-7"`) {
		t.Fatalf("%s", res)
	}
	bad := fake(t, 500, `oops`, &got)
	if _, f := d.Dispatch(context.Background(), req(workspace.Target{Kind: workspace.TargetWebhook, URL: bad.URL}, `{}`)); f == nil || f.Code != "TARGET_REJECTED" || f.Status != 500 {
		t.Fatalf("%v", f)
	}
}

// TestSignatureVector pins the signature to the same vector the TypeScript SDK checks
// (packages/bridge/test/bridge.test.ts), so both sides agree byte for byte.
func TestSignatureVector(t *testing.T) {
	got := Sign([]byte("webhook-secret-of-at-least-32-bytes!!"), "1790000000", []byte(`{"invocationId":"x","input":{"orderId":"1001"}}`))
	if got != "3a706ee8ac4b2e1d5c86db84aaac52f0ee5f6810586931f2574e621b071f27c1" {
		t.Fatal(got)
	}
}

func TestExchange(t *testing.T) {
	calls := 0
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || r.Form.Get("client_id") != "idp-workspace" || r.Form.Get("client_secret") != "s3cret" || r.Form.Get("audience") != "idp-host" {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"invalid_request"}`)
			return
		}
		if r.Form.Get("subject_token") == "revoked" {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"invalid_token"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"host-token-for-`+r.Form.Get("subject_token")+`","expires_in":300}`)
	}))
	defer idp.Close()
	now := time.Unix(1_790_000_000, 0)
	x := &Exchanger{TokenURL: idp.URL, ClientID: "idp-workspace", ClientSecret: func() (string, error) { return "s3cret", nil }, Audience: "idp-host", Now: func() time.Time { return now }}
	var got seen
	host := fake(t, 202, `{"runId":"r","status":"QUEUED"}`, &got)
	d := &Dispatcher{Host: &Downstream{Endpoint: host.URL, Auth: AuthExchange, Exchange: x}}
	r := req(workspace.Target{Kind: workspace.TargetHostRun, Agent: "a"}, `{"message":"hi"}`)
	if _, f := d.Dispatch(context.Background(), r); f != nil || got.auth != "Bearer host-token-for-user-token" {
		t.Fatalf("%v %s", f, got.auth)
	}
	// cached until 30 s before expiry, then exchanged again
	_, _ = d.Dispatch(context.Background(), r)
	if calls != 1 {
		t.Fatalf("exchanged %d times", calls)
	}
	now = now.Add(271 * time.Second)
	_, _ = d.Dispatch(context.Background(), r)
	if calls != 2 {
		t.Fatalf("not refreshed: %d", calls)
	}
	// the identity provider's refusal surfaces with its error code; nothing is sent downstream
	r.Principal.Token = "revoked"
	before := got
	if _, f := d.Dispatch(context.Background(), r); f == nil || f.Code != "TARGET_UNAVAILABLE" || !strings.Contains(f.Message, "invalid_token") || got.auth != before.auth {
		t.Fatalf("%v", f)
	}
}
