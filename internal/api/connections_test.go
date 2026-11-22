package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"idp-workspace/internal/authorization"
	"idp-workspace/internal/connections"
	"idp-workspace/internal/connections/connectionstest"
)

// The connected services behind the Workspace's HTTP surface: off when unset, behind the member's token, the callback page and
// the client metadata documents public, and the whole connect from the shell's point of view.

type tokens map[string]authorization.Principal

func (t tokens) Verify(_ context.Context, tok string) (authorization.Principal, error) {
	p, ok := t[tok]
	if !ok {
		return authorization.Principal{}, errors.New("no such token")
	}
	p.Token = tok
	return p, nil
}

var members = tokens{
	"tok-bob":    {Subject: "bob", Tenant: "t1", Roles: []string{"ws-member"}},
	"tok-alice":  {Subject: "alice", Tenant: "t1", Roles: []string{"ws-member"}},
	"tok-nobody": {Subject: "nobody", Tenant: "t1", Roles: []string{"somebody-else"}},
}

type rig struct {
	h       http.Handler
	bridge  *connectionstest.Bridge
	pending *connectionstest.Pending
	logs    *bytes.Buffer
}

func newRig(t *testing.T, on bool) *rig {
	t.Helper()
	web := t.TempDir()
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<!doctype html><title>shell</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &rig{bridge: connectionstest.New(t), pending: &connectionstest.Pending{}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a := &API{Auth: members, Log: log, WebDir: web}
	if on {
		a.Connections = &connections.Service{Bridge: r.bridge.Downstream(), PublicURL: "https://workspace.example", Pending: r.pending, Log: log}
	}
	r.h = a.Handler()
	return r
}

func (r *rig) do(method, path, token, body string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

func TestConnectionsAreOffWhenNotConfigured(t *testing.T) {
	r := newRig(t, false)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/me/connections", ""}, {"POST", "/v1/me/connections/github/authorization", `{}`}, {"POST", "/v1/me/connections/callback", `{"state":"x"}`},
		{"DELETE", "/v1/me/connections/github/grant", ""}, {"DELETE", "/v1/me/connections/github/reviewer/grant", ""},
	} {
		w := r.do(c.method, c.path, "tok-bob", c.body)
		if w.Code != 404 && w.Code != 405 {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	// the pages: a plain 404, not the shell's index that answers every path it does not know
	for _, p := range []string{"/oauth/callback?code=x&state=y", "/oauth/client/github.json"} {
		if w := r.do("GET", p, "", ""); w.Code != 404 || strings.Contains(w.Body.String(), "<title>") {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	// and /v1/me says so, for the shell's navigation
	var me map[string]any
	_ = json.Unmarshal(r.do("GET", "/v1/me", "tok-bob", "").Body.Bytes(), &me)
	if me["connections"] != false {
		t.Errorf("/v1/me: %v", me)
	}
	if len(r.bridge.Calls()) != 0 {
		t.Error("the bridge was called")
	}
}

func TestConnectionsRequireTheMembersTokenAndRole(t *testing.T) {
	r := newRig(t, true)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/me/connections", ""}, {"POST", "/v1/me/connections/github/authorization", `{}`}, {"POST", "/v1/me/connections/callback", `{"state":"x"}`},
		{"DELETE", "/v1/me/connections/github/grant", ""},
	} {
		if w := r.do(c.method, c.path, "", c.body); w.Code != 401 {
			t.Errorf("%s %s without a token: %d", c.method, c.path, w.Code)
		}
		if w := r.do(c.method, c.path, "tok-forged", c.body); w.Code != 401 {
			t.Errorf("%s %s with a bad token: %d", c.method, c.path, w.Code)
		}
		if w := r.do(c.method, c.path, "tok-nobody", c.body); w.Code != 403 {
			t.Errorf("%s %s without the member role: %d", c.method, c.path, w.Code)
		}
	}
	if len(r.bridge.Calls()) != 0 {
		t.Error("the bridge was called for a caller who is not a member")
	}
	var me map[string]any
	_ = json.Unmarshal(r.do("GET", "/v1/me", "tok-bob", "").Body.Bytes(), &me)
	if me["connections"] != true {
		t.Errorf("/v1/me: %v", me)
	}
}

func TestTheWholeConnectThroughTheHTTPSurface(t *testing.T) {
	r := newRig(t, true)
	const state = "state-0123456789abcdefghijklmnopqrstuvwxyz"
	r.bridge.Respond = func(c connectionstest.Call) (int, string) {
		switch {
		case c.Method == "GET":
			return 200, `{"connections":[{"id":"github","title":"GitHub","status":"not_connected"}]}`
		case strings.HasSuffix(c.Path, "/authorization"):
			return 200, `{"url":"https://github.example/authorize?state=` + state + `"}`
		case strings.HasSuffix(c.Path, "/callback"):
			return 200, `{"connection":"github","status":"connected"}`
		}
		return 204, ""
	}
	w := r.do("GET", "/v1/me/connections", "tok-bob", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"title":"GitHub"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = r.do("POST", "/v1/me/connections/github/authorization", "tok-bob", `{"returnPath":"/connections/github"}`)
	var auth struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &auth)
	if w.Code != 200 || auth.URL != "https://github.example/authorize?state="+state {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// the page the third party lands on is the shell, for anyone, never cached
	page := r.do("GET", "/oauth/callback?code=CODE-1&state="+state, "", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "<title>shell</title>") || page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("%d %v", page.Code, page.Header())
	}
	// alice cannot finish bob's
	w = r.do("POST", "/v1/me/connections/callback", "tok-alice", `{"code":"CODE-1","state":"`+state+`","iss":"","error":""}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), connections.CodeStateInvalid) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, c := range r.bridge.Calls() {
		if strings.HasSuffix(c.Path, "/callback") {
			t.Fatal("the bridge received alice's attempt")
		}
	}
	// a callback has no field that names a connection or an address: the connection is the one recorded. A body that tries is refused
	// as malformed, before the state is looked at (it stays usable)
	if w = r.do("POST", "/v1/me/connections/callback", "tok-bob", `{"code":"CODE-1","state":"`+state+`","connection":"notion"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "BAD_REQUEST") {
		t.Fatalf("a callback that names a connection: %d %s", w.Code, w.Body)
	}
	w = r.do("POST", "/v1/me/connections/callback", "tok-bob", `{"code":"CODE-1","state":"`+state+`","iss":"https://as.example","error":""}`)
	var res map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res["connection"] != "github" || res["status"] != "connected" || res["returnPath"] != "/connections/github" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w = r.do("POST", "/v1/me/connections/callback", "tok-bob", `{"code":"CODE-1","state":"`+state+`"}`); w.Code != 400 {
		t.Fatalf("the second use: %d", w.Code)
	}
	if w = r.do("DELETE", "/v1/me/connections/github/grant", "tok-bob", ""); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w = r.do("DELETE", "/v1/me/connections/github/reviewer/grant", "tok-bob", ""); w.Code != 204 {
		t.Fatalf("%d", w.Code)
	}
	// the body of a connect is closed: no field names another path, member or address
	if w = r.do("POST", "/v1/me/connections/github/authorization", "tok-bob", `{"url":"https://evil.example"}`); w.Code != 400 {
		t.Fatalf("an unknown field: %d", w.Code)
	}
	if w = r.do("POST", "/v1/me/connections/github/authorization", "tok-bob", `{"returnPath":"//evil.example"}`); w.Code != 422 {
		t.Fatalf("an open redirect: %d %s", w.Code, w.Body)
	}
	// neither the code nor the state are in the logs, through this surface either
	for _, secret := range []string{"CODE-1", state, "tok-bob", "tok-alice"} {
		if strings.Contains(r.logs.String(), secret) {
			t.Errorf("%q is in the logs:\n%s", secret, r.logs)
		}
	}
}

func TestClientMetadataDocumentsArePublicAndOnlyAtTheirOwnAddress(t *testing.T) {
	r := newRig(t, true)
	own := "https://workspace.example/oauth/client/github.json"
	r.bridge.Metadata["github"] = `{"client_id":"` + own + `","client_name":"IDP Workspace","redirect_uris":["https://workspace.example/oauth/callback"]}`
	r.bridge.Metadata["notion"] = `{"client_id":"` + own + `"}` // names another connection's address
	w := r.do("GET", "/oauth/client/github.json", "", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Body.String(), `"client_id":"`+own+`"`) || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body)
	}
	for _, p := range []string{"/oauth/client/notion.json", "/oauth/client/missing.json", "/oauth/client/github", "/oauth/client/github.json.json", "/oauth/client/git.hub.json", "/oauth/client/.json",
		"/oauth/client/" + strings.Repeat("a", 65) + ".json", "/oauth/client/gi%20thub.json", "/oauth/client/github.JSON"} {
		before := len(r.bridge.Calls())
		w := r.do("GET", p, "", "")
		if w.Code != 404 || strings.Contains(w.Body.String(), "client_id") {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
		// an id that does not match the connection id pattern never reaches the bridge
		if strings.Contains(p, "git.hub") || strings.Contains(p, "gi%20") || strings.Contains(p, "/.json") || strings.Contains(p, "aaaaaaaa") || strings.HasSuffix(p, "github.JSON") || strings.HasSuffix(p, "github") {
			if len(r.bridge.Calls()) != before {
				t.Errorf("%s: the bridge was asked", p)
			}
		}
	}
	if w := r.do("POST", "/oauth/client/github.json", "", ""); w.Code == 200 {
		t.Error("POST served the document")
	}
}
