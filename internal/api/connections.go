package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/authorization"
	"idp-workspace/internal/connections"
)

// Connected services (ADR-0001 D30). Without a.Connections none of these routes exists (the OAuth pages answer 404, /v1/me says
// "connections": false and the shell shows no page). They are the member's own, not a workspace's: a person connects their own
// account once, for every workspace.
//
// The Workspace has no cookie session: the member's session is the shell's bearer token. The third party's redirect to the
// callback page is a plain browser navigation with no Authorization header, so the page is the shell itself (served to anyone, it
// carries no data) and the shell hands the redirect's parameters to POST /v1/me/connections/callback with the member's token. What
// ties the callback to the person who started is the pending connect recorded for their tenant and subject.
func (a *API) connectionRoutes(mux *http.ServeMux) {
	if a.Connections == nil {
		// off: the shell's index would otherwise answer these paths (it answers every path it does not know); they are not pages
		mux.HandleFunc("GET /oauth/", http.NotFound)
		return
	}
	member := authorization.RoleMember
	mux.Handle("GET /v1/me/connections", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		out, err := a.Connections.List(r.Context(), p)
		if err != nil {
			return 0, nil, err
		}
		return 200, out, nil
	}, member))
	mux.Handle("POST /v1/me/connections/{id}/authorization", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in struct {
			Reviewer   bool   `json:"reviewer"`
			ReturnPath string `json:"returnPath"`
		}
		raw, err := readBody(r)
		if err != nil {
			return 0, nil, err
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			if err := decodeRaw(raw, &in); err != nil {
				return 0, nil, err
			}
		}
		out, err := a.Connections.Connect(r.Context(), p, r.PathValue("id"), in.Reviewer, in.ReturnPath)
		if err != nil {
			return 0, nil, err
		}
		return 200, out, nil
	}, member))
	mux.Handle("POST /v1/me/connections/callback", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		var in connections.Callback
		if err := decode(r, &in); err != nil {
			return 0, nil, err
		}
		out, err := a.Connections.Finish(r.Context(), p, in)
		if err != nil {
			return 0, nil, err
		}
		return 200, out, nil
	}, member))
	mux.Handle("DELETE /v1/me/connections/{id}/grant", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		return http.StatusNoContent, nil, a.Connections.Disconnect(r.Context(), p, r.PathValue("id"), false)
	}, member))
	mux.Handle("DELETE /v1/me/connections/{id}/reviewer/grant", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		return http.StatusNoContent, nil, a.Connections.Disconnect(r.Context(), p, r.PathValue("id"), true)
	}, member))
	// the page the third party's redirect lands on (the bridge's IDP_BRIDGE_REDIRECT_URI): the shell, which finishes the connect
	mux.HandleFunc("GET "+connections.CallbackPath, a.oauthCallbackPage)
	// the client metadata documents (public: the authorization server fetches them)
	mux.HandleFunc("GET "+connections.ClientPrefix+"{file}", a.clientDocument)
}

// oauthCallbackPage serves the shell for the callback. The redirect's query carries the authorization code: the page is never
// cached or sent on in a Referer, and the shell takes the query out of the address bar at once.
func (a *API) oauthCallbackPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	a.web(w, r)
}

// clientDocument serves a connection's client metadata document at the address that is its client_id: the bridge's document, and
// only when it names exactly this address. Anything else is a plain 404, whatever the reason.
func (a *API) clientDocument(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	id, ok := strings.CutSuffix(file, connections.ClientSuffix)
	if !ok {
		http.NotFound(w, r)
		return
	}
	doc, ok := a.Connections.ClientDocument(r.Context(), id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age=60")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc)
}

// AuditConnection writes the audit record of a member's connect or disconnect (never a code or state).
func (a *API) AuditConnection(ctx context.Context, p authorization.Principal, action, resource string, detail map[string]any) error {
	return a.Store.InTenant(ctx, p.Tenant, func(tx pgx.Tx) error {
		return postgres.Audit(ctx, tx, p.Tenant, p.Subject, action, "", map[string]any{"connection": resource, "detail": detail})
	})
}
