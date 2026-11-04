package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"idp-workspace/internal/authorization"
	"idp-workspace/internal/runs"
	"idp-workspace/internal/workspace"
)

// shellCSP keeps the shell to its own origin; app pages are framed only from https (or loopback in
// development) and are further confined by the frame's sandbox and the bridge's origin check (D5, D9).
const shellCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self'; " +
	"frame-src https: http://localhost:* http://127.0.0.1:*; frame-ancestors 'none'; base-uri 'none'; form-action 'none'; object-src 'none'"

// web serves the built shell (D9): files under WebDir, index.html for every other path (client-side
// routes). API paths never reach here.
func (a *API) web(w http.ResponseWriter, r *http.Request) {
	if a.WebDir == "" || isAPI(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	file := filepath.Join(a.WebDir, filepath.FromSlash(clean))
	if st, err := os.Stat(file); err != nil || st.IsDir() || !strings.HasPrefix(file, filepath.Clean(a.WebDir)+string(filepath.Separator)) {
		file = filepath.Join(a.WebDir, "index.html")
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if strings.HasPrefix(clean, "/assets/") && file != filepath.Join(a.WebDir, "index.html") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable") // content-hashed by the build
	} else {
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", shellCSP)
		h.Set("X-Frame-Options", "DENY")
	}
	http.ServeFile(w, r, file)
}

// follow puts the "running" item and follows the run in the background (D11).
func (a *API) follow(r *http.Request, p authorization.Principal, workspaceID, title string, result json.RawMessage, wctx *workspace.WorkContext) {
	var res struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(result, &res) != nil || res.ID == "" || a.Dispatcher == nil || a.Dispatcher.Host == nil {
		return
	}
	token, err := a.Dispatcher.Host.Bearer(p)
	if err != nil {
		return
	}
	run := runs.Run{Tenant: p.Tenant, Workspace: workspaceID, Subject: p.Subject, Token: token, RunID: res.ID, Title: title}
	if wctx != nil {
		run.Entities = wctx.Entities
	}
	if err := a.Runs.Started(r.Context(), run); err != nil {
		a.Log.Warn("run item not put into the inbox", "run", res.ID, "error", err)
		return
	}
	go a.Runs.Follow(run)
}
