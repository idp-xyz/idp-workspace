package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/authorization"
	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/transport"
)

// Conversations with the agents (ADR-0001 D22). The shell follows a run's AG-UI events as they happen,
// decides a tool call that waits for the member's confirmation, cancels a run and reads a conversation's
// messages - through the workspace, which relays to idp-host with the member's own credential for it
// (D16) and never with its own. The workspace reaches only what the member started from this workspace:
// its invocation records name every run and every thread, and nothing else is relayed. idp-host remains
// the owner of runs, messages and decisions; the workspace keeps no copy.

// hostID is what a run, thread or tool call id may look like before it goes into a path to idp-host.
var hostID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func (a *API) hostTarget() (*dispatch.Downstream, error) {
	if a.Dispatcher == nil || a.Dispatcher.Host == nil {
		return nil, transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", "no idp-host is configured")
	}
	return a.Dispatcher.Host, nil
}

// owned checks that the member started the run (field "id") or the conversation (field "threadId") from
// this workspace; anything else is not found, as it is not the workspace's to show.
func (a *API) owned(r *http.Request, p authorization.Principal, m *postgres.Membership, field, value string) error {
	if !hostID.MatchString(value) {
		return transport.Fail(http.StatusNotFound, "NOT_FOUND", "no such "+map[string]string{"id": "run", "threadId": "conversation"}[field]+" in this workspace")
	}
	if _, err := a.Store.InvocationByResult(r.Context(), p.Tenant, m.Workspace.ID, p.Subject, field, value); err != nil {
		var se *postgres.Error
		if errors.As(err, &se) && se.Code == "NOT_FOUND" {
			return transport.Fail(http.StatusNotFound, "NOT_FOUND", "no such "+map[string]string{"id": "run", "threadId": "conversation"}[field]+" in this workspace")
		}
		return err
	}
	return nil
}

// relayed prepares one relayed request: the membership, the ownership of what the path names, the
// downstream.
func (a *API) relayed(r *http.Request, p authorization.Principal, field, value string) (*dispatch.Downstream, error) {
	m, err := a.member(r, p)
	if err != nil {
		return nil, err
	}
	if err := a.owned(r, p, m, field, value); err != nil {
		return nil, err
	}
	return a.hostTarget()
}

func (a *API) conversations(r *http.Request, p authorization.Principal) (int, any, error) {
	m, err := a.member(r, p)
	if err != nil {
		return 0, nil, err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := a.Store.ListConversations(r.Context(), p.Tenant, m.Workspace.ID, p.Subject, limit)
	return 200, map[string]any{"conversations": out}, err
}

func (a *API) runGet(r *http.Request, p authorization.Principal) (int, any, error) {
	run := r.PathValue("run")
	ds, err := a.relayed(r, p, "id", run)
	if err != nil {
		return 0, nil, err
	}
	return relayJSON(r.Context(), ds, p, http.MethodGet, "/v1/runs/"+url.PathEscape(run), nil)
}

func (a *API) runToolCalls(r *http.Request, p authorization.Principal) (int, any, error) {
	run := r.PathValue("run")
	ds, err := a.relayed(r, p, "id", run)
	if err != nil {
		return 0, nil, err
	}
	return relayJSON(r.Context(), ds, p, http.MethodGet, "/v1/runs/"+url.PathEscape(run)+"/tool-calls", nil)
}

// runDecide confirms or rejects a tool call that waits for the member (idp-host ADR-0001 D7: only the
// run's owner may, and the member is the owner because the workspace started the run with a token
// exchanged for them).
func (a *API) runDecide(r *http.Request, p authorization.Principal) (int, any, error) {
	var in struct {
		Decision string `json:"decision"`
	}
	raw, err := readBody(r)
	if err != nil {
		return 0, nil, err
	}
	if err := decodeRaw(raw, &in); err != nil {
		return 0, nil, err
	}
	if in.Decision != "confirm" && in.Decision != "reject" {
		return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "decision must be confirm or reject")
	}
	run, call := r.PathValue("run"), r.PathValue("call")
	if !hostID.MatchString(call) {
		return 0, nil, transport.Fail(http.StatusNotFound, "NOT_FOUND", "no such tool call")
	}
	ds, err := a.relayed(r, p, "id", run)
	if err != nil {
		return 0, nil, err
	}
	status, out, err := relayJSON(r.Context(), ds, p, http.MethodPost, "/v1/runs/"+url.PathEscape(run)+"/tool-calls/"+url.PathEscape(call)+"/decide", in)
	if err == nil {
		a.Log.Info("tool call decided", "run", run, "call", call, "decision", in.Decision, "subject", p.Subject)
	}
	return status, out, err
}

func (a *API) runCancel(r *http.Request, p authorization.Principal) (int, any, error) {
	run := r.PathValue("run")
	ds, err := a.relayed(r, p, "id", run)
	if err != nil {
		return 0, nil, err
	}
	status, out, err := relayJSON(r.Context(), ds, p, http.MethodPost, "/v1/runs/"+url.PathEscape(run)+"/cancel", nil)
	if err == nil {
		a.Log.Info("run cancel requested", "run", run, "subject", p.Subject)
	}
	return status, out, err
}

func (a *API) threadMessages(r *http.Request, p authorization.Principal) (int, any, error) {
	thread := r.PathValue("thread")
	ds, err := a.relayed(r, p, "threadId", thread)
	if err != nil {
		return 0, nil, err
	}
	return relayJSON(r.Context(), ds, p, http.MethodGet, "/v1/threads/"+url.PathEscape(thread)+"/messages", nil)
}

func (a *API) threadRuns(r *http.Request, p authorization.Principal) (int, any, error) {
	thread := r.PathValue("thread")
	ds, err := a.relayed(r, p, "threadId", thread)
	if err != nil {
		return 0, nil, err
	}
	return relayJSON(r.Context(), ds, p, http.MethodGet, "/v1/threads/"+url.PathEscape(thread)+"/runs", nil)
}

// runEvents relays the run's event log: as Server-Sent Events while the client accepts them (live, from
// `after` or Last-Event-ID, until idp-host closes the stream at the run's end or its interruption), as JSON
// otherwise.
func (a *API) runEvents(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	if !p.Has(authorization.RoleMember) && !p.Has(authorization.RoleAdmin) {
		transport.WriteProblem(w, transport.Fail(http.StatusForbidden, "FORBIDDEN", "role ws-member required"))
		return
	}
	run := r.PathValue("run")
	ds, err := a.relayed(r, p, "id", run)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	path := "/v1/runs/" + url.PathEscape(run) + "/events"
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		q := url.Values{}
		for _, k := range []string{"after", "limit"} {
			if v := r.URL.Query().Get(k); v != "" {
				q.Set(k, v)
			}
		}
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		status, out, err := relayJSON(r.Context(), ds, p, http.MethodGet, path, nil)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		transport.WriteJSON(w, status, out)
		return
	}
	relayStream(w, r, ds, p, path)
}

// relayJSON sends one request to idp-host for the member and returns idp-host's answer as the
// workspace's own: the status and the JSON body pass through; a refusal keeps idp-host's code and message
// (they describe the run: NOT_FOUND, CONFLICT, INVALID) but not its address. 401 from idp-host is not the
// member's session: it is the workspace's credential that was refused.
func relayJSON(ctx context.Context, ds *dispatch.Downstream, p authorization.Principal, method, path string, body any) (int, json.RawMessage, error) {
	token, err := ds.Bearer(p)
	if err != nil {
		return 0, nil, transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", "no credential for idp-host: "+err.Error())
	}
	timeout := ds.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(cctx, method, strings.TrimRight(ds.Endpoint, "/")+path, rd)
	if err != nil {
		return 0, nil, transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", err.Error())
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	hc := ds.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", "idp-host is unreachable")
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, nil, hostProblem(resp.StatusCode, answer)
	}
	if len(bytes.TrimSpace(answer)) == 0 {
		answer = []byte("null")
	}
	if !json.Valid(answer) {
		return 0, nil, transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", "idp-host answered something that is not JSON")
	}
	return resp.StatusCode, json.RawMessage(answer), nil
}

// hostProblem is the workspace's problem for idp-host's refusal.
func hostProblem(status int, answer []byte) error {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(answer, &e)
	code, msg := e.Error.Code, e.Error.Message
	if code == "" {
		code, msg = e.Code, e.Message
	}
	switch {
	case status >= 500:
		return transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", "idp-host failed ("+strconv.Itoa(status)+")")
	case status == http.StatusUnauthorized:
		return transport.Fail(http.StatusForbidden, "FORBIDDEN", "idp-host did not accept the workspace's credential for you")
	}
	if code == "" {
		code = "HTTP_" + strconv.Itoa(status)
	}
	if msg == "" {
		msg = "idp-host refused the request"
	}
	return transport.Fail(status, code, msg)
}

// relayStream copies idp-host's event stream to the client line by line, flushing each line so that text
// arrives as idp-host writes it; `after` and Last-Event-ID resume from an event id. The client's departure
// ends the upstream request through the context.
func relayStream(w http.ResponseWriter, r *http.Request, ds *dispatch.Downstream, p authorization.Principal, path string) {
	token, err := ds.Bearer(p)
	if err != nil {
		transport.WriteProblem(w, transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", "no credential for idp-host: "+err.Error()))
		return
	}
	u := strings.TrimRight(ds.Endpoint, "/") + path
	if after := r.URL.Query().Get("after"); after != "" {
		u += "?after=" + url.QueryEscape(after)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
	if err != nil {
		transport.WriteProblem(w, transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", err.Error()))
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		req.Header.Set("Last-Event-ID", id)
	}
	hc := ds.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		transport.WriteProblem(w, transport.Fail(http.StatusBadGateway, "TARGET_UNAVAILABLE", "idp-host is unreachable"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		transport.WriteProblem(w, hostProblem(resp.StatusCode, answer))
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) // a stream outlives the server's write timeout
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if _, err := w.Write(sc.Bytes()); err != nil {
			return
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
}
