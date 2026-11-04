package workspace

import "encoding/json"

// BridgeProtocol names the postMessage protocol between the shell and an app page (D5).
const BridgeProtocol = "idp.bridge/v1"

// Bridge message types.
const (
	BridgeRequest  = "request"  // app -> shell
	BridgeResponse = "response" // shell -> app, answering a request by id
	BridgeEvent    = "event"    // shell -> app
)

// Bridge methods (requests an app may send) and the capability each needs ("" = none).
var BridgeMethods = map[string]string{
	"bridge.hello":   "",
	"context.get":    CapContextRead,
	"context.set":    CapContextWrite,
	"actions.invoke": CapActionsInvoke,
	"inbox.list":     CapInboxRead,
	"ui.notify":      CapNotify,
	"identity.token": CapIdentityToken,
}

// Bridge events (the shell sends) and the capability an app needs to receive each.
var BridgeEvents = map[string]string{
	"context.changed": CapContextRead,
	"theme.changed":   "",
}

// BridgeMessage is one message of the app bridge. A request carries method and params; a response
// carries the request's id and result or error; an event carries event and data.
type BridgeMessage struct {
	Protocol string          `json:"protocol"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type"`
	Method   string          `json:"method,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
	Error    *Problem        `json:"error,omitempty"`
	Event    string          `json:"event,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}
