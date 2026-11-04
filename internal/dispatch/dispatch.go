// Package dispatch forwards an invoked action to the system that owns its decision (ADR-0001 D4):
// an AgentRun on idp-host, an approval decision on idp-eer, or the app's own backend (a signed
// webhook). It never decides anything itself; it reports what the target answered.
package dispatch

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"idp-workspace/internal/authorization"
	"idp-workspace/internal/workspace"
)

// Credential modes for a downstream system.
const (
	AuthForward = "forward" // the caller's own token (the identity provider issues it for the downstream audience too)
	AuthStatic  = "static"  // a service token from a file
)

// Downstream is one configured system.
type Downstream struct {
	Endpoint string // https://host:port (no trailing slash)
	HTTP     *http.Client
	Auth     string
	Token    func() (string, error) // static mode
	Exchange *Exchanger             // exchange mode
	Timeout  time.Duration
}

// Bearer is the credential this downstream is called with on behalf of p.
func (d *Downstream) Bearer(p authorization.Principal) (string, error) { return d.bearer(p) }

func (d *Downstream) bearer(p authorization.Principal) (string, error) {
	switch d.Auth {
	case AuthExchange:
		if d.Exchange == nil {
			return "", errors.New("no token exchange configured")
		}
		return d.Exchange.Exchange(context.Background(), p.Token)
	case AuthStatic:
		if d.Token == nil {
			return "", errors.New("no service token configured")
		}
		return d.Token()
	default:
		if p.Token == "" {
			return "", errors.New("the caller's token is not available to forward")
		}
		return p.Token, nil
	}
}

// Dispatcher holds the configured targets; a nil target makes actions of that kind fail with
// TARGET_UNAVAILABLE.
type Dispatcher struct {
	Host *Downstream
	EER  *Downstream
	// EERTenant is the tenant id idp-eer expects in request bodies (its own mapping, not the
	// workspace's tenant).
	EERTenant string
	// WebhookSecret signs webhook bodies; WebhookHTTP sends them.
	WebhookSecret []byte
	WebhookHTTP   *http.Client
	Now           func() time.Time
}

// Request is one action to forward.
type Request struct {
	InvocationID string
	Principal    authorization.Principal
	Workspace    workspace.Workspace
	App          string
	AppVersion   string
	Action       workspace.ActionContract
	Input        json.RawMessage // validated against the action's input schema
	Item         *workspace.InboxItem
	Context      *workspace.WorkContext
}

// Failure is a dispatch that did not succeed: a stable code and a safe message.
type Failure struct {
	Code    string
	Message string
	Status  int // the downstream HTTP status, 0 when it was not reached
}

func (f *Failure) Error() string { return f.Code + ": " + f.Message }

func failure(code string, status int, format string, a ...any) *Failure {
	return &Failure{Code: code, Status: status, Message: fmt.Sprintf(format, a...)}
}

// Dispatch forwards the action and returns the target's reference.
func (d *Dispatcher) Dispatch(ctx context.Context, r Request) (json.RawMessage, *Failure) {
	switch r.Action.Target.Kind {
	case workspace.TargetHostRun:
		return d.startRun(ctx, r)
	case workspace.TargetEERDecision:
		return d.eerDecision(ctx, r)
	case workspace.TargetWebhook:
		return d.webhook(ctx, r)
	}
	return nil, failure("TARGET_UNSUPPORTED", 0, "target kind %q", r.Action.Target.Kind)
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// hostRun is what the workspace reads of idp-host's AgentRun (idp.telos.host; pinned in
// tests/contracts/idp.telos.host and checked by TestHostContractPin).
type hostRun struct {
	RunID    string `json:"runId"`
	ThreadID string `json:"threadId"`
	Status   string `json:"status"`
}

// hostRun starts an AgentRun: the input becomes the user message, the work context's entities and
// the action's identity go in as AG-UI context. The invocation id is the run id and the
// idempotency key, so a retried invocation does not start a second run.
func (d *Dispatcher) startRun(ctx context.Context, r Request) (json.RawMessage, *Failure) {
	if d.Host == nil {
		return nil, failure("TARGET_UNAVAILABLE", 0, "no idp-host is configured")
	}
	message := string(r.Input)
	var in map[string]any
	if json.Unmarshal(r.Input, &in) == nil {
		if m, ok := in["message"].(string); ok && len(in) == 1 {
			message = m
		}
	}
	runID := "ws_" + strings.ReplaceAll(r.InvocationID, "-", "")
	ctxEntries := []map[string]string{{"description": "idp.workspace/action", "value": r.App + "/" + r.Action.Name + " in workspace " + r.Workspace.Name}}
	threadID := ""
	if r.Context != nil {
		if len(r.Context.Entities) > 0 {
			b, _ := json.Marshal(r.Context.Entities)
			ctxEntries = append(ctxEntries, map[string]string{"description": "idp.workspace/entities", "value": string(b)})
		}
		threadID = r.Context.Thread
	}
	if r.Item != nil {
		b, _ := json.Marshal(map[string]any{"itemId": r.Item.ID, "ref": r.Item.Ref, "title": r.Item.Title})
		ctxEntries = append(ctxEntries, map[string]string{"description": "idp.workspace/item", "value": string(b)})
	}
	body := map[string]any{
		"threadId": threadID, "runId": runID,
		"messages": []map[string]any{{"id": "msg_" + runID, "role": "user", "content": message}},
		"tools":    []any{}, "context": ctxEntries,
	}
	var out hostRun
	status, f := d.call(ctx, d.Host, r.Principal, http.MethodPost, "/v1/agents/"+url.PathEscape(r.Action.Target.Agent)+"/runs", runID, body, &out)
	if f != nil {
		return nil, f
	}
	res, _ := json.Marshal(map[string]any{"system": "host", "kind": "run", "id": out.RunID, "threadId": out.ThreadID, "status": out.Status, "httpStatus": status})
	return res, nil
}

// eerDecision decides a T2 approval of an execution: approve -> approved, reject -> rejected. The
// execution is the item's ref (system eer, kind execution) or the input's executionId; the actor is
// the caller's subject (idp-eer checks it against its own mapping of the token).
func (d *Dispatcher) eerDecision(ctx context.Context, r Request) (json.RawMessage, *Failure) {
	if d.EER == nil {
		return nil, failure("TARGET_UNAVAILABLE", 0, "no idp-eer is configured")
	}
	execution := ""
	if r.Item != nil && r.Item.Ref.System == "eer" && r.Item.Ref.Kind == "execution" {
		execution = r.Item.Ref.ID
	}
	var in struct {
		ExecutionID string `json:"executionId"`
	}
	_ = json.Unmarshal(r.Input, &in)
	if execution == "" {
		execution = in.ExecutionID
	} else if in.ExecutionID != "" && in.ExecutionID != execution {
		return nil, failure("INVALID_INPUT", 0, "executionId %s does not match the item's execution %s", in.ExecutionID, execution)
	}
	if execution == "" {
		return nil, failure("INVALID_INPUT", 0, "an eer.decision needs an inbox item about an execution or an executionId")
	}
	decision := map[string]string{"approve": "approved", "reject": "rejected"}[r.Action.Target.Decision]
	body := map[string]string{"tenant_id": d.EERTenant, "decision": decision, "actor_id": r.Principal.Subject}
	var out struct {
		Decision string `json:"decision"`
		Accepted bool   `json:"accepted"`
	}
	status, f := d.call(ctx, d.EER, r.Principal, http.MethodPost, "/v1/executions/"+url.PathEscape(execution)+"/approval", r.InvocationID, body, &out)
	if f != nil {
		return nil, f
	}
	res, _ := json.Marshal(map[string]any{"system": "eer", "kind": "execution", "id": execution, "decision": out.Decision, "accepted": out.Accepted, "httpStatus": status})
	return res, nil
}

// webhook posts the invocation to the app's backend, signed: Idp-Signature: t=<unix>,v1=<hex
// HMAC-SHA256 of "<t>.<body>">. The app must not trust an unsigned or stale request.
func (d *Dispatcher) webhook(ctx context.Context, r Request) (json.RawMessage, *Failure) {
	if len(d.WebhookSecret) == 0 {
		return nil, failure("TARGET_UNAVAILABLE", 0, "no webhook signing secret is configured")
	}
	payload := map[string]any{
		"invocationId": r.InvocationID, "workspace": map[string]string{"id": r.Workspace.ID, "name": r.Workspace.Name},
		"subject": r.Principal.Subject, "tenant": r.Principal.Tenant, "app": r.App, "appVersion": r.AppVersion, "action": r.Action.Name, "input": r.Input,
	}
	if r.Item != nil {
		payload["item"] = map[string]any{"itemId": r.Item.ID, "ref": r.Item.Ref}
	}
	if r.Context != nil {
		payload["entities"] = r.Context.Entities
	}
	body, _ := json.Marshal(payload)
	ts := strconv.FormatInt(d.now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Action.Target.URL, bytes.NewReader(body))
	if err != nil {
		return nil, failure("TARGET_UNAVAILABLE", 0, "webhook: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idp-Signature", "t="+ts+",v1="+Sign(d.WebhookSecret, ts, body))
	req.Header.Set("Idempotency-Key", r.InvocationID)
	hc := d.WebhookHTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, failure("TARGET_UNAVAILABLE", 0, "webhook unreachable")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, failure("TARGET_REJECTED", resp.StatusCode, "webhook answered HTTP %d", resp.StatusCode)
	}
	res := map[string]any{"system": "app:" + r.App, "httpStatus": resp.StatusCode}
	if json.Valid(raw) && len(bytes.TrimSpace(raw)) > 0 {
		res["body"] = json.RawMessage(raw)
	}
	b, _ := json.Marshal(res)
	return b, nil
}

// Sign computes the webhook signature of body at timestamp ts.
func Sign(secret []byte, ts string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks a webhook signature header against body, accepting timestamps within tolerance of
// now (for app backends written in Go; the TypeScript SDK has its own).
func Verify(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return errors.New("malformed signature header")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tolerance || d < -tolerance {
		return errors.New("signature timestamp outside the tolerance")
	}
	want := Sign(secret, ts, body)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("signature mismatch")
	}
	return nil
}

// call sends one JSON request and decodes a 2xx answer into out. Downstream errors keep the
// downstream's code when it gives one ({"error":{"code"}} or a problem's "code").
func (d *Dispatcher) call(ctx context.Context, ds *Downstream, p authorization.Principal, method, path, idempotencyKey string, body, out any) (int, *Failure) {
	token, err := ds.bearer(p)
	if err != nil {
		return 0, failure("TARGET_UNAVAILABLE", 0, "%v", err)
	}
	timeout := ds.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(cctx, method, strings.TrimRight(ds.Endpoint, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return 0, failure("TARGET_UNAVAILABLE", 0, "%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	hc := ds.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, failure("TARGET_UNAVAILABLE", 0, "%s unreachable", ds.Endpoint)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		code := downstreamCode(answer)
		if code == "" {
			code = "HTTP_" + strconv.Itoa(resp.StatusCode)
		}
		kind := "TARGET_REJECTED"
		if resp.StatusCode >= 500 {
			kind = "TARGET_UNAVAILABLE"
		}
		return resp.StatusCode, failure(kind, resp.StatusCode, "%s answered %d %s", ds.Endpoint, resp.StatusCode, code)
	}
	if out != nil && len(answer) > 0 {
		if err := json.Unmarshal(answer, out); err != nil {
			return resp.StatusCode, failure("TARGET_UNAVAILABLE", resp.StatusCode, "malformed answer")
		}
	}
	return resp.StatusCode, nil
}

func downstreamCode(raw []byte) string {
	var e struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	if e.Error.Code != "" {
		return e.Error.Code
	}
	return e.Code
}
