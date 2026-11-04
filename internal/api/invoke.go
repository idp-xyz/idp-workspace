package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"idp-workspace/adapters/store/postgres"
	"idp-workspace/internal/authorization"
	"idp-workspace/internal/canonical"
	"idp-workspace/internal/dispatch"
	"idp-workspace/internal/schema"
	"idp-workspace/internal/transport"
	"idp-workspace/internal/workspace"
)

// invocationStatus maps a recorded failure to the HTTP status of the answer (stable on replay).
var invocationStatus = map[string]int{
	"INVALID_INPUT":      http.StatusUnprocessableEntity,
	"TARGET_REJECTED":    http.StatusConflict,
	"TARGET_UNAVAILABLE": http.StatusBadGateway,
	"TARGET_UNSUPPORTED": http.StatusBadGateway,
}

// invoke runs an action (ADR-0001 D4): the member, the installed and still published app release,
// the contract's roles, the inbox item it is taken from (open, offering this action), the input
// schema, then the target. Every forwarded attempt is recorded; Idempotency-Key makes a retry return
// the recorded invocation instead of acting twice.
func (a *API) invoke(r *http.Request, p authorization.Principal) (int, any, error) {
	raw, err := readBody(r)
	if err != nil {
		return 0, nil, err
	}
	var in struct {
		App    string          `json:"app"`
		Action string          `json:"action"`
		Input  json.RawMessage `json:"input"`
		ItemID string          `json:"itemId"`
	}
	if err := decodeRaw(raw, &in); err != nil {
		return 0, nil, err
	}
	m, err := a.member(r, p)
	if err != nil {
		return 0, nil, err
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 200 {
		return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID", "Idempotency-Key is at most 200 characters")
	}
	sum := sha256.Sum256(append([]byte(r.URL.Path+"\n"), raw...))
	requestHash := hex.EncodeToString(sum[:])
	if prior, hash, err := a.Store.PriorInvocation(r.Context(), p.Tenant, p.Subject, key); err != nil {
		return 0, nil, err
	} else if prior != nil {
		if hash != requestHash {
			return 0, nil, transport.Fail(http.StatusConflict, "CONFLICT", "Idempotency-Key "+key+" was used for a different request")
		}
		return a.answer(prior, true)
	}

	inst, err := a.Store.GetInstallation(r.Context(), p.Tenant, m.Workspace.ID, in.App)
	if err != nil {
		return 0, nil, transport.Fail(http.StatusNotFound, "APP_NOT_INSTALLED", in.App+" is not installed in this workspace")
	}
	if inst.ReleaseStatus != "published" {
		return 0, nil, transport.Fail(http.StatusConflict, "APP_REVOKED", in.App+" "+inst.Version+" is revoked")
	}
	rel, err := a.Store.GetRelease(r.Context(), p.Tenant, inst.App, inst.Version)
	if err != nil {
		return 0, nil, err
	}
	contract, ok := rel.Manifest.Action(in.Action)
	if !ok {
		return 0, nil, transport.Fail(http.StatusNotFound, "ACTION_NOT_FOUND", in.App+" "+inst.Version+" has no action "+in.Action)
	}
	if len(contract.Roles) > 0 && !overlaps(contract.Roles, m.Roles) {
		return 0, nil, transport.Fail(http.StatusForbidden, "FORBIDDEN", "action "+in.Action+" needs one of the workspace roles "+strings.Join(contract.Roles, ", "))
	}
	input := in.Input
	if len(bytes.TrimSpace(input)) == 0 || string(bytes.TrimSpace(input)) == "null" {
		input = json.RawMessage(`{}`)
	}
	var item *workspace.InboxItem
	if in.ItemID != "" {
		if item, err = a.Store.GetItem(r.Context(), p.Tenant, m.Workspace.ID, in.ItemID, p.Subject, m.Roles); err != nil {
			return 0, nil, err
		}
		if item.Status != workspace.ItemOpen {
			return 0, nil, transport.Fail(http.StatusConflict, "ITEM_CLOSED", "the item is "+item.Status)
		}
		var offered *workspace.ItemAction
		for i := range item.Actions {
			if item.Actions[i].App == in.App && item.Actions[i].Action == in.Action {
				offered = &item.Actions[i]
			}
		}
		if offered == nil {
			return 0, nil, transport.Fail(http.StatusConflict, "ACTION_NOT_OFFERED", "the item does not offer "+in.App+"/"+in.Action)
		}
		if input, err = merge(offered.Input, input); err != nil {
			return 0, nil, err
		}
	}
	s, err := schema.Compile(contract.Input)
	if err != nil {
		return 0, nil, transport.Fail(http.StatusConflict, "INVALID_MANIFEST", err.Error())
	}
	if err := schema.Validate(s, input); err != nil {
		return 0, nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID_INPUT", err.Error())
	}
	wctx, err := a.Store.GetContext(r.Context(), p.Tenant, m.Workspace.ID, p.Subject)
	if err != nil {
		return 0, nil, err
	}
	canon, err := canonical.Canonical(input)
	if err != nil {
		return 0, nil, err
	}
	digest := sha256.Sum256(canon)
	v := workspace.Invocation{ID: postgres.NewUUID(), WorkspaceID: m.Workspace.ID, Subject: p.Subject, App: inst.App, AppVersion: inst.Version, Action: contract.Name,
		InputDigest: "sha256:" + hex.EncodeToString(digest[:]), Target: contract.Target}
	if item != nil {
		v.ItemID = item.ID
	}
	if a.Dispatcher == nil {
		a.Dispatcher = &dispatch.Dispatcher{}
	}
	result, f := a.Dispatcher.Dispatch(r.Context(), dispatch.Request{InvocationID: v.ID, Principal: p, Workspace: m.Workspace, App: inst.App, AppVersion: inst.Version,
		Action: *contract, Input: input, Item: item, Context: wctx})
	if f != nil {
		v.Status, v.Error = workspace.InvocationFailed, &workspace.Problem{Code: f.Code, Message: f.Message}
	} else {
		v.Status, v.Result = workspace.InvocationSucceeded, result
		if contract.Target.Kind == workspace.TargetHostRun && a.Runs != nil {
			a.follow(r, p, m.Workspace.ID, contract.Title, result, wctx)
		}
	}
	if a.invocations != nil {
		a.invocations.Inc(contract.Target.Kind, v.Status)
	}
	out, replayed, err := a.Store.RecordInvocation(r.Context(), p.Tenant, v, key, requestHash)
	if err != nil {
		return 0, nil, err
	}
	return a.answer(out, replayed)
}

// answer is the response for an invocation: 201 when it succeeded, the mapped error status with the
// invocation attached when it failed; a replay says so.
func (a *API) answer(v *workspace.Invocation, replayed bool) (int, any, error) {
	status, body := http.StatusCreated, any(v)
	if v.Status == workspace.InvocationFailed && v.Error != nil {
		status = invocationStatus[v.Error.Code]
		if status == 0 {
			status = http.StatusBadGateway
		}
		body = map[string]any{"error": v.Error, "invocation": v}
	}
	if replayed {
		if status == http.StatusCreated {
			status = http.StatusOK
		}
		return status, &withHeaders{body: body, headers: map[string]string{"Idempotent-Replayed": "true"}}, nil
	}
	return status, body, nil
}

// merge lays the member's input over an item's preset input (top-level fields; the member's win).
func merge(preset, input json.RawMessage) (json.RawMessage, error) {
	if len(preset) == 0 {
		return input, nil
	}
	var base, over map[string]any
	if err := json.Unmarshal(preset, &base); err != nil {
		return nil, transport.Fail(http.StatusConflict, "INVALID_ITEM", "the item's preset input is not an object")
	}
	if err := json.Unmarshal(input, &over); err != nil {
		return nil, transport.Fail(http.StatusUnprocessableEntity, "INVALID_INPUT", "input must be an object")
	}
	for k, v := range over {
		base[k] = v
	}
	return json.Marshal(base)
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
