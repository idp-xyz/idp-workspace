package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/workspace"
)

func host(t *testing.T, events ...string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/v1/runs/run_1/events" || r.Header.Get("Accept") != "text/event-stream" {
			http.Error(w, "no", 403)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, e := range events {
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", i+1, e)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRead(t *testing.T) {
	r := Run{RunID: "run_1", Token: "tok"}
	srv := host(t, `{"type":"RUN_STARTED"}`, `{"type":"TEXT_MESSAGE_START","messageId":"m1"}`, `{"type":"TEXT_MESSAGE_CONTENT","messageId":"m1","delta":"Thinking"}`,
		`{"type":"TEXT_MESSAGE_END","messageId":"m1"}`, `{"type":"TEXT_MESSAGE_START","messageId":"m2"}`, `{"type":"TEXT_MESSAGE_CONTENT","messageId":"m2","delta":"Order 1001 "}`,
		`{"type":"TEXT_MESSAGE_CONTENT","messageId":"m2","delta":"waits for customs."}`, `{"type":"TEXT_MESSAGE_END","messageId":"m2"}`, `{"type":"RUN_FINISHED"}`)
	out, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), r, 0, "")
	if err != nil || out.failed || out.waiting != nil || out.text != "Order 1001 waits for customs." || out.last != 9 {
		t.Fatalf("%+v %v", out, err)
	}
	failed := host(t, `{"type":"RUN_STARTED"}`, `{"type":"RUN_ERROR","code":"MODEL_UNAVAILABLE","message":"no provider answered"}`)
	out, err = (&Follower{Endpoint: failed.URL}).read(context.Background(), r, 0, "")
	if err != nil || !out.failed || out.message != "MODEL_UNAVAILABLE no provider answered" {
		t.Fatalf("%+v %v", out, err)
	}
	cut := host(t, `{"type":"RUN_STARTED"}`)
	if _, err := (&Follower{Endpoint: cut.URL}).read(context.Background(), r, 0, ""); err == nil || !strings.Contains(err.Error(), "ended before") {
		t.Fatal(err)
	}
	if _, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), Run{RunID: "run_1", Token: "other"}, 0, ""); err == nil {
		t.Fatal("a refused stream is not an outcome")
	}
}

func TestReadInterrupt(t *testing.T) {
	// idp-host's RUN_FINISHED for a tool call that waits for the owner's confirmation is not an end
	srv := host(t, `{"type":"RUN_STARTED"}`, `{"type":"CUSTOM","name":"idp.host/confirmation.required"}`,
		`{"type":"RUN_FINISHED","outcome":{"type":"interrupt","interrupts":[{"type":"confirmation","toolCallId":"call_1","tool":"minimal__echo","arguments":{"text":"hi"},"expiresAt":"2026-11-06T11:07:21Z"}]}}`)
	out, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), Run{RunID: "run_1", Token: "tok"}, 0, "said before")
	if err != nil || len(out.waiting) != 1 || out.waiting[0].Tool != "minimal__echo" || !out.waiting[0].ExpiresAt.Equal(time.Date(2026, 11, 6, 11, 7, 21, 0, time.UTC)) ||
		out.last != 3 || out.text != "said before" {
		t.Fatalf("%+v %v", out, err)
	}
	for raw, waits := range map[string]bool{`{"type":"success"}`: false, `{"type":"interrupt"}`: true, `"interrupt"`: true, `"success"`: false, `null`: false, ``: false} {
		if got := interrupted(json.RawMessage(raw)) != nil; got != waits {
			t.Errorf("interrupted(%s) = %v", raw, got)
		}
	}
}

// resumable is a fake idp-host event stream: the first request gets first (ids 1..n, none with
// noIDs); a request that resumes after an event gets rest (ids n+1..) once release is called.
type resumable struct {
	srv    *httptest.Server
	gate   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	afters []string
}

func newResumable(t *testing.T, noIDs bool, first, rest []string) *resumable {
	s := &resumable{gate: make(chan struct{})}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after")
		s.mu.Lock()
		s.afters = append(s.afters, after)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		events, base := first, 0
		if after != "" {
			w.(http.Flusher).Flush()
			select {
			case <-s.gate:
			case <-r.Context().Done():
				return
			}
			events, base = rest, len(first)
		}
		for i, e := range events {
			if noIDs {
				fmt.Fprintf(w, "data: %s\n\n", e)
			} else {
				fmt.Fprintf(w, "id: %d\ndata: %s\n\n", base+i+1, e)
			}
		}
	}))
	t.Cleanup(func() { s.release(); s.srv.Close() })
	return s
}

func (s *resumable) release() { s.once.Do(func() { close(s.gate) }) }

func (s *resumable) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.afters...)
}

// items records what the follower puts into the inbox.
type items struct {
	mu  sync.Mutex
	put []postgres.ItemInput
}

func (s *items) PutItem(_ context.Context, _, _, _ string, in postgres.ItemInput) (*workspace.InboxItem, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.put = append(s.put, in)
	return &workspace.InboxItem{}, false, nil
}

func (s *items) all() []postgres.ItemInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]postgres.ItemInput(nil), s.put...)
}

func paused(expires time.Time) []string {
	return []string{`{"type":"RUN_STARTED"}`, `{"type":"CUSTOM","name":"idp.host/confirmation.required"}`,
		fmt.Sprintf(`{"type":"RUN_FINISHED","outcome":{"type":"interrupt","interrupts":[{"type":"confirmation","toolCallId":"call_1","tool":"minimal__echo","arguments":{"text":"hi"},"expiresAt":%q}]}}`,
			expires.Format(time.RFC3339Nano))}
}

func follow(t *testing.T, s *resumable, maxWait time.Duration) (*items, <-chan struct{}) {
	t.Helper()
	store := &items{}
	done := make(chan struct{})
	r := Run{Tenant: "t1", Workspace: "w1", Subject: "bob", Token: "tok", RunID: "run_1", Title: "Ask the order assistant"}
	go func() {
		(&Follower{Store: store, Endpoint: s.srv.URL, MaxWait: maxWait}).Follow(r)
		close(done)
	}()
	return store, done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func ended(done <-chan struct{}) func() bool {
	return func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

func TestFollowAwaitingConfirmation(t *testing.T) {
	// D18: the item says the run awaits confirmation and when the request expires; the follower reads
	// on after the interruption, past MaxWait, and puts the real result once the run ends
	start := time.Now()
	expires := start.Add(2 * time.Second)
	s := newResumable(t, false, paused(expires), []string{`{"type":"CUSTOM","name":"idp.host/confirmation.decided"}`, `{"type":"RUN_STARTED"}`,
		`{"type":"TOOL_CALL_RESULT","toolCallId":"call_1","content":"hi"}`, `{"type":"TEXT_MESSAGE_START","messageId":"m1"}`,
		`{"type":"TEXT_MESSAGE_CONTENT","messageId":"m1","delta":"Result: hi"}`, `{"type":"TEXT_MESSAGE_END","messageId":"m1"}`,
		`{"type":"RUN_FINISHED","outcome":{"type":"success"}}`})
	store, done := follow(t, s, 200*time.Millisecond)
	waitFor(t, "the awaiting item", func() bool { return len(store.all()) == 1 })
	got := store.all()[0]
	if got.Title != "Ask the order assistant — awaiting confirmation" || got.ExternalRef != "run:run_1" || !strings.Contains(got.Summary, "minimal__echo") ||
		!strings.Contains(got.Summary, "cannot be confirmed in the Workspace") || !strings.Contains(got.Summary, expires.UTC().Format("2006-01-02 15:04:05")+" UTC") ||
		got.DueAt == nil || !got.DueAt.Equal(expires) {
		t.Fatalf("%+v", got)
	}
	waitFor(t, "the resumed stream", func() bool { return len(s.requests()) == 2 })
	time.Sleep(time.Until(start.Add(400 * time.Millisecond)))
	select {
	case <-done:
		t.Fatal("the follower gave up at MaxWait while the run waited for the decision")
	default:
	}
	s.release()
	waitFor(t, "the follower to end", ended(done))
	all := store.all()
	if final := all[len(all)-1]; len(all) != 2 || final.Title != "Ask the order assistant — finished" || final.Summary != "Result: hi" || final.DueAt != nil {
		t.Fatalf("%+v", all)
	}
	if got := s.requests(); got[0] != "" || got[1] != "3" {
		t.Fatalf("event requests after: %v", got)
	}
}

func TestFollowFailsAfterConfirmation(t *testing.T) {
	// a confirmation after idp-host's run deadline: the tool runs, then the run fails; the item says failed
	s := newResumable(t, false, paused(time.Now().Add(2*time.Minute)), []string{`{"type":"CUSTOM","name":"idp.host/confirmation.decided"}`,
		`{"type":"RUN_STARTED"}`, `{"type":"TOOL_CALL_RESULT","toolCallId":"call_1","content":"hi"}`,
		`{"type":"RUN_ERROR","code":"RUN_TIMEOUT","message":"the run exceeded its time limit"}`})
	s.release()
	store, done := follow(t, s, time.Minute)
	waitFor(t, "the follower to end", ended(done))
	all := store.all()
	if final := all[len(all)-1]; len(all) != 2 || all[0].DueAt == nil || final.Title != "Ask the order assistant — failed" ||
		final.Summary != "RUN_TIMEOUT the run exceeded its time limit" || final.DueAt != nil {
		t.Fatalf("%+v", all)
	}
}

func TestFollowGivesUpAfterExpiry(t *testing.T) {
	// no decision and no end: the follower stops at the request's expiry plus MaxWait, not at MaxWait
	start := time.Now()
	s := newResumable(t, false, paused(start.Add(300*time.Millisecond)), nil)
	store, done := follow(t, s, 100*time.Millisecond)
	waitFor(t, "the follower to end", ended(done))
	if took := time.Since(start); took < 350*time.Millisecond {
		t.Fatalf("gave up after %v, before the request expired", took)
	}
	all := store.all()
	if final := all[len(all)-1]; len(all) != 2 || final.Title != "Ask the order assistant — result unavailable" || !strings.Contains(final.Summary, "deadline") {
		t.Fatalf("%+v", all)
	}
}

func TestFollowNeedsEventIDsToResume(t *testing.T) {
	// a stream without event ids cannot be resumed after the interruption: say so instead of guessing
	s := newResumable(t, true, paused(time.Now().Add(time.Minute)), nil)
	store, done := follow(t, s, time.Minute)
	waitFor(t, "the follower to end", ended(done))
	all := store.all()
	if len(all) != 1 || all[0].Title != "Ask the order assistant — result unavailable" || !strings.Contains(all[0].Summary, "cannot be resumed") {
		t.Fatalf("%+v", all)
	}
}
