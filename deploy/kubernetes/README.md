# idp-workspace on Kubernetes

`kubectl apply -k deploy/kubernetes` after creating the Secrets of `secrets.example.yaml` with your secret
management and pointing `kustomization.yaml`'s image at the image `deploy/build.sh` built (by digest).

- Run the `idp-workspace-migrate` Job before rolling out a new version; create the service login
  (`idp-workspace ensure-login idp_ws_svc`) and tenants (`idp-workspace seed-tenant`) as the schema owner.
- `workspace-shell` (the shell's public OIDC client), `idp-workspace` (the token-exchange client) and the
  audiences `idp-host` / `eer-runtime` must exist at the IdP (see idp-telos-go `deploy/keycloak/realm.py`).
- The NetworkPolicies assume namespaces `databases`, `ingress`, `observability`, `idp-host`, `idp-eer`;
  replace the ipBlock placeholders with your IdP and webhook addresses.

`go test ./tests/deploy` keeps the manifests consistent; the platform repository applies them to a kind
cluster in its CI.

## Connected services (ADR-0001 D30; optional)

Off unless `targets.connections` is in `IDP_WORKSPACE_CONFIG`: then there is no page, no route (`/oauth/*` answers 404) and
`/v1/me` says `"connections": false`. To turn it on, after `idp-mcp-bridge` is deployed (its RUNBOOK 5):

```yaml
targets:
  connections:
    endpoint: http://idp-mcp-bridge.idp-mcp-bridge.svc.cluster.local:8681   # the bridge's INTERNAL API port, not the MCP port 8680
    auth: exchange                                                          # the only mode: the bridge names the member from an exchanged token and from nothing else
    exchange: {tokenUrl: https://idp.example/realms/idp/protocol/openid-connect/token, clientId: idp-workspace, clientSecretFile: /run/secrets/exchange, audience: idp-mcp-bridge}
    publicUrl: https://workspace.example                                    # the Workspace's public base URL: scheme and host, no path; https
```

| Setting | What it is |
|---|---|
| `endpoint` | The bridge's internal API (port 8681). Reachable only from the Workspace: add an egress rule to `networkpolicy.yaml` for it (the policy is default-deny and does not have one). |
| `exchange.audience` | The bridge's `IDP_BRIDGE_API_AUDIENCE`. `exchange.clientId` must be the bridge's `IDP_BRIDGE_WORKSPACE_CLIENT` (its `azp` check). The member's token carries the tenant claim the bridge reads. |
| `publicUrl` | How members' browsers reach the Workspace. Used for the two addresses below; normally the same as `IDP_WORKSPACE_PUBLIC_URL`. |
| `testOrigins` | Acceptance only, never in a deployment: exact `host:port` pairs that may be plain http (a test third party's authorization page, an http `publicUrl`). |

What the operator sets on the **bridge** so that it matches:

| On the bridge | Value |
|---|---|
| `IDP_BRIDGE_REDIRECT_URI` | `<publicUrl>/oauth/callback`, for example `https://workspace.example/oauth/callback`. Register the same address at every pre-registered client at the third party. |
| `client.document` of a `cimd` connection | `<publicUrl>/oauth/client/<connection id>.json`, for example `https://workspace.example/oauth/client/github.json`. The Workspace serves the bridge's document there, and only if the document's `client_id` is exactly that address. |

The Workspace routes (the shell is the same-origin page; the API is behind the member's token):
`GET /oauth/callback` (the shell, public, `no-store`), `GET /oauth/client/{id}.json` (public), `GET /v1/me/connections`,
`POST /v1/me/connections/{id}/authorization`, `POST /v1/me/connections/callback`, `DELETE /v1/me/connections/{id}/grant`,
`DELETE /v1/me/connections/{id}/reviewer/grant`. Pending connects (a member's started authorization, kept 10 minutes, the hash
of the state only) live in the table `connection_pending` (migration 0007: run the migrate Job before rolling out); the
Workspace itself never stores a token, code or state. Upgrade order: the bridge first, then the Workspace; roll back the other
way (an old Workspace ignores the table).
