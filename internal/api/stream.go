package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idp-workspace/internal/authorization"
	"idp-workspace/internal/transport"
)

// Stream intervals (ADR-0001 D10); tests shorten them.
var (
	StreamPoll      = 2 * time.Second
	StreamKeepAlive = 15 * time.Second
	streamOverlap   = 5 * time.Second
)

// inboxStream pushes the caller's visible item changes as server-sent events "item", each with id
// "<unix micros>:<itemId>" so a reconnect resumes with Last-Event-ID (or ?after=).
func (a *API) inboxStream(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !p.Has(authorization.RoleMember) && !p.Has(authorization.RoleAdmin) {
		transport.WriteProblem(w, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role ws-member required"))
		return
	}
	m, err := a.member(r, p)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	since := time.Now().Add(-streamOverlap)
	if last := firstNonEmpty(r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")); last != "" {
		micros, _, _ := strings.Cut(last, ":")
		n, err := strconv.ParseInt(micros, 10, 64)
		if err != nil {
			transport.WriteProblem(w, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "Last-Event-ID must be <unix micros>:<itemId>"))
			return
		}
		since = time.UnixMicro(n)
	} else if r.URL.Query().Get("replay") == "open" {
		since = time.Time{} // everything visible, then live
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) // a stream outlives the server's write timeout
	sse := transport.NewSSEWriter(w)
	if sse == nil {
		return
	}
	sent := map[string]int{} // itemId -> version already sent
	ctx := r.Context()
	poll := time.NewTicker(StreamPoll)
	defer poll.Stop()
	keep := time.NewTicker(StreamKeepAlive)
	defer keep.Stop()
	push := func() error {
		items, err := a.Store.ChangedItems(ctx, p.Tenant, m.Workspace.ID, p.Subject, m.Roles, since, 200)
		if err != nil {
			return err
		}
		for _, it := range items {
			if sent[it.ID] >= it.Version {
				continue
			}
			raw, _ := json.Marshal(it)
			if err := sse.Send(fmt.Sprintf("%d:%s", it.UpdatedAt.UnixMicro(), it.ID), "item", raw); err != nil {
				return err
			}
			sent[it.ID] = it.Version
			if t := it.UpdatedAt.Add(-streamOverlap); t.After(since) {
				since = t
			}
		}
		return nil
	}
	if err := push(); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-keep.C:
			if sse.Comment("keep-alive") != nil {
				return
			}
		case <-poll.C:
			if push() != nil {
				return
			}
		}
	}
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
