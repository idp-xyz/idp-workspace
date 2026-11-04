package api

import (
	"net/http"

	"idp-workspace/internal/apptoken"
	"idp-workspace/internal/authorization"
	"idp-workspace/internal/transport"
	"idp-workspace/internal/workspace"
)

// ShellOIDC tells the shell where to sign in (D14): the issuer and the public client.
type ShellOIDC struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
	Scope    string `json:"scope"`
}

// appToken issues a short-lived token for an installed app's own backend (D15): only for a member,
// an installed and published release, and a manifest that requested identity.token.
func (a *API) appToken(r *http.Request, p authorization.Principal) (int, any, error) {
	if a.AppTokens == nil {
		return 0, nil, transport.Fail(http.StatusServiceUnavailable, "UNAVAILABLE", "app tokens are not configured")
	}
	m, err := a.member(r, p)
	if err != nil {
		return 0, nil, err
	}
	inst, err := a.Store.GetInstallation(r.Context(), p.Tenant, m.Workspace.ID, r.PathValue("app"))
	if err != nil {
		return 0, nil, transport.Fail(http.StatusNotFound, "APP_NOT_INSTALLED", r.PathValue("app")+" is not installed in this workspace")
	}
	if inst.ReleaseStatus != "published" {
		return 0, nil, transport.Fail(http.StatusConflict, "APP_REVOKED", inst.App+" "+inst.Version+" is revoked")
	}
	rel, err := a.Store.GetRelease(r.Context(), p.Tenant, inst.App, inst.Version)
	if err != nil {
		return 0, nil, err
	}
	if !rel.Manifest.Has(workspace.CapIdentityToken) {
		return 0, nil, transport.Fail(http.StatusForbidden, "CAPABILITY_DENIED", inst.App+" did not request identity.token")
	}
	tok, exp, err := a.AppTokens.Issue(apptoken.Claims{Subject: p.Subject, Tenant: p.Tenant, WorkspaceID: m.Workspace.ID, Workspace: m.Workspace.Name,
		App: inst.App, AppVersion: inst.Version, Roles: m.Roles})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"token": tok, "expiresAt": exp.UTC(), "audience": apptoken.AudiencePrefix + inst.App}, nil
}
