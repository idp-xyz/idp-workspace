package api

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/authorization"
	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/transport"
)

// "My memory" (ADR-0001 D21): the member reads and deletes what idp-host stored about them in
// idp-cmi. The memory belongs to the person, so these routes are not under a workspace. Every call
// to idp-cmi carries the member's own token (forward or exchanged for the CMI audience), never a
// service credential; idp-cmi decides what is theirs.

func (a *API) cmi() (*dispatch.Downstream, error) {
	if a.CMI == nil {
		return nil, transport.Fail(http.StatusNotFound, "NOT_CONFIGURED", "memory is not configured")
	}
	return a.CMI, nil
}

// memoryProblem maps a failed call to idp-cmi: its 404 and 403 pass through, like the dispatcher
// every other failure is TARGET_REJECTED (the CMI refused) or TARGET_UNAVAILABLE.
func memoryProblem(f *dispatch.Failure) error {
	switch {
	case f.Status == http.StatusNotFound:
		return transport.Fail(http.StatusNotFound, "NOT_FOUND", "no such memory")
	case f.Status == http.StatusForbidden:
		return transport.Fail(http.StatusForbidden, "FORBIDDEN", "the memory store does not let you delete this")
	case f.Code == "TARGET_REJECTED":
		return transport.Fail(http.StatusBadGateway, "TARGET_REJECTED", f.Message)
	}
	return transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", f.Message)
}

func (a *API) memoryList(r *http.Request, p authorization.Principal) (int, any, error) {
	ds, err := a.cmi()
	if err != nil {
		return 0, nil, err
	}
	list, f := dispatch.ListMemory(r.Context(), ds, p)
	if f != nil {
		return 0, nil, memoryProblem(f)
	}
	return 200, map[string]any{"memories": list}, nil
}

// memoryID accepts only a UUID-like token: it goes into a request body to idp-cmi and an audit row.
func memoryID(s string) bool { return postgres.IsUUID(s) && len(s) == 36 }

func (a *API) memoryDeleteOne(r *http.Request, p authorization.Principal) (int, any, error) {
	ds, err := a.cmi()
	if err != nil {
		return 0, nil, err
	}
	id := strings.ToLower(r.PathValue("id"))
	if !memoryID(id) {
		return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "the memory id is not a UUID")
	}
	n, f := dispatch.RevokeMemory(r.Context(), ds, p, id)
	if f != nil {
		return 0, nil, memoryProblem(f)
	}
	if n == 0 {
		return 0, nil, transport.Fail(http.StatusNotFound, "NOT_FOUND", "no such memory")
	}
	if err := a.auditMemory(r, p, id, n); err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"revoked": n}, nil
}

// memoryDeleteAll lists the caller's memory (as idp-cmi shows it to them) and revokes each record.
// It stops at the first failure and answers it; what was already revoked is audited.
func (a *API) memoryDeleteAll(r *http.Request, p authorization.Principal) (int, any, error) {
	ds, err := a.cmi()
	if err != nil {
		return 0, nil, err
	}
	list, f := dispatch.ListMemory(r.Context(), ds, p)
	if f != nil {
		return 0, nil, memoryProblem(f)
	}
	total := 0
	var failed error
	for _, m := range list {
		if !memoryID(strings.ToLower(m.ID)) {
			continue // not something this workspace would put in a request
		}
		n, f := dispatch.RevokeMemory(r.Context(), ds, p, m.ID)
		if f != nil {
			failed = memoryProblem(f)
			break
		}
		total += n
	}
	if total > 0 || failed == nil {
		if err := a.auditMemory(r, p, "", total); err != nil {
			return 0, nil, err
		}
	}
	if failed != nil {
		return 0, nil, failed
	}
	return 200, map[string]any{"revoked": total}, nil
}

// auditMemory records that the member deleted memory: the id (none for "all") and the count, never
// the content.
func (a *API) auditMemory(r *http.Request, p authorization.Principal, id string, count int) error {
	return a.Store.InTenant(r.Context(), p.Tenant, func(tx pgx.Tx) error {
		return postgres.Audit(r.Context(), tx, p.Tenant, p.Subject, "memory.delete", id, map[string]any{"count": count})
	})
}
