// Package connectionstest holds the doubles the tests of the connected services use: idp-mcp-bridge's internal API (it records
// what it receives and checks that the bearer is the member's own token exchanged for ITS audience, as the real bridge does)
// with the token endpoint that does the exchange, and a pending-connect store in memory. Nothing here is used by the service.
package connectionstest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/workspace"
)

// Audience is what the bridge double accepts tokens for.
const Audience = "idp-mcp-bridge"

// Call is one request the bridge double received.
type Call struct {
	Method, Path  string
	Authorization string
	// Member is the member the bearer names (what the double read from the exchanged token); "" when the bearer is not good.
	Member string
	Body   string
}

// Bridge is the double: STS is the token endpoint (RFC 8693) and Server the internal API. Set the handlers before the test
// makes its calls; every request is recorded in Calls, answered 401 unless its bearer is an exchanged token for Audience.
type Bridge struct {
	Server, STS *httptest.Server

	mu       sync.Mutex
	calls    []Call
	exchange []string // the audiences asked of the token endpoint

	// Respond answers an authenticated request: status and body ("" = no body). Default: 404 {"error":"not_found"}.
	Respond func(c Call) (int, string)
	// Metadata answers the public client-metadata route: connection id -> document (absent: 404).
	Metadata map[string]string
}

// New starts the doubles; they stop with the test.
func New(t *testing.T) *Bridge {
	t.Helper()
	b := &Bridge{Metadata: map[string]string{}}
	b.STS = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		b.mu.Lock()
		b.exchange = append(b.exchange, r.Form.Get("audience"))
		b.mu.Unlock()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || r.Form.Get("client_id") != "idp-workspace" || r.Form.Get("client_secret") != "s3cret" || r.Form.Get("subject_token") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_request"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// the exchanged token names its audience and the member it was exchanged for, as the real one does in its claims
		_, _ = fmt.Fprintf(w, `{"access_token":"exchanged.%s.%s","expires_in":300}`, r.Form.Get("audience"), r.Form.Get("subject_token"))
	}))
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		c := Call{Method: r.Method, Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: string(raw)}
		if id, ok := strings.CutPrefix(r.URL.Path, "/internal/v1/connections/"); ok && strings.HasSuffix(id, "/client-metadata") {
			b.record(c) // public: no token
			doc, ok := b.Metadata[strings.TrimSuffix(id, "/client-metadata")]
			if !ok {
				write(w, 404, `{"error":"not_found"}`)
				return
			}
			write(w, 200, doc)
			return
		}
		if bearer, ok := strings.CutPrefix(c.Authorization, "Bearer exchanged."+Audience+"."); ok && bearer != "" {
			c.Member = bearer
		}
		b.record(c)
		if c.Member == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			write(w, 401, `{"error":"unauthenticated"}`)
			return
		}
		status, body := 404, `{"error":"not_found"}`
		if b.Respond != nil {
			status, body = b.Respond(c)
		}
		write(w, status, body)
	}))
	t.Cleanup(func() { b.Server.Close(); b.STS.Close() })
	return b
}

func write(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != "" {
		_, _ = io.WriteString(w, body)
	}
}

func (b *Bridge) record(c Call) {
	b.mu.Lock()
	b.calls = append(b.calls, c)
	b.mu.Unlock()
}

// Calls are the requests received so far.
func (b *Bridge) Calls() []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Call(nil), b.calls...)
}

// Audiences are the audiences the token endpoint was asked to exchange for.
func (b *Bridge) Audiences() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.exchange...)
}

// Downstream is the Workspace's client of the double: the member's token exchanged for Audience at STS.
func (b *Bridge) Downstream() *dispatch.Downstream {
	return &dispatch.Downstream{Endpoint: b.Server.URL, Auth: dispatch.AuthExchange, HTTP: b.Server.Client(), Exchange: &dispatch.Exchanger{
		TokenURL: b.STS.URL, ClientID: "idp-workspace", ClientSecret: func() (string, error) { return "s3cret", nil }, Audience: Audience, HTTP: b.STS.Client()}}
}

// Pending is the pending-connect store in memory: the semantics of the PostgreSQL one (keyed by tenant, subject and state hash;
// taking consumes it; a second put of the same state conflicts).
type Pending struct {
	mu   sync.Mutex
	rows map[string]workspace.PendingConnect
}

func key(tenant, subject, hash string) string { return tenant + "\x00" + subject + "\x00" + hash }

// PutPendingConnect implements connections.PendingStore.
func (p *Pending) PutPendingConnect(_ context.Context, tenant, subject string, rec workspace.PendingConnect) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rows == nil {
		p.rows = map[string]workspace.PendingConnect{}
	}
	if _, dup := p.rows[key(tenant, subject, rec.StateHash)]; dup {
		return fmt.Errorf("duplicate")
	}
	p.rows[key(tenant, subject, rec.StateHash)] = rec
	return nil
}

// TakePendingConnect implements connections.PendingStore.
func (p *Pending) TakePendingConnect(_ context.Context, tenant, subject, hash string) (*workspace.PendingConnect, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec, ok := p.rows[key(tenant, subject, hash)]
	if !ok {
		return nil, nil
	}
	delete(p.rows, key(tenant, subject, hash))
	return &rec, nil
}

// Len is how many connects are pending; Get reads one without consuming it.
func (p *Pending) Len() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.rows) }

// Get returns the record of (tenant, subject, hash) without consuming it.
func (p *Pending) Get(tenant, subject, hash string) (workspace.PendingConnect, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.rows[key(tenant, subject, hash)]
	return r, ok
}

// Clock is a clock a test moves.
type Clock struct {
	mu sync.Mutex
	T  time.Time
}

// Now returns the clock's time.
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.T }

// Advance moves the clock.
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); c.T = c.T.Add(d); c.mu.Unlock() }

// JSON is a helper for the doubles' bodies.
func JSON(v any) string { b, _ := json.Marshal(v); return string(b) }
