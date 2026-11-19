// Package runs turns an AgentRun a member started through an action into an inbox item it can read
// (ADR-0001 D11): the item appears when the run is started and is updated with the run's last
// assistant text, or its error, when the run ends. A run that stops for its owner's confirmation is
// shown as awaiting it and followed on until it ends (D18). The workspace relays; idp-host keeps the
// run.
package runs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/workspace"
)

// Producer is the producer name the workspace uses for its own items.
const Producer = "idp-workspace"

// Items is the part of the store the follower writes.
type Items interface {
	PutItem(ctx context.Context, tenant, workspaceID, producer string, in postgres.ItemInput) (*workspace.InboxItem, bool, error)
}

// Tickets is the part of the store that ends a run's file tickets (D27).
type Tickets interface {
	EndRun(ctx context.Context, tenant, run string, keep time.Duration) error
}

// Follower follows runs on idp-host.
type Follower struct {
	Store Items
	// Tickets, if set, is told when a run has ended, so that its file tickets stop working then and not an hour later
	// (D27). TicketTTL is how long the tickets would have lived, which is how long the end has to be remembered.
	Tickets   Tickets
	TicketTTL time.Duration
	Endpoint  string
	HTTP      *http.Client
	Log       *slog.Logger
	MaxWait   time.Duration // default 10 minutes, used when the run's own deadline cannot be read; time spent awaiting a confirmation does not count (D18)
}

// runDeadlineGrace is how long the follower keeps reading after the run's own deadline (D26). A step that
// started before the deadline still finishes its model call and its tool call, and only then does the run
// fail, so the terminal event arrives after deadlineAt.
const runDeadlineGrace = 3 * time.Minute

// Run is one run to follow.
type Run struct {
	Tenant    string
	Workspace string // id
	Subject   string
	Token     string // the credential the run was created with
	RunID     string
	Title     string // what the member asked for (the action's title)
	Entities  []workspace.Entity
}

// Started puts the "running" item; call it before Follow.
func (f *Follower) Started(ctx context.Context, r Run) error {
	_, _, err := f.Store.PutItem(ctx, r.Tenant, r.Workspace, Producer, f.item(r, r.Title+" — running", "The agent run "+r.RunID+" has started."))
	return err
}

// Follow reads the run's events until it ends, then updates the item. A RUN_FINISHED whose outcome
// is an interrupt is not an end: the run waits on idp-host for its owner's decision, so the item says
// so and the follower reads on after the last event. The run is followed until its own deadline (D26),
// or for MaxWait when that deadline cannot be read. A confirmation request moves the bound to the
// request's expiry plus MaxWait, because idp-host resumes the run by then at the latest (an expired
// request counts as rejected). Follow runs detached from the request and logs, never returns, its failures.
func (f *Follower) Follow(r Run) {
	wait := f.MaxWait
	if wait == 0 {
		wait = 10 * time.Minute
	}
	deadline := time.Now().Add(wait)
	if until := f.runDeadline(r); !until.IsZero() {
		if end := until.Add(runDeadlineGrace); end.After(time.Now()) {
			deadline = end
		}
	}
	var out outcome
	var err error
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		last := out.last
		out, err = f.read(ctx, r, last, out.text, out.files)
		cancel()
		if err != nil || out.waiting == nil {
			break
		}
		if out.last <= last {
			err = errors.New("its event stream cannot be resumed after the run's interruption")
			break
		}
		title, summary, due := awaiting(r, out.waiting)
		f.put(r, title, summary, due, nil)
		if latest := latestExpiry(out.waiting); latest.Add(wait).After(deadline) {
			deadline = latest.Add(wait)
		}
	}
	// The run has ended (finished or failed, as idp-host says): nothing acts for it any more, so its file tickets end
	// too. Not when the stream could not be followed: the run may still be working.
	if err == nil && f.Tickets != nil {
		keep := f.TicketTTL
		if keep <= 0 {
			keep = time.Hour
		}
		ectx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if eerr := f.Tickets.EndRun(ectx, r.Tenant, r.RunID, keep+time.Minute); eerr != nil && f.Log != nil {
			f.Log.Warn("the run's file tickets were not ended", "run", r.RunID, "error", eerr)
		}
		cancel()
	}
	title, summary := r.Title+" — finished", ""
	switch {
	case err != nil:
		title, summary = r.Title+" — result unavailable", "The workspace could not follow run "+r.RunID+": "+err.Error()
	case out.failed:
		title, summary = r.Title+" — failed", out.message
	default:
		summary = out.text
		if summary == "" {
			summary = "The run finished without a text answer."
		}
	}
	if len(out.files) > 0 { // the files themselves are in the conversation and among the member's files
		summary = "Files delivered: " + strings.Join(out.files, ", ") + "\n\n" + summary
	}
	// D29: the sources belong to a run that finished. The host sends the event only for a completed run, so a run
	// that failed, was cancelled or hit its time limit has none here, and so has an item whose follow was lost: the
	// workspace does not ask for them with a request of its own, and "result unavailable" says nothing about
	// what the run read.
	var src *sourceSet
	if err == nil && !out.failed {
		src = out.sources
	}
	f.put(r, title, summary, nil, src)
}

// sourceSet is what the follower keeps of the host's idp.host/run.sources event, after checking it.
type sourceSet struct {
	sources    []workspace.RunSource
	unverified []int
}

func (f *Follower) put(r Run, title, summary string, due *time.Time, src *sourceSet) {
	if len(summary) > 3900 {
		summary = summary[:3900] + "…"
	}
	in := f.item(r, title, summary)
	in.DueAt = due
	if src != nil {
		in.Sources, in.UnverifiedCitations = src.sources, src.unverified
	}
	if _, _, err := f.Store.PutItem(context.Background(), r.Tenant, r.Workspace, Producer, in); err != nil && f.Log != nil {
		f.Log.Warn("run item not put into the inbox", "run", r.RunID, "error", err)
	}
}

func (f *Follower) item(r Run, title, summary string) postgres.ItemInput {
	if len(title) > 300 {
		title = title[:299] + "…"
	}
	return postgres.ItemInput{ExternalRef: "run:" + r.RunID, Kind: workspace.ItemRun, Title: title, Summary: summary,
		Ref:       workspace.Ref{System: "host", Kind: "run", ID: r.RunID},
		Assignees: workspace.Assignees{Subjects: []string{r.Subject}, Roles: []string{}}, Priority: "normal", Entities: r.Entities}
}

// awaiting is the item of a run that waits for a decision the workspace cannot take yet; its due
// time is the earliest expiry of the confirmation requests.
func awaiting(r Run, interrupts []interrupt) (title, summary string, due *time.Time) {
	var tools []string
	var first time.Time
	confirmations := 0
	for _, in := range interrupts {
		if in.Type != "confirmation" {
			continue
		}
		confirmations++
		if in.Tool != "" && !slices.Contains(tools, in.Tool) {
			tools = append(tools, in.Tool)
		}
		if !in.ExpiresAt.IsZero() && (first.IsZero() || in.ExpiresAt.Before(first)) {
			first = in.ExpiresAt
		}
	}
	if confirmations == 0 {
		return r.Title + " — waiting for input", "The run " + r.RunID + " waits for input that cannot be given in the Workspace.", nil
	}
	what := "a tool"
	if len(tools) > 0 {
		what = strings.Join(tools, ", ")
	}
	summary = "The agent wants to call " + what + " and waits for the run owner's confirmation. It cannot be confirmed in the Workspace yet."
	if !first.IsZero() {
		t := first.UTC()
		due = &t
		summary += " The request expires at " + t.Format("2006-01-02 15:04:05") + " UTC; an expired request counts as rejected."
	}
	return r.Title + " — awaiting confirmation", summary, due
}

func latestExpiry(interrupts []interrupt) time.Time {
	var t time.Time
	for _, in := range interrupts {
		if in.ExpiresAt.After(t) {
			t = in.ExpiresAt
		}
	}
	return t
}

type outcome struct {
	failed  bool
	message string
	text    string
	files   []string    // names of the files the run delivered (idp.host/file.delivered, D23)
	sources *sourceSet  // what the run read (idp.host/run.sources, D29): nil when the stream had none worth keeping
	waiting []interrupt // non-nil: the run stopped for a decision (RUN_FINISHED with an interrupt outcome)
	last    int64       // the id of the last event read
}

// interrupt is what the follower reads of one entry of an interrupt outcome (idp-host ADR-0001 D7:
// a tool call that waits for its owner's confirmation).
type interrupt struct {
	Type      string    `json:"type"`
	Tool      string    `json:"tool"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// interrupted returns the interrupts of a RUN_FINISHED outcome ({"type":"interrupt","interrupts":
// [...]}, or the bare string "interrupt"), and nil when the run really finished.
func interrupted(raw json.RawMessage) []interrupt {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "interrupt" {
			return []interrupt{}
		}
		return nil
	}
	var o struct {
		Type       string      `json:"type"`
		Interrupts []interrupt `json:"interrupts"`
	}
	if json.Unmarshal(raw, &o) != nil || o.Type != "interrupt" {
		return nil
	}
	if o.Interrupts == nil {
		return []interrupt{}
	}
	return o.Interrupts
}

// runDeadline reads the run's own deadline (GET /v1/runs/{id}). A zero time means it could not be read,
// and the caller keeps its own bound.
func (f *Follower) runDeadline(r Run) time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.Endpoint, "/")+"/v1/runs/"+url.PathEscape(r.RunID), nil)
	if err != nil {
		return time.Time{}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.Token)
	hc := f.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return time.Time{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return time.Time{}
	}
	var body struct {
		DeadlineAt time.Time `json:"deadlineAt"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil {
		return time.Time{}
	}
	return body.DeadlineAt
}

// read streams GET /v1/runs/{id}/events after event id after (0: from the start) and collects the
// last assistant message, starting from text, the last one read before.
func (f *Follower) read(ctx context.Context, r Run, after int64, text string, files []string) (outcome, error) {
	u := strings.TrimRight(f.Endpoint, "/") + "/v1/runs/" + url.PathEscape(r.RunID) + "/events"
	if after > 0 {
		u += "?after=" + strconv.FormatInt(after, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return outcome{}, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+r.Token)
	hc := f.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return outcome{}, fmt.Errorf("idp-host unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return outcome{}, fmt.Errorf("idp-host answered HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data []string
	var current strings.Builder
	var id int64
	out := outcome{text: text, last: after, files: files}
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			continue
		}
		if v, ok := strings.CutPrefix(line, "id:"); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				id = n
			}
			continue
		}
		if line != "" || len(data) == 0 {
			continue
		}
		if id > out.last {
			out.last = id
		}
		var ev struct {
			Type    string          `json:"type"`
			Delta   string          `json:"delta"`
			Message string          `json:"message"`
			Code    string          `json:"code"`
			Outcome json.RawMessage `json:"outcome"`
			Name    string          `json:"name"`
			Value   json.RawMessage `json:"value"`
		}
		raw := strings.Join(data, "\n")
		data = nil
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "TEXT_MESSAGE_START":
			current.Reset()
		case "TEXT_MESSAGE_CONTENT":
			current.WriteString(ev.Delta)
		case "TEXT_MESSAGE_END":
			if t := strings.TrimSpace(current.String()); t != "" {
				out.text = t
			}
		case "CUSTOM":
			switch ev.Name {
			case "idp.host/file.delivered":
				var v struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(ev.Value, &v) == nil && v.Name != "" {
					out.files = append(out.files, v.Name)
				}
			case "idp.host/run.sources":
				// D29: the host's list is read again here, field by field, before anything is kept; the last
				// event wins (the host sends one), and one with nothing valid in it leaves no sources
				out.sources = nil
				if srcs, unverified, _ := workspace.ParseRunSources(ev.Value); len(srcs) > 0 || len(unverified) > 0 {
					out.sources = &sourceSet{sources: srcs, unverified: unverified}
				}
			}
		case "RUN_FINISHED":
			out.waiting = interrupted(ev.Outcome)
			return out, nil
		case "RUN_ERROR":
			out.failed = true
			out.message = strings.TrimSpace(ev.Code + " " + ev.Message)
			return out, nil
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, fmt.Errorf("the event stream ended before the run did")
}
