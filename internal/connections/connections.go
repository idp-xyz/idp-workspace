// Package connections is the Workspace's side of the connected services (ADR-0001 D30): the member connects their own
// account at a third party once, and their agents use its MCP tools as them. idp-mcp-bridge holds the tokens and the
// OAuth flow; the Workspace is where the member is. It
//
//   - lists the connections with the member's state, calling the bridge's internal API with the member's own token,
//     exchanged for the bridge's audience (the bridge names the member from that token and from nothing else);
//   - starts a connect: asks the bridge for the authorization address, records for THIS member that they started it (the
//     hash of the address's state, the connection, a return path of the Workspace's own), and only then lets the browser go;
//   - finishes it: the third party redirects the browser to the Workspace's callback page; the page hands the parameters
//     here with the member's token; the state must be one THIS member started, still unused and unexpired, or the bridge
//     is not called at all; the parameters pass on exactly as received, for the recorded connection;
//   - serves the connections' client metadata documents, but only a document that names the address it is served at.
//
// It never logs a code or a state, never shows anything a third party said (the answers carry fixed codes and the page
// translates them), and never follows an address it was not given by the bridge over https.
package connections

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
	"net/url"
	"strings"
	"sync"
	"time"

	"idp-workspace/internal/authorization"
	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/transport"
	"idp-workspace/internal/workspace"
)

const (
	// CallbackPath is the Workspace's OAuth callback page: the bridge's IDP_BRIDGE_REDIRECT_URI is PublicURL + CallbackPath.
	CallbackPath = "/oauth/callback"
	// ClientPrefix and ClientSuffix make a connection's client metadata document address: PublicURL/oauth/client/<id>.json.
	ClientPrefix = "/oauth/client/"
	ClientSuffix = ".json"
	// DefaultReturnPath is where a connect comes back to when the caller names no path.
	DefaultReturnPath = "/connections"
	// PendingTTL is how long a started connect may be finished in: the bridge's own state lives 10 minutes too.
	PendingTTL = 10 * time.Minute

	maxListBody     = 1 << 20
	maxSmallBody    = 16 << 10
	maxDocument     = 64 << 10
	maxParam        = 4096
	maxReturnPath   = 200
	maxAuthorizeURL = 4096
	maxConnections  = 200
	docTTL          = time.Minute
	docMissTTL      = 15 * time.Second
	docCacheEntries = 256
)

// PendingStore keeps the connects members started (postgres.Store).
type PendingStore interface {
	PutPendingConnect(ctx context.Context, tenant, subject string, p workspace.PendingConnect) error
	// TakePendingConnect consumes it: nil when the member has none with this state hash.
	TakePendingConnect(ctx context.Context, tenant, subject, stateHash string) (*workspace.PendingConnect, error)
}

// Service is the connected-services feature; a nil *Service is the feature off.
type Service struct {
	// Bridge is the bridge's internal API: auth exchange, the audience the bridge expects.
	Bridge *dispatch.Downstream
	// PublicURL is the Workspace's public base URL (scheme and host, no trailing slash).
	PublicURL string
	// TestOrigins are host:port pairs that may be plain http (acceptance only).
	TestOrigins []string
	Pending     PendingStore
	// Audit records what a member did to a connection (never a code or state); optional.
	Audit func(ctx context.Context, p authorization.Principal, action, resource string, detail map[string]any) error
	Log   *slog.Logger
	Now   func() time.Time

	mu   sync.Mutex
	docs map[string]docEntry
}

type docEntry struct {
	body  []byte // nil: the bridge has no such document (or it was not acceptable)
	until time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// CallbackURL is the Workspace's callback page, the address the bridge is configured with.
func (s *Service) CallbackURL() string { return s.PublicURL + CallbackPath }

// ClientURL is the address of a connection's client metadata document: the client_id the bridge puts in it.
func (s *Service) ClientURL(id string) string { return s.PublicURL + ClientPrefix + id + ClientSuffix }

// ---- the bridge's answers, in the Workspace's fixed words ----

// The fixed sets of reasons the bridge may give; a reason outside its set is not passed on (the message is then the
// code's own sentence). Nothing of what a third party said is among them.
var (
	refusedReasons = set("issuer_mismatch", "issuer_not_listed", "resource_mismatch", "resource_metadata_origin", "pkce_unsupported", "client_identity_unsupported", "address_refused", "redirect_refused", "metadata_invalid")
	failedReasons  = set("iss_mismatch", "iss_missing", "invalid_grant", "token_response_invalid", "token_endpoint_error")
	deniedReasons  = set("access_denied", "invalid_scope", "server_error", "temporarily_unavailable", "invalid_request", "unauthorized_client", "unsupported_response_type", "authorization_failed")
)

func set(v ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range v {
		m[x] = true
	}
	return m
}

// Problem codes of this feature. The message of the three that carry a reason is that reason's fixed code (or a fixed sentence
// when the bridge gave none): the page puts it in words.
const (
	CodeUnavailable    = "CONNECTIONS_UNAVAILABLE"
	CodeStateInvalid   = "CONNECTION_STATE_INVALID"
	CodeStateExpired   = "CONNECTION_STATE_EXPIRED"
	CodeRequestInvalid = "CONNECTION_REQUEST_INVALID"
	CodeFailed         = "CONNECTION_AUTHORIZATION_FAILED"
	CodeDenied         = "CONNECTION_AUTHORIZATION_DENIED"
	CodeRefused        = "CONNECTION_REFUSED"
	CodeChanged        = "CONNECTION_CONFIG_CHANGED"
	CodeServer         = "CONNECTION_SERVER_UNAVAILABLE"
)

// bridgeProblem is what the bridge answered with a status that is not a success, translated: the HTTP status the Workspace
// answers with, a code and a message, all fixed.
func bridgeProblem(status int, code, reason string) *transport.Problem {
	with := func(known map[string]bool, fallback string) string {
		if known[reason] {
			return reason
		}
		return fallback
	}
	switch {
	case status == 404 && code == "not_found":
		return transport.Fail(404, "NOT_FOUND", "no such connection")
	case status == 403 && code == "forbidden":
		return transport.Fail(403, "FORBIDDEN", "the connection's reviewer account is for operators")
	case status == 400 && code == "invalid_state":
		return transport.Fail(400, CodeStateInvalid, "this authorization is not one you started, or was used already")
	case status == 400 && code == "state_expired":
		return transport.Fail(400, CodeStateExpired, "this authorization expired")
	case status == 400 && code == "invalid_request":
		return transport.Fail(400, CodeRequestInvalid, "the authorization's answer was not complete")
	case status == 400 && code == "authorization_failed":
		return transport.Fail(400, CodeFailed, with(failedReasons, "authorization_failed"))
	case status == 400 && code == "authorization_denied":
		return transport.Fail(400, CodeDenied, with(deniedReasons, "authorization_failed"))
	case status == 409 && code == "authorization_server_refused":
		return transport.Fail(409, CodeRefused, with(refusedReasons, "refused"))
	case status == 409 && code == "configuration_changed":
		return transport.Fail(409, CodeChanged, "the connection's configuration changed; start again")
	case status == 502 && code == "authorization_server_unavailable":
		return transport.Fail(502, CodeServer, "the service's authorization server did not answer")
	}
	return transport.Fail(502, CodeUnavailable, "connected services are not available")
}

// call sends one request to the bridge's internal API as the member and decodes a 2xx answer into out (nil: not read). A
// refusal is returned as the Workspace's problem for it; the bridge's own words never get further than this function.
func (s *Service) call(ctx context.Context, p authorization.Principal, method, path string, body, out any, limit int64) error {
	token, err := s.Bridge.Bearer(p)
	if err != nil {
		s.log().Warn("connected services: no credential for the bridge", "error", err)
		return transport.Fail(502, CodeUnavailable, "connected services are not available")
	}
	timeout := s.Bridge.Timeout
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
	req, err := http.NewRequestWithContext(cctx, method, strings.TrimRight(s.Bridge.Endpoint, "/")+path, rd)
	if err != nil {
		return transport.Fail(502, CodeUnavailable, "connected services are not available")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	hc := s.Bridge.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		s.log().Warn("connected services: the bridge did not answer")
		return transport.Fail(502, CodeUnavailable, "connected services are not available")
	}
	defer resp.Body.Close()
	raw, err := transport.ReadBounded(resp.Body, limit)
	if err != nil {
		return transport.Fail(502, CodeUnavailable, "connected services are not available")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &e)
		pb := bridgeProblem(resp.StatusCode, e.Error, e.Reason)
		if pb.Code == CodeUnavailable {
			// only a fixed thing is logged: the status (never the body, which the bridge keeps free of secrets but is not ours to log)
			s.log().Warn("connected services: the bridge refused", "status", resp.StatusCode)
		}
		return pb
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return transport.Fail(502, CodeUnavailable, "connected services are not available")
		}
	}
	return nil
}

func validID(id string) bool { return workspace.ConnectionIDPattern.MatchString(id) }

var errNoSuchConnection = transport.Fail(404, "NOT_FOUND", "no such connection")

// ---- list ----

// List is the member's connected services, as the bridge reports them, each field checked again.
func (s *Service) List(ctx context.Context, p authorization.Principal) (*workspace.ConnectionList, error) {
	var in struct {
		Connections []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Status   string `json:"status"`
			Reason   string `json:"reason"`
			Scope    string `json:"scope"`
			Reviewer *struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
				Scope  string `json:"scope"`
			} `json:"reviewer"`
		} `json:"connections"`
	}
	if err := s.call(ctx, p, http.MethodGet, "/internal/v1/connections", nil, &in, maxListBody); err != nil {
		return nil, err
	}
	out := &workspace.ConnectionList{Connections: []workspace.Connection{}}
	for _, c := range in.Connections {
		if len(out.Connections) >= maxConnections {
			break
		}
		if !validID(c.ID) || !workspace.ConnectionStatuses[c.Status] {
			continue // not shown: the page lists what it can name
		}
		x := workspace.Connection{ID: c.ID, Title: cleanTitle(c.Title, c.ID), Status: c.Status, Reason: reasonOf(c.Status, c.Reason), Scope: cleanScope(c.Scope)}
		if r := c.Reviewer; r != nil && workspace.ConnectionStatuses[r.Status] {
			x.Reviewer = &workspace.ConnectionAccount{Status: r.Status, Reason: reasonOf(r.Status, r.Reason), Scope: cleanScope(r.Scope)}
		}
		out.Connections = append(out.Connections, x)
	}
	return out, nil
}

func reasonOf(status, reason string) string {
	if status == workspace.ConnectionReauthorize && workspace.ConnectionReasons[reason] {
		return reason
	}
	return ""
}

func cleanTitle(title, id string) string {
	if t := workspace.CleanSourceTitle(title); t != "" {
		if r := []rune(t); len(r) > 100 {
			return string(r[:100])
		}
		return t
	}
	return id
}

// cleanScope keeps a scope string to space-separated printable ASCII tokens, at most 500 characters.
func cleanScope(s string) string {
	var out []string
	n := 0
	for _, f := range strings.Fields(s) {
		ok := len(f) <= 200
		for i := 0; ok && i < len(f); i++ {
			ok = f[i] > 0x20 && f[i] < 0x7f
		}
		if !ok {
			continue
		}
		if n += len(f) + 1; n > 500 {
			break
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// ---- connect ----

// ValidReturnPath reports whether s is a relative path of the Workspace itself: it starts with one slash, no second slash or
// backslash after it, nothing but visible ASCII (everything else percent-encoded), no scheme, host or user info once parsed,
// and not the callback page (that would be a loop). A path that fails is never recorded and never gone back to.
func ValidReturnPath(s string) bool {
	if s == "" || len(s) > maxReturnPath || s[0] != '/' {
		return false
	}
	if len(s) > 1 && (s[1] == '/' || s[1] == '\\') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] >= 0x7f || s[i] == '\\' {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return false
	}
	// what the percent-encoding says must pass the same checks: it is no way to a second slash, a backslash or a control character
	if d := u.Path; strings.HasPrefix(d, "//") || strings.HasPrefix(d, "/\\") || strings.ContainsAny(d, "\\") || strings.IndexFunc(d, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return false
	}
	return !strings.HasPrefix(u.Path, CallbackPath)
}

// plainHTTPAllowed reports whether host (host:port) is a test origin.
func (s *Service) plainHTTPAllowed(host string) bool {
	for _, o := range s.TestOrigins {
		if o == host {
			return true
		}
	}
	return false
}

// authorizationURL checks the address the bridge returned and takes the hash of its state. The browser is sent only to an
// https address (an http one only for a test origin), with a host, no user info and no fragment, and with exactly one state.
func (s *Service) authorizationURL(raw string) (hash string, ok bool) {
	if raw == "" || len(raw) > maxAuthorizeURL {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 || raw[i] >= 0x7f {
			return "", false
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && s.plainHTTPAllowed(u.Host)) {
		return "", false
	}
	st := u.Query()["state"]
	if len(st) != 1 || len(st[0]) < 16 || len(st[0]) > 512 {
		return "", false
	}
	return hashOf(st[0]), true
}

func hashOf(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// Connect starts authorizing a connection for the member (their own account, or the reviewer account) and returns the address
// of the third party's authorization page. The member's pending connect is recorded before the address is returned.
func (s *Service) Connect(ctx context.Context, p authorization.Principal, id string, reviewer bool, returnPath string) (*workspace.ConnectionAuthorization, error) {
	if !validID(id) {
		return nil, errNoSuchConnection
	}
	if returnPath == "" {
		returnPath = DefaultReturnPath
	}
	if !ValidReturnPath(returnPath) {
		return nil, transport.Fail(422, "INVALID", "returnPath must be a relative path of the Workspace")
	}
	path := "/internal/v1/connections/" + id
	if reviewer {
		path += "/reviewer"
	}
	var a struct {
		URL string `json:"url"`
	}
	if err := s.call(ctx, p, http.MethodPost, path+"/authorization", nil, &a, maxSmallBody); err != nil {
		return nil, err
	}
	hash, ok := s.authorizationURL(a.URL)
	if !ok {
		// the address itself is not logged: it carries the state
		s.log().Warn("connected services: the authorization address was not acceptable", "connection", id)
		return nil, transport.Fail(502, CodeUnavailable, "connected services are not available")
	}
	rec := workspace.PendingConnect{StateHash: hash, Connection: id, Reviewer: reviewer, ReturnPath: returnPath, ExpiresAt: s.now().Add(PendingTTL)}
	if err := s.Pending.PutPendingConnect(ctx, p.Tenant, p.Subject, rec); err != nil {
		return nil, err
	}
	return &workspace.ConnectionAuthorization{URL: a.URL}, nil
}

// ---- callback ----

// Callback is what the third party's redirect carried, exactly as the browser received it.
type Callback struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Iss   string `json:"iss"`
	Error string `json:"error"`
}

// Finish completes a connect. The state must be one this member started (same tenant and subject), unused and unexpired; it is
// consumed first, so a second use fails whatever the first one came to. Anything else is refused here, without a call to the
// bridge (which checks the same again: this is the Workspace's half of "the person who authorizes is the one who started").
func (s *Service) Finish(ctx context.Context, p authorization.Principal, c Callback) (*workspace.ConnectionResult, error) {
	if len(c.Code) > maxParam || len(c.State) > maxParam || len(c.Iss) > maxParam || len(c.Error) > maxParam {
		return nil, transport.Fail(422, "INVALID", "the callback's parameters are too long")
	}
	rec, err := s.Pending.TakePendingConnect(ctx, p.Tenant, p.Subject, hashOf(c.State))
	if err != nil {
		return nil, err
	}
	if rec == nil || !validID(rec.Connection) {
		return nil, bridgeProblem(400, "invalid_state", "")
	}
	if !s.now().Before(rec.ExpiresAt) {
		return nil, bridgeProblem(400, "state_expired", "")
	}
	if err := s.call(ctx, p, http.MethodPost, "/internal/v1/connections/"+rec.Connection+"/callback", c, nil, maxSmallBody); err != nil {
		return nil, err
	}
	s.audit(ctx, p, "connection.connect", rec.Connection, rec.Reviewer)
	return &workspace.ConnectionResult{Connection: rec.Connection, Status: workspace.ConnectionConnected, ReturnPath: rec.ReturnPath, Reviewer: rec.Reviewer}, nil
}

func (s *Service) audit(ctx context.Context, p authorization.Principal, action, id string, reviewer bool) {
	if s.Audit == nil {
		return
	}
	if err := s.Audit(ctx, p, action, id, map[string]any{"reviewer": reviewer}); err != nil {
		s.log().Warn("connected services: the audit record was not written", "action", action, "connection", id)
	}
}

// Disconnect deletes the member's grant (or the reviewer account's) at the bridge.
func (s *Service) Disconnect(ctx context.Context, p authorization.Principal, id string, reviewer bool) error {
	if !validID(id) {
		return errNoSuchConnection
	}
	path := "/internal/v1/connections/" + id
	if reviewer {
		path += "/reviewer"
	}
	if err := s.call(ctx, p, http.MethodDelete, path+"/grant", nil, nil, maxSmallBody); err != nil {
		return err
	}
	s.audit(ctx, p, "connection.disconnect", id, reviewer)
	return nil
}

// ---- client metadata documents ----

// ClientDocument is the client metadata document of a connection, from the bridge, to be served at ClientURL(id) and only
// there: the document's own client_id must be that address, character for character, or there is no document. ok is false
// for every other case (no such connection, the bridge failed, the document is large, is not an object, or names another
// address). The result is the bridge's document re-encoded from the parsed object (one value per key, whatever the bridge's
// bytes were) and is cached briefly, a miss for less time.
func (s *Service) ClientDocument(ctx context.Context, id string) (doc []byte, ok bool) {
	if !validID(id) {
		return nil, false
	}
	now := s.now()
	s.mu.Lock()
	if e, hit := s.docs[id]; hit && now.Before(e.until) {
		s.mu.Unlock()
		return e.body, e.body != nil
	}
	s.mu.Unlock()
	body := s.fetchDocument(ctx, id)
	ttl := docMissTTL
	if body != nil {
		ttl = docTTL
	}
	s.mu.Lock()
	if s.docs == nil {
		s.docs = map[string]docEntry{}
	}
	for k, e := range s.docs { // what has expired goes, so the cache stays small
		if !now.Before(e.until) {
			delete(s.docs, k)
		}
	}
	if len(s.docs) < docCacheEntries {
		s.docs[id] = docEntry{body: body, until: now.Add(ttl)}
	}
	s.mu.Unlock()
	return body, body != nil
}

func (s *Service) fetchDocument(ctx context.Context, id string) []byte {
	timeout := s.Bridge.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// no credential: the document is public by design and holds nothing secret (the bridge's D22)
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, strings.TrimRight(s.Bridge.Endpoint, "/")+"/internal/v1/connections/"+id+"/client-metadata", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	hc := s.Bridge.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	raw, err := transport.ReadBounded(resp.Body, maxDocument)
	if err != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) { // nothing after the object
		return nil
	}
	if cid, _ := obj["client_id"].(string); cid != s.ClientURL(id) {
		s.log().Warn("connected services: a client metadata document names another address; not served", "connection", id)
		return nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}
