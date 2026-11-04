// Package runs turns an AgentRun a member started through an action into an inbox item it can read
// (ADR-0001 D11): the item appears when the run is started and is updated with the run's last
// assistant text, or its error, when the run ends. The workspace relays; idp-host keeps the run.
package runs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/workspace"
)

// Producer is the producer name the workspace uses for its own items.
const Producer = "idp-workspace"

// Follower follows runs on idp-host.
type Follower struct {
	Store    *postgres.Store
	Endpoint string
	HTTP     *http.Client
	Log      *slog.Logger
	MaxWait  time.Duration // default 10 minutes
}

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

// Follow reads the run's events until it ends (or MaxWait), then updates the item. It runs detached
// from the request and logs, never returns, its failures.
func (f *Follower) Follow(r Run) {
	wait := f.MaxWait
	if wait == 0 {
		wait = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	title, summary := r.Title+" — finished", ""
	outcome, err := f.read(ctx, r)
	switch {
	case err != nil:
		title, summary = r.Title+" — result unavailable", "The workspace could not follow run "+r.RunID+": "+err.Error()
	case outcome.failed:
		title, summary = r.Title+" — failed", outcome.message
	default:
		summary = outcome.text
		if summary == "" {
			summary = "The run finished without a text answer."
		}
	}
	if len(summary) > 3900 {
		summary = summary[:3900] + "…"
	}
	if _, _, err := f.Store.PutItem(context.Background(), r.Tenant, r.Workspace, Producer, f.item(r, title, summary)); err != nil && f.Log != nil {
		f.Log.Warn("run result not put into the inbox", "run", r.RunID, "error", err)
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

type outcome struct {
	failed  bool
	message string
	text    string
}

// read streams GET /v1/runs/{id}/events and collects the last assistant message.
func (f *Follower) read(ctx context.Context, r Run) (outcome, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.Endpoint, "/")+"/v1/runs/"+url.PathEscape(r.RunID)+"/events", nil)
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
	var out outcome
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			continue
		}
		if line != "" || len(data) == 0 {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Delta   string `json:"delta"`
			Message string `json:"message"`
			Code    string `json:"code"`
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
		case "RUN_FINISHED":
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
