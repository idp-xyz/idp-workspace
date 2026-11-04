// Package api serves the IDP Workspace (ADR-0001): administration of workspaces, members, the app
// registry and installations (/api/v1, ws-admin); the member's workspace (/v1: apps, inbox, work
// context, action invocation); and the producers' inbox interface (/v1/.../inbox/items, ws-producer).
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idp-workspace/adapters/identity"
	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/authorization"
	"idp-workspace/internal/canonical"
	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/observability"
	"idp-workspace/internal/runs"
	"idp-workspace/internal/schema"
	"idp-workspace/internal/transport"
	"idp-workspace/internal/workspace"
)

// MaxBody bounds request bodies.
const MaxBody = 1 << 20

// Verifier authenticates bearer tokens.
type Verifier interface {
	Verify(ctx context.Context, token string) (authorization.Principal, error)
}

// API serves the workspace.
type API struct {
	Store      *postgres.Store
	Auth       Verifier
	Dispatcher *dispatch.Dispatcher
	// Runs relays the results of host.run actions into the inbox (D11); nil = not followed.
	Runs *runs.Follower
	// WebDir holds the built web shell (D9); "" = the API only.
	WebDir string
	// DevMode tells the shell to offer development sign-in.
	DevMode bool
	Log     *slog.Logger
	Metrics *observability.Metrics
	Version string

	invocations *observability.CounterVec
}

type ctxKey int

const principalKey ctxKey = 1

func principalOf(r *http.Request) authorization.Principal {
	p, _ := r.Context().Value(principalKey).(authorization.Principal)
	return p
}

var open = map[string]bool{"/healthz": true, "/readyz": true, "/version": true}

// isAPI reports whether a path belongs to the authenticated API; everything else is the web shell's
// static files (D9), which carry no data.
func isAPI(path string) bool {
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/")
}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if open[r.URL.Path] || !isAPI(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		tok, err := identity.Bearer(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			transport.WriteProblem(w, transport.Fail(http.StatusUnauthorized, "UNAUTHENTICATED", "bearer token required"))
			return
		}
		p, err := a.Auth.Verify(r.Context(), tok)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			transport.WriteProblem(w, transport.Fail(http.StatusUnauthorized, "UNAUTHENTICATED", "invalid token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

type handler func(r *http.Request, p authorization.Principal) (int, any, error)

type withHeaders struct {
	body    any
	headers map[string]string
}

// role requires one of the roles; an administrator passes every role check (but not the roles of
// an action contract, which belong to the workspace: D4).
func (a *API) role(h handler, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r)
		ok := p.Has(authorization.RoleAdmin)
		for _, role := range roles {
			ok = ok || p.Has(role)
		}
		if !ok {
			transport.WriteProblem(w, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role "+roles[0]+" required"))
			return
		}
		status, body, err := h(r, p)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		if wh, ok := body.(*withHeaders); ok {
			for k, v := range wh.headers {
				w.Header().Set(k, v)
			}
			body = wh.body
		}
		if body == nil {
			w.WriteHeader(status)
			return
		}
		transport.WriteJSON(w, status, body)
	}
}

func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var se *postgres.Error
	var pb *transport.Problem
	switch {
	case errors.As(err, &pb):
	case errors.As(err, &se):
		status := map[string]int{"NOT_FOUND": 404, "CONFLICT": 409, "FORBIDDEN": 403, "INVALID": 422}[se.Code]
		if status == 0 {
			status = 500
		}
		pb = transport.Fail(status, se.Code, se.Message)
	default:
		a.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err, "trace", observability.TraceID(r.Context()))
		pb = transport.Fail(http.StatusInternalServerError, "INTERNAL", "internal error")
	}
	transport.WriteProblem(w, pb)
}

func readBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, MaxBody))
	if err != nil {
		return nil, transport.Fail(http.StatusBadRequest, "BAD_REQUEST", "body unreadable: "+err.Error())
	}
	return raw, nil
}

func decodeRaw(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return transport.Fail(http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body: "+err.Error())
	}
	return nil
}

func decode(r *http.Request, v any) error {
	raw, err := readBody(r)
	if err != nil {
		return err
	}
	return decodeRaw(raw, v)
}

// ifMatch reads a positive expected revision from If-Match; 0 when absent.
func ifMatch(r *http.Request) (int, error) {
	v := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "If-Match must be a positive revision")
	}
	return n, nil
}

// member resolves the workspace of the path and requires the caller to be a member of it
// (administrators pass). Not a member = 404, so workspace names do not leak.
func (a *API) member(r *http.Request, p authorization.Principal) (*postgres.Membership, error) {
	m, err := a.Store.MembershipOf(r.Context(), p.Tenant, r.PathValue("ws"), p.Subject)
	if err != nil {
		return nil, err
	}
	if !m.Member && !p.Has(authorization.RoleAdmin) {
		return nil, transport.Fail(http.StatusNotFound, "NOT_FOUND", "workspace not found")
	}
	return m, nil
}

// workspaceOf resolves the workspace of the path without a membership check (administration, producers).
func (a *API) workspaceOf(r *http.Request, p authorization.Principal) (*workspace.Workspace, error) {
	return a.Store.GetWorkspace(r.Context(), p.Tenant, r.PathValue("ws"))
}

// Handler returns the routes behind authentication.
func (a *API) Handler() http.Handler {
	if a.Log == nil {
		a.Log = slog.Default()
	}
	if a.Metrics != nil {
		a.invocations = a.Metrics.Counter("idp_workspace_invocations_total", "Action invocations by target kind and status.", "target", "status")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := a.Store.Pool.Ping(ctx); err != nil {
			transport.WriteProblem(w, transport.Fail(http.StatusServiceUnavailable, "NOT_READY", "database unreachable"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		transport.WriteJSON(w, 200, map[string]any{"service": "idp-workspace", "version": a.Version})
	})
	admin, member, producer := authorization.RoleAdmin, authorization.RoleMember, authorization.RoleProducer

	// ---- administration (D1, D2) ----
	mux.Handle("POST /api/v1/workspaces", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.CreateWorkspace(r.Context(), p.Tenant, in.Name, in.DisplayName, p.Subject)
		return http.StatusCreated, out, err
	}, admin))
	mux.Handle("GET /api/v1/workspaces/{ws}/members", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		w, err := a.workspaceOf(r, p)
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.ListMembers(r.Context(), p.Tenant, w.ID)
		return 200, map[string]any{"members": out}, err
	}, admin))
	mux.Handle("PUT /api/v1/workspaces/{ws}/members/{subject}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Roles []string `json:"roles"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		w, err := a.workspaceOf(r, p)
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.PutMember(r.Context(), p.Tenant, w.ID, r.PathValue("subject"), in.Roles, p.Subject)
		return 200, out, err
	}, admin))
	mux.Handle("DELETE /api/v1/workspaces/{ws}/members/{subject}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		w, err := a.workspaceOf(r, p)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Store.RemoveMember(r.Context(), p.Tenant, w.ID, r.PathValue("subject"), p.Subject)
	}, admin))
	mux.Handle("POST /api/v1/apps", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		raw, err := readBody(r)
		if err != nil {
			return 0, nil, err
		}
		m, canon, digest, err := parseManifest(raw)
		if err != nil {
			return 0, nil, err
		}
		out, created, err := a.Store.PublishRelease(r.Context(), p.Tenant, *m, canon, digest, p.Subject)
		if err != nil {
			return 0, nil, err
		}
		if created {
			return http.StatusCreated, out, nil
		}
		return http.StatusOK, out, nil
	}, admin))
	mux.Handle("GET /api/v1/apps", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListReleases(r.Context(), p.Tenant, r.URL.Query().Get("name"))
		return 200, map[string]any{"releases": out}, err
	}, admin))
	mux.Handle("GET /api/v1/apps/{name}/{version}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.GetRelease(r.Context(), p.Tenant, r.PathValue("name"), r.PathValue("version"))
		return 200, out, err
	}, admin))
	mux.Handle("POST /api/v1/apps/{name}/{version}/revoke", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Store.RevokeRelease(r.Context(), p.Tenant, r.PathValue("name"), r.PathValue("version"), in.Reason, p.Subject)
		return 200, out, err
	}, admin))
	mux.Handle("PUT /api/v1/workspaces/{ws}/installations/{app}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Version string          `json:"version"`
			Config  json.RawMessage `json:"config"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		expected, err := ifMatch(r)
		if err != nil {
			return 0, nil, err
		}
		w, err := a.workspaceOf(r, p)
		if err != nil {
			return 0, nil, err
		}
		rel, err := a.Store.GetRelease(r.Context(), p.Tenant, r.PathValue("app"), in.Version)
		if err != nil {
			return 0, nil, err
		}
		if len(bytes.TrimSpace(in.Config)) == 0 || string(bytes.TrimSpace(in.Config)) == "null" {
			in.Config = json.RawMessage(`{}`)
		}
		if rel.Manifest.Config != nil { // D2: configuration errors surface at install time
			s, err := schema.Compile(rel.Manifest.Config.Schema)
			if err != nil {
				return 0, nil, transport.Fail(http.StatusConflict, "INVALID_MANIFEST", "config schema: "+err.Error())
			}
			if err := schema.Validate(s, in.Config); err != nil {
				return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID_CONFIG", err.Error())
			}
		} else if string(bytes.TrimSpace(in.Config)) != "{}" {
			return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID_CONFIG", "the app declares no configuration")
		}
		out, created, err := a.Store.Install(r.Context(), p.Tenant, w.ID, r.PathValue("app"), in.Version, in.Config, p.Subject, expected)
		if err != nil {
			return 0, nil, err
		}
		if created {
			return http.StatusCreated, out, nil
		}
		return http.StatusOK, out, nil
	}, admin))
	mux.Handle("DELETE /api/v1/workspaces/{ws}/installations/{app}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		w, err := a.workspaceOf(r, p)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Store.Uninstall(r.Context(), p.Tenant, w.ID, r.PathValue("app"), p.Subject)
	}, admin))
	mux.Handle("GET /api/v1/workspaces/{ws}/invocations", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		w, err := a.workspaceOf(r, p)
		if err != nil {
			return 0, nil, err
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListInvocations(r.Context(), p.Tenant, w.ID, r.URL.Query().Get("subject"), limit)
		return 200, map[string]any{"invocations": out}, err
	}, admin))
	mux.Handle("GET /api/v1/audit", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListAudit(r.Context(), p.Tenant, limit)
		return 200, map[string]any{"events": out}, err
	}, admin))

	// ---- the member's workspace (D1, D3, D5) ----
	mux.Handle("GET /v1/workspaces", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Store.ListWorkspaces(r.Context(), p.Tenant, p.Subject, p.Has(authorization.RoleAdmin))
		return 200, map[string]any{"workspaces": out}, err
	}, member))
	mux.Handle("GET /v1/workspaces/{ws}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		return 200, map[string]any{"workspace": m.Workspace, "roles": m.Roles, "member": m.Member}, nil
	}, member))
	mux.Handle("GET /v1/workspaces/{ws}/apps", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		ins, err := a.Store.ListInstallations(r.Context(), p.Tenant, m.Workspace.ID)
		if err != nil {
			return 0, nil, err
		}
		type app struct {
			Installation workspace.Installation `json:"installation"`
			Manifest     workspace.Manifest     `json:"manifest"`
			Digest       string                 `json:"digest"`
			Origin       string                 `json:"origin,omitempty"`
		}
		out := []app{}
		for _, in := range ins {
			if in.ReleaseStatus != "published" {
				continue // a revoked release is not offered (D2)
			}
			rel, err := a.Store.GetRelease(r.Context(), p.Tenant, in.App, in.Version)
			if err != nil {
				return 0, nil, err
			}
			x := app{Installation: in, Manifest: rel.Manifest, Digest: rel.Digest}
			if rel.Manifest.Entry != nil {
				x.Origin = rel.Manifest.Entry.Origin()
			}
			out = append(out, x)
		}
		return 200, map[string]any{"apps": out}, nil
	}, member))
	mux.Handle("GET /v1/workspaces/{ws}/inbox", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		status := q.Get("status")
		if status != "" && status != "all" && status != workspace.ItemOpen && status != workspace.ItemResolved && status != workspace.ItemWithdrawn {
			return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "status must be open, resolved, withdrawn or all")
		}
		out, err := a.Store.ListInbox(r.Context(), p.Tenant, m.Workspace.ID, p.Subject, m.Roles, postgres.InboxQuery{Status: status, Kind: q.Get("kind"), Archived: q.Get("archived") == "true", Limit: limit})
		return 200, map[string]any{"items": out}, err
	}, member))
	mux.Handle("GET /v1/workspaces/{ws}/inbox/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.GetItem(r.Context(), p.Tenant, m.Workspace.ID, r.PathValue("id"), p.Subject, m.Roles)
		return 200, out, err
	}, member))
	mux.Handle("POST /v1/workspaces/{ws}/inbox/{id}/marks", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Read     *bool `json:"read"`
			Archived *bool `json:"archived"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.MarkItem(r.Context(), p.Tenant, m.Workspace.ID, r.PathValue("id"), p.Subject, m.Roles, in.Read, in.Archived)
		return 200, out, err
	}, member))
	mux.Handle("GET /v1/workspaces/{ws}/context", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		out, err := a.Store.GetContext(r.Context(), p.Tenant, m.Workspace.ID, p.Subject)
		return 200, out, err
	}, member))
	mux.Handle("PUT /v1/workspaces/{ws}/context", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in workspace.WorkContext
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		expected := -1 // unconditional unless If-Match names a version (0 = must not exist yet)
		if v := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "If-Match must be a version number")
			}
			expected = n
		}
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		if in.ItemID != "" {
			if _, err := a.Store.GetItem(r.Context(), p.Tenant, m.Workspace.ID, in.ItemID, p.Subject, m.Roles); err != nil {
				return 0, nil, err
			}
		}
		out, err := a.Store.SetContext(r.Context(), p.Tenant, m.Workspace.ID, p.Subject, in, expected)
		return 200, out, err
	}, member))
	mux.Handle("GET /v1/workspaces/{ws}/invocations", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListInvocations(r.Context(), p.Tenant, m.Workspace.ID, p.Subject, limit)
		return 200, map[string]any{"invocations": out}, err
	}, member))
	mux.Handle("POST /v1/workspaces/{ws}/actions/invoke", a.role(a.invoke, member))
	mux.HandleFunc("GET /v1/workspaces/{ws}/inbox/stream", a.inboxStream)

	// ---- producers (D3) ----
	mux.Handle("POST /v1/workspaces/{ws}/inbox/items", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in postgres.ItemInput
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		w, err := a.workspaceOf(r, p)
		if err != nil {
			return 0, nil, err
		}
		out, created, err := a.Store.PutItem(r.Context(), p.Tenant, w.ID, p.Subject, in)
		if err != nil {
			return 0, nil, err
		}
		if created {
			return http.StatusCreated, out, nil
		}
		return http.StatusOK, out, nil
	}, producer))
	mux.Handle("POST /v1/workspaces/{ws}/inbox/close", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			ItemID      string `json:"itemId"`
			ExternalRef string `json:"externalRef"`
			Status      string `json:"status"`
			Outcome     string `json:"outcome"`
			Note        string `json:"note"`
		}
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		if (in.ItemID == "") == (in.ExternalRef == "") {
			return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "give itemId or externalRef")
		}
		w, err := a.workspaceOf(r, p)
		if err != nil {
			return 0, nil, err
		}
		ref, byExternal := in.ItemID, false
		if in.ExternalRef != "" {
			ref, byExternal = in.ExternalRef, true
		}
		out, err := a.Store.CloseItem(r.Context(), p.Tenant, w.ID, p.Subject, ref, byExternal, in.Status, in.Outcome, in.Note)
		return 200, out, err
	}, producer))
	mux.Handle("GET /v1/workspaces/{ws}/inbox/produced", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		w, err := a.workspaceOf(r, p)
		if err != nil {
			return 0, nil, err
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		status := r.URL.Query().Get("status")
		if status == "" {
			status = "all"
		}
		out, err := a.Store.ListInbox(r.Context(), p.Tenant, w.ID, p.Subject, nil, postgres.InboxQuery{Status: status, Archived: true, Limit: limit, Producer: p.Subject})
		return 200, map[string]any{"items": out}, err
	}, producer))
	mux.HandleFunc("GET /shell-config.json", func(w http.ResponseWriter, _ *http.Request) {
		transport.WriteJSON(w, 200, map[string]any{"devMode": a.DevMode, "version": a.Version})
	})
	mux.HandleFunc("GET /", a.web)
	return a.authenticate(mux)
}

// parseManifest decodes and checks a manifest, returning it with its canonical form and digest.
func parseManifest(raw []byte) (*workspace.Manifest, []byte, string, error) {
	var m workspace.Manifest
	if err := decodeRaw(raw, &m); err != nil {
		return nil, nil, "", err
	}
	if m.Capabilities == nil {
		m.Capabilities = []string{}
	}
	if m.Actions == nil {
		m.Actions = []workspace.ActionContract{}
	}
	if err := m.Validate(); err != nil {
		return nil, nil, "", transport.Fail(http.StatusUnprocessableEntity, "INVALID_MANIFEST", err.Error())
	}
	for _, act := range m.Actions {
		if _, err := schema.Compile(act.Input); err != nil {
			return nil, nil, "", transport.Fail(http.StatusUnprocessableEntity, "INVALID_MANIFEST", "action "+act.Name+" input: "+err.Error())
		}
	}
	if m.Config != nil {
		if _, err := schema.Compile(m.Config.Schema); err != nil {
			return nil, nil, "", transport.Fail(http.StatusUnprocessableEntity, "INVALID_MANIFEST", "config schema: "+err.Error())
		}
	}
	canon, err := canonical.Canonical(m)
	if err != nil {
		return nil, nil, "", err
	}
	sum := sha256.Sum256(canon)
	return &m, canon, "sha256:" + hex.EncodeToString(sum[:]), nil
}
