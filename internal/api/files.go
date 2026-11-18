package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/authorization"
	"idp-workspace/internal/files"
	"idp-workspace/internal/transport"
	"idp-workspace/internal/workspace"
)

// FileConfig is how the workspace keeps members' files (ADR-0001 D23).
type FileConfig struct {
	MaxBytes   int64         // one file (default 25 MB)
	QuotaBytes int64         // one member's files in the tenant (default 200 MB)
	QuotaFiles int           // (default 500)
	Retention  time.Duration // how long a file is kept (default 30 days)
	RunFiles   int           // files one run may deliver (default 10)
	// TicketReads is how often one read ticket may be presented (default 1: an attachment is fetched once, D27).
	TicketReads int
	// Tickets signs the tickets a run gets for its files; nil = runs get no file access (uploads still work).
	Tickets *files.Ticketer
}

func (c *FileConfig) defaults() {
	if c.MaxBytes == 0 {
		c.MaxBytes = 25 << 20
	}
	if c.QuotaBytes == 0 {
		c.QuotaBytes = 200 << 20
	}
	if c.QuotaFiles == 0 {
		c.QuotaFiles = 500
	}
	if c.Retention == 0 {
		c.Retention = 30 * 24 * time.Hour
	}
	if c.RunFiles == 0 {
		c.RunFiles = 10
	}
	if c.TicketReads <= 0 {
		c.TicketReads = 1
	}
}

// fileRoutes: the member's own files in a workspace (D23).
func (a *API) fileRoutes(mux *http.ServeMux) {
	member := authorization.RoleMember
	mux.Handle("POST /v1/workspaces/{ws}/files", a.role(a.uploadFile, member))
	mux.Handle("GET /v1/workspaces/{ws}/files", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		source := r.URL.Query().Get("source")
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		out, err := a.Store.ListFiles(r.Context(), p.Tenant, m.Workspace.ID, p.Subject, source, limit)
		return 200, map[string]any{"files": out}, err
	}, member))
	mux.Handle("GET /v1/workspaces/{ws}/files/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		f, _, err := a.Store.GetFile(r.Context(), p.Tenant, m.Workspace.ID, p.Subject, r.PathValue("id"), false)
		return 200, f, err
	}, member))
	mux.Handle("DELETE /v1/workspaces/{ws}/files/{id}", a.role(func(r *http.Request, p authorization.Principal) (int, any, error) {
		m, err := a.member(r, p)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusNoContent, nil, a.Store.DeleteFile(r.Context(), p.Tenant, m.Workspace.ID, p.Subject, r.PathValue("id"))
	}, member))
	mux.HandleFunc("GET /v1/workspaces/{ws}/files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r)
		if !p.Has(member) && !p.Has(authorization.RoleAdmin) {
			transport.WriteProblem(w, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role ws-member required"))
			return
		}
		m, err := a.member(r, p)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		f, data, err := a.Store.GetFile(r.Context(), p.Tenant, m.Workspace.ID, p.Subject, r.PathValue("id"), true)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		serveFile(w, f, data)
	})
}

// serveFile answers a download: an attachment the browser saves, never renders (D23).
func serveFile(w http.ResponseWriter, f *workspace.MemberFile, data []byte) {
	h := w.Header()
	h.Set("Content-Type", f.MediaType)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Idp-File-Ref", f.ID)
	h.Set("Idp-File-Sha256", f.SHA256)
	_, _ = w.Write(data)
}

// uploadFile keeps the request body as a file: POST /v1/workspaces/{ws}/files?name=<file name>.
func (a *API) uploadFile(r *http.Request, p authorization.Principal) (int, any, error) {
	m, err := a.member(r, p)
	if err != nil {
		return 0, nil, err
	}
	name := files.SafeName(r.URL.Query().Get("name"))
	if name == "" {
		return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "name is required (?name=<file name>)")
	}
	// ?sensitivity= is how the member classifies the file (default internal): a file above the tenant's ceiling is not
	// given to an agent that may use an external model (D28, idp-host D43)
	sensitivity := r.URL.Query().Get("sensitivity")
	if sensitivity != "" && !files.ValidSensitivity(sensitivity) {
		return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "sensitivity must be public, internal, confidential or restricted")
	}
	data, err := a.readFile(r)
	if err != nil {
		return 0, nil, err
	}
	if err := files.Check(name, data); err != nil {
		return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "FILE_REFUSED", err.Error())
	}
	f, err := a.Store.PutFile(r.Context(), p.Tenant, postgres.FileInput{WorkspaceID: m.Workspace.ID, Owner: p.Subject, Name: name, MediaType: files.MediaType(name),
		Sensitivity: sensitivity, Source: workspace.FileSourceUpload, Content: data, Retention: a.Files.Retention, QuotaBytes: a.Files.QuotaBytes, QuotaFiles: a.Files.QuotaFiles})
	if err != nil {
		return 0, nil, err
	}
	a.audit(r, p, "file.uploaded", f)
	return http.StatusCreated, f, nil
}

func (a *API) readFile(r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, a.Files.MaxBytes))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			return nil, transport.Fail(http.StatusRequestEntityTooLarge, "TOO_LARGE", "a file is at most "+strconv.FormatInt(a.Files.MaxBytes>>20, 10)+" MB")
		}
		return nil, transport.Fail(http.StatusBadRequest, "BAD_REQUEST", "body unreadable")
	}
	if len(data) == 0 {
		return nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "the file is empty")
	}
	return data, nil
}

func (a *API) audit(r *http.Request, p authorization.Principal, action string, f *workspace.MemberFile) {
	a.Log.Info("file", "action", action, "tenant", p.Tenant, "subject", p.Subject, "file", f.ID, "size", f.Size, "sha256", f.SHA256, "source", f.Source)
}

// InternalHandler serves the sandbox's file endpoint on the internal listener (D23; platform ADR-0002 F4): a run's
// ticket is the only credential. GET /internal/v1/files/content reads the file a read ticket names; POST
// /internal/v1/files delivers a file with a write ticket (name in Content-Disposition), kept as the member's file with
// the source run:<runId>.
func (a *API) InternalHandler() http.Handler {
	a.Files.defaults()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	open := func(w http.ResponseWriter, r *http.Request, kind string) (files.Ticket, bool) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || a.Files.Tickets == nil {
			transport.WriteProblem(w, transport.Fail(http.StatusUnauthorized, "UNAUTHENTICATED", "a file ticket is required"))
			return files.Ticket{}, false
		}
		t, err := a.Files.Tickets.Open(strings.TrimSpace(tok))
		if err != nil || t.Kind != kind {
			transport.WriteProblem(w, transport.Fail(http.StatusUnauthorized, "UNAUTHENTICATED", "invalid or expired file ticket"))
			return files.Ticket{}, false
		}
		// a ticket is good while its run lasts, and a read ticket for a set number of uses (D27). The use is recorded
		// before any byte is handed out: of two presentations at once, one is refused.
		uses := 0
		if kind == files.KindRead {
			uses = a.Files.TicketReads
		}
		if err := a.Store.SpendTicket(r.Context(), t.Tenant, t.ID, t.Run, uses, t.Expires); err != nil {
			var se *postgres.Error
			if errors.As(err, &se) && (se.Code == "TICKET_SPENT" || se.Code == "TICKET_ENDED") {
				a.Log.Warn("file ticket refused", "reason", se.Code, "tenant", t.Tenant, "subject", t.Owner, "run", t.Run, "kind", t.Kind)
			}
			a.writeError(w, r, err)
			return files.Ticket{}, false
		}
		return t, true
	}
	mux.HandleFunc("GET /internal/v1/files/content", func(w http.ResponseWriter, r *http.Request) {
		t, ok := open(w, r, files.KindRead)
		if !ok {
			return
		}
		f, data, err := a.Store.GetFile(r.Context(), t.Tenant, "", t.Owner, t.File, true)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		a.Log.Info("file", "action", "file.read", "tenant", t.Tenant, "subject", t.Owner, "file", f.ID, "run", t.Run)
		serveFile(w, f, data)
	})
	mux.HandleFunc("POST /internal/v1/files", func(w http.ResponseWriter, r *http.Request) {
		t, ok := open(w, r, files.KindWrite)
		if !ok {
			return
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
		name := files.SafeName(params["filename"])
		if name == "" {
			transport.WriteProblem(w, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "Content-Disposition must name the file"))
			return
		}
		limit := a.Files.MaxBytes
		if t.MaxBytes < limit {
			limit = t.MaxBytes
		}
		data, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, limit))
		if err != nil {
			transport.WriteProblem(w, transport.Fail(http.StatusRequestEntityTooLarge, "TOO_LARGE", "the file is larger than the run may deliver"))
			return
		}
		if err := files.Check(name, data); err != nil {
			transport.WriteProblem(w, transport.Fail(http.StatusUnprocessableEntity, "FILE_REFUSED", err.Error()))
			return
		}
		runFiles := a.Files.RunFiles
		if t.MaxFiles < runFiles {
			runFiles = t.MaxFiles
		}
		f, err := a.Store.PutFile(r.Context(), t.Tenant, postgres.FileInput{WorkspaceID: t.Workspace, Owner: t.Owner, Name: name, MediaType: files.MediaType(name),
			Sensitivity: t.Sensitivity, Source: "run:" + t.Run, Content: data, Retention: a.Files.Retention, QuotaBytes: a.Files.QuotaBytes, QuotaFiles: a.Files.QuotaFiles, RunFiles: runFiles})
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		a.Log.Info("file", "action", "file.delivered", "tenant", t.Tenant, "subject", t.Owner, "file", f.ID, "run", t.Run, "size", f.Size, "sha256", f.SHA256)
		transport.WriteJSON(w, http.StatusCreated, map[string]any{"ref": f.ID, "name": f.Name, "mediaType": f.MediaType, "size": f.Size, "sha256": f.SHA256})
	})
	return mux
}
