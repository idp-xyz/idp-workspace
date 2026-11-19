package runs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
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
	out, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), r, 0, "", nil)
	if err != nil || out.failed || out.waiting != nil || out.text != "Order 1001 waits for customs." || out.last != 9 {
		t.Fatalf("%+v %v", out, err)
	}
	// D23: the files a run delivered are named on its item
	files := host(t, `{"type":"RUN_STARTED"}`, `{"type":"CUSTOM","name":"idp.host/file.delivered","value":{"toolCallId":"c1","ref":"f1","name":"报价单-修订.docx","size":10}}`,
		`{"type":"CUSTOM","name":"idp.host/model.usage","value":{"name":"not a file"}}`, `{"type":"RUN_FINISHED"}`)
	out, err = (&Follower{Endpoint: files.URL}).read(context.Background(), r, 0, "", []string{"earlier.xlsx"})
	if err != nil || len(out.files) != 2 || out.files[0] != "earlier.xlsx" || out.files[1] != "报价单-修订.docx" {
		t.Fatalf("%+v %v", out, err)
	}
	failed := host(t, `{"type":"RUN_STARTED"}`, `{"type":"RUN_ERROR","code":"MODEL_UNAVAILABLE","message":"no provider answered"}`)
	out, err = (&Follower{Endpoint: failed.URL}).read(context.Background(), r, 0, "", nil)
	if err != nil || !out.failed || out.message != "MODEL_UNAVAILABLE no provider answered" {
		t.Fatalf("%+v %v", out, err)
	}
	cut := host(t, `{"type":"RUN_STARTED"}`)
	if _, err := (&Follower{Endpoint: cut.URL}).read(context.Background(), r, 0, "", nil); err == nil || !strings.Contains(err.Error(), "ended before") {
		t.Fatal(err)
	}
	if _, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), Run{RunID: "run_1", Token: "other"}, 0, "", nil); err == nil {
		t.Fatal("a refused stream is not an outcome")
	}
}

func TestReadInterrupt(t *testing.T) {
	// idp-host's RUN_FINISHED for a tool call that waits for the owner's confirmation is not an end
	srv := host(t, `{"type":"RUN_STARTED"}`, `{"type":"CUSTOM","name":"idp.host/confirmation.required"}`,
		`{"type":"RUN_FINISHED","outcome":{"type":"interrupt","interrupts":[{"type":"confirmation","toolCallId":"call_1","tool":"minimal__echo","arguments":{"text":"hi"},"expiresAt":"2026-11-06T11:07:21Z"}]}}`)
	out, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), Run{RunID: "run_1", Token: "tok"}, 0, "said before", nil)
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
		if !strings.HasSuffix(r.URL.Path, "/events") { // Follow first reads the run's own deadline (D26); these tests have none
			http.NotFound(w, r)
			return
		}
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

func TestFollowLastsAsLongAsTheRun(t *testing.T) {
	// D26: a run whose own deadline is later than MaxWait is followed past MaxWait, and its real ending
	// reaches the inbox
	start := time.Now()
	deadline := start.Add(600 * time.Millisecond)
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runs/run_1" {
			fmt.Fprintf(w, `{"runId":"run_1","deadlineAt":%q}`, deadline.Format(time.RFC3339Nano))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, "id: 1\ndata: %s\n\n", `{"type":"RUN_ERROR","code":"STEP_LIMIT","message":"the run reached its limit of 120 steps"}`)
	}))
	t.Cleanup(func() { release(); srv.Close() })
	store := &items{}
	done := make(chan struct{})
	go func() {
		(&Follower{Store: store, Endpoint: srv.URL, MaxWait: 150 * time.Millisecond}).Follow(
			Run{Tenant: "t1", Workspace: "w1", Subject: "bob", Token: "tok", RunID: "run_1", Title: "Work on a document"})
		close(done)
	}()
	time.Sleep(350 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the follower gave up at MaxWait while the run's own deadline was still ahead")
	default:
	}
	release()
	waitFor(t, "the follower to end", ended(done))
	all := store.all()
	if final := all[len(all)-1]; len(all) != 1 || final.Title != "Work on a document — failed" || !strings.Contains(final.Summary, "STEP_LIMIT the run reached its limit of 120 steps") {
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

type endedRuns struct {
	mu   sync.Mutex
	runs []string
	keep time.Duration
}

func (e *endedRuns) EndRun(_ context.Context, tenant, run string, keep time.Duration) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runs, e.keep = append(e.runs, tenant+"/"+run), keep
	return nil
}

func (e *endedRuns) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.runs...)
}

// D27: a run that has ended, finished or failed, takes its file tickets with it. One that only waits for its owner has
// not ended, and one the follower lost may still be working: neither ends the tickets.
func TestFollowEndsTheRunsFileTicketsWhenTheRunEnds(t *testing.T) {
	r := Run{Tenant: "t1", Workspace: "w1", Subject: "bob", Token: "tok", RunID: "run_1", Title: "Office"}
	for name, c := range map[string]struct {
		events []string
		ends   bool
	}{
		"finished": {[]string{`{"type":"RUN_STARTED"}`, `{"type":"RUN_FINISHED","outcome":{"type":"success"}}`}, true},
		"failed":   {[]string{`{"type":"RUN_STARTED"}`, `{"type":"RUN_ERROR","code":"STEP_LIMIT","message":"the run reached its limit"}`}, true},
		"lost":     {[]string{`{"type":"RUN_STARTED"}`}, false}, // the stream ends without a terminal event
	} {
		tickets := &endedRuns{}
		(&Follower{Store: &items{}, Endpoint: host(t, c.events...).URL, Tickets: tickets, TicketTTL: 45 * time.Minute}).Follow(r)
		got := tickets.all()
		if c.ends && (len(got) != 1 || got[0] != "t1/run_1" || tickets.keep < 45*time.Minute) {
			t.Errorf("%s: the tickets were ended %v, kept %v", name, got, tickets.keep)
		}
		if !c.ends && len(got) != 0 {
			t.Errorf("%s: the tickets of a run that may still be working were ended", name)
		}
	}
	// waiting for a confirmation is not an end; the real end after it is
	s := newResumable(t, false, paused(time.Now().Add(2*time.Minute)), []string{`{"type":"RUN_STARTED"}`, `{"type":"RUN_FINISHED","outcome":{"type":"success"}}`})
	tickets := &endedRuns{}
	done := make(chan struct{})
	go func() {
		(&Follower{Store: &items{}, Endpoint: s.srv.URL, MaxWait: time.Minute, Tickets: tickets}).Follow(Run{Tenant: "t1", Workspace: "w1", Token: "tok", RunID: "run_1", Title: "Office"})
		close(done)
	}()
	waitFor(t, "the resumed stream", func() bool { return len(s.requests()) == 2 })
	if got := tickets.all(); len(got) != 0 {
		t.Fatalf("the tickets were ended while the run waited for its owner: %v", got)
	}
	s.release()
	waitFor(t, "the follower to end", ended(done))
	if got := tickets.all(); len(got) != 1 {
		t.Fatalf("after the real end: %v", got)
	}
}

// sourcesEvent is the host's idp.host/run.sources event (idp-host D49-9) with the given value.
func sourcesEvent(value string) string {
	return `{"type":"CUSTOM","name":"idp.host/run.sources","value":` + value + `}`
}

const goodSources = `{"sources":[{"n":1,"title":"Customs clearance times","url":"https://port.example/customs","fetchedAt":"2026-11-19T10:00:00Z","sha256":"` +
	"5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d" + `","cited":true},{"n":2,"title":"Carrier notices","url":"https://carrier.example/n","cited":false}],` +
	`"unverifiedCitations":[7],"droppedInvalid":0,"droppedOverLimit":0,"conflicts":0,"checked":true}`

func TestTheFollowerReadsTheSourcesOfTheHostsEventAndWithoutOneThereAreNone(t *testing.T) {
	r := Run{RunID: "run_1", Token: "tok"}
	with := host(t, `{"type":"RUN_STARTED"}`, `{"type":"CUSTOM","name":"idp.host/file.delivered","value":{"name":"a.docx"}}`, sourcesEvent(goodSources), `{"type":"RUN_FINISHED"}`)
	out, err := (&Follower{Endpoint: with.URL}).read(context.Background(), r, 0, "", nil)
	if err != nil || out.sources == nil || len(out.sources.sources) != 2 || out.sources.sources[0].URL != "https://port.example/customs" || !out.sources.sources[0].Cited ||
		fmt.Sprint(out.sources.unverified) != "[7]" || len(out.files) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	without := host(t, `{"type":"RUN_STARTED"}`, `{"type":"CUSTOM","name":"idp.host/model.usage","value":{"sources":[{"n":1}]}}`, `{"type":"RUN_FINISHED"}`)
	if out, err := (&Follower{Endpoint: without.URL}).read(context.Background(), r, 0, "", nil); err != nil || out.sources != nil {
		t.Fatalf("a stream without the event has no sources: %+v %v", out, err)
	}
	nothing := host(t, `{"type":"RUN_STARTED"}`, sourcesEvent(`{"sources":[{"n":0,"title":"t","url":"javascript:alert(1)","cited":false}]}`), `{"type":"RUN_FINISHED"}`)
	if out, err := (&Follower{Endpoint: nothing.URL}).read(context.Background(), r, 0, "", nil); err != nil || out.sources != nil {
		t.Fatalf("an event with nothing valid in it keeps nothing: %+v %v", out, err)
	}
}

func TestAHostileEventIsKeptCleanedOrNotAtAll(t *testing.T) {
	var entries []string
	hostile := []struct{ title, url string }{
		{`<script>alert(1)</script>`, `https://a.example/1`},
		{`<img src=x onerror=alert(1)>`, `https://a.example/2`},
		{`[click here](javascript:alert(1)) **bold**`, `https://a.example/3`},
		{"invoice‮fdp.exe⁦", `https://a.example/4`},
		{`javascript link`, `javascript:alert(document.cookie)`},
		{`JaVaScRiPt link`, `JaVaScRiPt:alert(1)`},
		{`data link`, `data:text/html;base64,PHNjcmlwdD4=`},
		{`user info`, `https://trusted.example@evil.example/`},
		{`vbscript`, `vbscript:msgbox(1)`},
	}
	for i, h := range hostile {
		tb, _ := json.Marshal(h.title)
		ub, _ := json.Marshal(h.url)
		entries = append(entries, fmt.Sprintf(`{"n":%d,"title":%s,"url":%s,"cited":false}`, i+1, tb, ub))
	}
	srv := host(t, sourcesEvent(`{"sources":[`+strings.Join(entries, ",")+`]}`), `{"type":"RUN_FINISHED"}`)
	out, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), Run{RunID: "run_1", Token: "tok"}, 0, "", nil)
	if err != nil || out.sources == nil || len(out.sources.sources) != 4 {
		t.Fatalf("only the four with an http address stay: %+v %v", out, err)
	}
	for _, s := range out.sources.sources {
		if strings.ContainsAny(s.Title, "‮⁦") || !strings.HasPrefix(s.URL, "https://a.example/") {
			t.Errorf("not cleaned: %+v", s)
		}
	}
	// markup is data: it is kept as text and escaped where it is shown (apps/web/test/Inbox.test.tsx)
	if out.sources.sources[0].Title != `<script>alert(1)</script>` || out.sources.sources[3].Title != "invoicefdp.exe" {
		t.Errorf("%+v", out.sources.sources)
	}
}

func TestAnEventOfAHundredAndFiftySourcesKeepsAHundred(t *testing.T) {
	var entries []string
	for n := 1; n <= 150; n++ {
		entries = append(entries, fmt.Sprintf(`{"n":%d,"title":"t","url":"https://a.example/%d","cited":false}`, n, n))
	}
	srv := host(t, sourcesEvent(`{"sources":[`+strings.Join(entries, ",")+`]}`), `{"type":"RUN_FINISHED"}`)
	out, err := (&Follower{Endpoint: srv.URL}).read(context.Background(), Run{RunID: "run_1", Token: "tok"}, 0, "", nil)
	if err != nil || out.sources == nil || len(out.sources.sources) != 100 || out.sources.sources[99].N != 100 {
		t.Fatalf("%v", err)
	}
}

func TestTheItemOfAFinishedRunCarriesItsSourcesAndTheSummaryIsLeftAlone(t *testing.T) {
	s := newResumable(t, false, []string{`{"type":"RUN_STARTED"}`, `{"type":"TEXT_MESSAGE_START","messageId":"m1"}`,
		`{"type":"TEXT_MESSAGE_CONTENT","messageId":"m1","delta":"Two days [1]; four [7]."}`, `{"type":"TEXT_MESSAGE_END","messageId":"m1"}`,
		sourcesEvent(goodSources), `{"type":"RUN_FINISHED"}`}, nil)
	store, done := follow(t, s, time.Minute)
	waitFor(t, "the follower to end", ended(done))
	all := store.all()
	final := all[len(all)-1]
	if len(all) != 1 || final.Title != "Ask the order assistant — finished" || final.Summary != "Two days [1]; four [7]." || len(final.Sources) != 2 || fmt.Sprint(final.UnverifiedCitations) != "[7]" {
		t.Fatalf("%+v", all)
	}
	if strings.Contains(final.Summary, "port.example") {
		t.Fatal("the sources are not appended to the summary text")
	}
	if err := workspace.ValidRunSources(final.Sources, final.UnverifiedCitations); err != nil {
		t.Fatal(err)
	}
}

func TestARunWithoutTheEventHasNoSourcesOnItsItem(t *testing.T) {
	s := newResumable(t, false, []string{`{"type":"RUN_STARTED"}`, `{"type":"RUN_FINISHED"}`}, nil)
	store, done := follow(t, s, time.Minute)
	waitFor(t, "the follower to end", ended(done))
	if got := store.all()[0]; got.Sources != nil || got.UnverifiedCitations != nil {
		t.Fatalf("%+v", got)
	}
}

func TestAFailedOrCutFollowShowsNoSources(t *testing.T) {
	// idp-host sends the event only for a completed run (D49-8), and the follower asks for nothing more, so an item
	// whose run failed, was cancelled or hit its limit has none; so has one whose stream was lost (D29)
	for name, events := range map[string][]string{
		"the run failed":             {`{"type":"RUN_STARTED"}`, sourcesEvent(goodSources), `{"type":"RUN_ERROR","code":"RUN_TIMEOUT","message":"the run exceeded its time limit"}`},
		"the run was cancelled":      {`{"type":"RUN_STARTED"}`, sourcesEvent(goodSources), `{"type":"RUN_ERROR","code":"CANCELED","message":"canceled by its owner"}`},
		"the stream ended too early": {`{"type":"RUN_STARTED"}`, sourcesEvent(goodSources)},
	} {
		s := newResumable(t, false, events, nil)
		store, done := follow(t, s, time.Minute)
		waitFor(t, name, ended(done))
		got := store.all()[0]
		if got.Sources != nil || got.UnverifiedCitations != nil || got.Title == "Ask the order assistant — finished" {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// TestWhatIdpHostsContractAllowsOfASourceTheFollowerKeeps is the consumer side of idp.telos.host 1.11.0's RunSource
// (D29): the bounds the workspace checks are the contract's, and every source of the contract's published AgentRun
// example comes out of the follower's reading of the event unchanged. A source the host may publish that the
// workspace dropped would be a source the member never sees.
func TestWhatIdpHostsContractAllowsOfASourceTheFollowerKeeps(t *testing.T) {
	dir := "../../tests/contracts/idp.telos.host/1.11.0/"
	raw, err := os.ReadFile(dir + "schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	src := schema.Defs["RunSource"]
	read := map[string]bool{}
	rt := reflect.TypeOf(workspace.RunSource{})
	for i := 0; i < rt.NumField(); i++ {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		read[name] = true
		if _, ok := src.Properties[name]; !ok {
			t.Errorf("the workspace keeps RunSource.%s, which idp.telos.host does not define", name)
		}
	}
	for name := range src.Properties {
		if !read[name] {
			t.Errorf("idp.telos.host's RunSource.%s is not read by the workspace", name)
		}
	}
	bound := func(prop, key string) float64 {
		var p map[string]any
		_ = json.Unmarshal(src.Properties[prop], &p)
		v, _ := p[key].(float64)
		return v
	}
	if bound("n", "maximum") != workspace.MaxSourceNumber || bound("n", "minimum") != 1 || bound("title", "maxLength") != workspace.MaxSourceTitle || bound("url", "maxLength") != workspace.MaxSourceURL {
		t.Errorf("the bounds differ from the contract's: n %v..%v, title %v, url %v", bound("n", "minimum"), bound("n", "maximum"), bound("title", "maxLength"), bound("url", "maxLength"))
	}
	var run struct {
		Props map[string]map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(mustJSON(t, schema.Defs["AgentRun"]), &run)
	if run.Props["sources"]["maxItems"] != float64(workspace.MaxRunSources) {
		t.Errorf("AgentRun.sources.maxItems %v", run.Props["sources"]["maxItems"])
	}
	items, _ := run.Props["unverifiedCitations"]["items"].(map[string]any)
	if items["maximum"] != float64(workspace.MaxCitationNumber) || items["minimum"] != 1.0 {
		t.Errorf("AgentRun.unverifiedCitations items %v", items)
	}
	raw, err = os.ReadFile(dir + "examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Examples []struct {
			Definition string          `json:"definition"`
			Value      json.RawMessage `json:"value"`
		} `json:"examples"`
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, x := range ex.Examples {
		var r struct {
			Sources    []workspace.RunSource `json:"sources"`
			Unverified []int                 `json:"unverifiedCitations"`
		}
		if x.Definition != "AgentRun" || json.Unmarshal(x.Value, &r) != nil || len(r.Sources) == 0 {
			continue
		}
		checked++
		got, unverified, dropped := workspace.ParseRunSources(x.Value)
		if dropped != 0 || !reflect.DeepEqual(got, r.Sources) || !reflect.DeepEqual(unverified, r.Unverified) {
			t.Errorf("the follower changes the contract's own example: %+v %v (%d dropped), want %+v %v", got, unverified, dropped, r.Sources, r.Unverified)
		}
	}
	if checked == 0 {
		t.Error("the contract has no example with sources")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
