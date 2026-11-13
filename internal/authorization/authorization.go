// Package authorization holds the authenticated caller and the workspace's role model. Decisions
// are made on tenant, subject and roles (and workspace membership, kept in the store); everything
// else on the principal is audit information.
package authorization

import "time"

// Roles (ADR-0001 D6). An administrator creates workspaces, publishes and installs apps and
// manages members; a member works in the workspaces it belongs to; a producer is a service (EER,
// HOST, an app's backend) that puts items into inboxes and resolves them.
const (
	RoleAdmin    = "ws-admin"
	RoleMember   = "ws-member"
	RoleProducer = "ws-producer"
)

// Principal is an authenticated caller.
type Principal struct {
	Subject string
	// TokenSubject is the token's own sub claim: how the rest of the platform (idp-mcp, idp-sandbox) names the caller,
	// which differs from Subject when IDP_WORKSPACE_SUBJECT_CLAIM names another claim (file tickets, D23).
	TokenSubject string
	Tenant       string
	Roles        []string
	Method       string    // "jwt" or "dev"
	Expires      time.Time // credential expiry
	// Client is the OAuth client the token was issued to (azp); Actor the party acting for the
	// subject in a delegated token (RFC 8693 act claim). Audit only.
	Client string
	Actor  string
	// Token is the caller's bearer credential; forwarded only to targets configured to receive the
	// caller's identity (ADR-0001 D4), never to apps.
	Token string `json:"-"`
}

// Has reports whether the principal holds role.
func (p Principal) Has(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}
