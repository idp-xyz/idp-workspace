package workspace

import (
	"regexp"
	"time"
)

// Connected services (ADR-0001 D30; contract idp.telos.workspace 1.4.0). A connection is a third-party MCP service the
// operator configured on idp-mcp-bridge; the member connects their own account to it once, and their agents use its
// tools as them. The bridge holds the tokens and decides everything about them; the Workspace shows the state it
// reports, with every field re-checked, because the Workspace renders it.

// ConnectionIDPattern is what a connection id looks like: the same pattern the bridge's configuration and idp-host's
// idp.host/authorization.required event use (idp-host D61-2). It stays out of addresses and markup.
var ConnectionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Statuses of a connection for a member.
const (
	ConnectionNotRequired  = "not_required"
	ConnectionNotConnected = "not_connected"
	ConnectionConnected    = "connected"
	ConnectionReauthorize  = "reauthorization_required"
)

// ConnectionStatuses and ConnectionReasons are the closed sets the bridge answers with; anything else is not shown.
var (
	ConnectionStatuses = map[string]bool{ConnectionNotRequired: true, ConnectionNotConnected: true, ConnectionConnected: true, ConnectionReauthorize: true}
	ConnectionReasons  = map[string]bool{"insufficient_scope": true, "issuer_changed": true, "expired": true}
)

// ConnectionAccount is the state of one account on a connection: the member's own (on Connection itself) or, for an
// operator, the reviewer account the connection's tools are discovered and reviewed with.
type ConnectionAccount struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	Scope  string `json:"scope,omitempty"`
}

// Connection is one entry of the member's list of connected services.
type Connection struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// Reason is one of ConnectionReasons, with a status of reauthorization_required.
	Reason string `json:"reason,omitempty"`
	// Scope is what the member granted, space separated.
	Scope string `json:"scope,omitempty"`
	// Reviewer is present only when the bridge reports it (the caller holds its reviewer role).
	Reviewer *ConnectionAccount `json:"reviewer,omitempty"`
}

// ConnectionList is the answer of GET /v1/me/connections.
type ConnectionList struct {
	Connections []Connection `json:"connections"`
}

// ConnectionAuthorization is where the member's browser goes to authorize: the third party's authorization page, which
// the bridge built and the Workspace checked (https, no user info).
type ConnectionAuthorization struct {
	URL string `json:"url"`
}

// ConnectionResult is the answer of the callback: the connection that is now connected and where the Workspace goes back to
// (a relative path of the Workspace itself).
type ConnectionResult struct {
	Connection string `json:"connection"`
	Status     string `json:"status"`
	ReturnPath string `json:"returnPath"`
	// Reviewer: the authorization was for the connection's reviewer account.
	Reviewer bool `json:"reviewer,omitempty"`
}

// PendingConnect is a connect the member started and has not finished: kept for the member (tenant and subject) until the
// callback consumes it or it expires. It holds the hash of the state, never the state, and nothing the callback brings.
type PendingConnect struct {
	// StateHash is the hex SHA-256 of the state parameter of the authorization address the bridge returned.
	StateHash  string
	Connection string
	// Reviewer: the authorization is for the connection's reviewer account, not the member's own.
	Reviewer bool
	// ReturnPath is a relative path of the Workspace the callback goes back to (ValidReturnPath).
	ReturnPath string
	ExpiresAt  time.Time
}
