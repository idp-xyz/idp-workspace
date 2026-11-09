package dispatch

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"idp-workspace/internal/authorization"
)

// Memory is one conversation memory idp-host stored about the member in idp-cmi (ADR-0001 D21).
// Content is as stored; the workspace shows it and never interprets it.
type Memory struct {
	ID         string   `json:"id"`
	Content    string   `json:"content"`
	CreatedAt  string   `json:"createdAt"`
	ValidUntil string   `json:"validUntil,omitempty"`
	Source     string   `json:"source,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Tags       []string `json:"tags"`
}

// MaxMemories is how many records one listing asks idp-cmi for.
const MaxMemories = 100

// ListMemory reads the caller's own usable records (idp-cmi's default scope is the caller's
// subject), newest first. ds must be called with the member's token (forward or exchange), never a
// service credential: the workspace does not widen what the member may read.
func ListMemory(ctx context.Context, ds *Downstream, p authorization.Principal) ([]Memory, *Failure) {
	var out struct {
		Records []struct {
			RecordID   string   `json:"recordId"`
			Content    string   `json:"content"`
			Tags       []string `json:"tags"`
			Provenance struct {
				Source     string `json:"source"`
				Ref        string `json:"ref"`
				ObservedAt string `json:"observedAt"`
			} `json:"provenance"`
			Validity struct {
				ValidUntil string `json:"validUntil"`
			} `json:"validity"`
			CreatedAt string `json:"createdAt"`
		} `json:"records"`
	}
	if _, f := ds.Do(ctx, p, http.MethodGet, "/v1/memory/records?limit="+strconv.Itoa(MaxMemories), "", nil, &out); f != nil {
		return nil, f
	}
	list := make([]Memory, 0, len(out.Records))
	for _, r := range out.Records {
		m := Memory{ID: r.RecordID, Content: r.Content, CreatedAt: r.CreatedAt, ValidUntil: r.Validity.ValidUntil, Source: r.Provenance.Source, Ref: r.Provenance.Ref, Tags: r.Tags}
		if m.CreatedAt == "" {
			m.CreatedAt = r.Provenance.ObservedAt
		}
		if m.Tags == nil {
			m.Tags = []string{}
		}
		list = append(list, m)
	}
	// newest first (RFC 3339 strings of one zone sort by time; parse to be safe)
	sortNewestFirst(list)
	return list, nil
}

func sortNewestFirst(l []Memory) {
	at := func(m Memory) time.Time { t, _ := time.Parse(time.RFC3339Nano, m.CreatedAt); return t }
	for i := 1; i < len(l); i++ { // insertion sort: stable, at most 100 entries
		for j := i; j > 0 && at(l[j]).After(at(l[j-1])); j-- {
			l[j], l[j-1] = l[j-1], l[j]
		}
	}
}

// RevokeMemory revokes one record of the caller's own scope and returns how many idp-cmi revoked
// (0 or 1). idp-cmi answers 404 for an id that is not the caller's: that is passed on in the
// Failure's Status, not retried.
func RevokeMemory(ctx context.Context, ds *Downstream, p authorization.Principal, id string) (int, *Failure) {
	var out struct {
		Revoked int `json:"revoked"`
	}
	body := map[string]string{"recordId": id, "reason": "deleted by the member in the workspace"}
	if _, f := ds.Do(ctx, p, http.MethodPost, "/v1/memory/revoke", "", body, &out); f != nil {
		return 0, f
	}
	return out.Revoked, nil
}

// ExportMemory fetches the caller's own memory as idp-cmi's export document (ADR-0001 D36), passed
// on unchanged for the member to save. Like the list, it uses the member's own token.
func ExportMemory(ctx context.Context, ds *Downstream, p authorization.Principal) (map[string]any, *Failure) {
	var out map[string]any
	if _, f := ds.Do(ctx, p, http.MethodGet, "/v1/memory/export", "", nil, &out); f != nil {
		return nil, f
	}
	return out, nil
}
