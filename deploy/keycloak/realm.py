#!/usr/bin/env python3
"""Generate the development Keycloak realm for the workspace's OIDC=1 end-to-end run (ADR-0001 D14-D17).

- workspace-shell: the shell, a public client (authorization code + PKCE S256); its tokens carry
  aud idp-workspace, tenant_id (user attribute), roles (realm roles) and preferred_username, which
  the workspace uses as the member's name (IDP_WORKSPACE_SUBJECT_CLAIM).
- idp-workspace: the workspace as a confidential client with standard token exchange, so a member's
  token (aud idp-workspace) is exchanged for one with aud idp-host (D16).
- svc-eer: a producer service (client credentials): aud idp-workspace, tenant, role ws-producer (D17).
- e2e-cli: a test-only client with the password grant, so the script can act as hostadmin etc.

  python3 deploy/keycloak/realm.py OUT_DIR TENANT_ID CLIENTS_FILE
writes OUT_DIR/idp-realm.json (mounted for --import-realm) and CLIENTS_FILE (secrets for the test),
which must not be in OUT_DIR (Keycloak imports every .json there). TEST DOUBLE: dev secrets only.
"""
import json
import os
import secrets
import sys

out, tenant, clients_file = sys.argv[1:4]
SHELL = "http://127.0.0.1:8560"


def hardcoded(name, value, json_type="String"):
    return {"name": name, "protocol": "openid-connect", "protocolMapper": "oidc-hardcoded-claim-mapper",
            "config": {"claim.name": name, "claim.value": value, "jsonType.label": json_type,
                       "access.token.claim": "true", "id.token.claim": "false", "introspection.token.claim": "true"}}


def audience(a):
    return {"name": "aud-" + a, "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
            "config": {"included.client.audience": a, "access.token.claim": "true", "id.token.claim": "false", "introspection.token.claim": "true"}}


tenant_attr = {"name": "tenant_id", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-attribute-mapper",
               "config": {"user.attribute": "tenant_id", "claim.name": "tenant_id", "jsonType.label": "String",
                          "access.token.claim": "true", "id.token.claim": "true", "userinfo.token.claim": "true", "introspection.token.claim": "true"}}
roles_map = {"name": "roles", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-realm-role-mapper",
             "config": {"claim.name": "roles", "jsonType.label": "String", "multivalued": "true",
                        "access.token.claim": "true", "id.token.claim": "false", "introspection.token.claim": "true"}}
username = {"name": "preferred_username", "protocol": "openid-connect", "protocolMapper": "oidc-usermodel-property-mapper",
            "config": {"user.attribute": "username", "claim.name": "preferred_username", "jsonType.label": "String",
                       "access.token.claim": "true", "id.token.claim": "true", "userinfo.token.claim": "true"}}

users = {"alice": ["ws-admin", "host-user"], "bob": ["ws-member", "host-user"], "hostadmin": ["host-admin"], "hostapprover": ["host-approver"]}
passwords = {u: secrets.token_urlsafe(16) for u in users}
ws_secret, eer_secret, cli_secret = (secrets.token_urlsafe(32) for _ in range(3))
realm = {
    "realm": "idp", "enabled": True, "sslRequired": "none", "accessTokenLifespan": 600,
    "roles": {"realm": [{"name": r} for r in ("ws-admin", "ws-member", "ws-producer", "host-user", "host-admin", "host-approver")]},
    "users": [{"username": u, "enabled": True, "emailVerified": True, "email": u + "@example.com", "firstName": u, "lastName": "User",
               "attributes": {"tenant_id": [tenant]}, "realmRoles": roles,
               "credentials": [{"type": "password", "value": passwords[u], "temporary": False}]} for u, roles in users.items()],
    "clients": [
        {"clientId": "workspace-shell", "name": "IDP Workspace shell (authorization code + PKCE)", "enabled": True, "protocol": "openid-connect", "publicClient": True,
         "standardFlowEnabled": True, "directAccessGrantsEnabled": False, "redirectUris": [SHELL + "/callback"], "webOrigins": [SHELL],
         "fullScopeAllowed": True, "attributes": {"pkce.code.challenge.method": "S256", "post.logout.redirect.uris": SHELL + "/"},
         "protocolMappers": [audience("idp-workspace"), tenant_attr, roles_map, username]},
        {"clientId": "idp-workspace", "name": "IDP Workspace (token exchange requester)", "enabled": True, "protocol": "openid-connect",
         "publicClient": False, "secret": ws_secret, "standardFlowEnabled": False, "serviceAccountsEnabled": False, "fullScopeAllowed": True,
         "attributes": {"standard.token.exchange.enabled": "true"}, "protocolMappers": [audience("idp-host"), tenant_attr, roles_map, username]},
        {"clientId": "idp-host", "name": "IDP HOST (audience)", "enabled": True, "protocol": "openid-connect", "bearerOnly": True, "publicClient": False, "standardFlowEnabled": False},
        {"clientId": "svc-eer", "name": "EER as an inbox producer (client credentials)", "enabled": True, "protocol": "openid-connect", "publicClient": False,
         "secret": eer_secret, "standardFlowEnabled": False, "serviceAccountsEnabled": True, "fullScopeAllowed": False,
         "protocolMappers": [audience("idp-workspace"), hardcoded("tenant_id", tenant), hardcoded("roles", json.dumps(["ws-producer"]), "JSON"), username]},
        {"clientId": "e2e-cli", "name": "end-to-end script (password grant; test only)", "enabled": True, "protocol": "openid-connect", "publicClient": False,
         "secret": cli_secret, "standardFlowEnabled": False, "directAccessGrantsEnabled": True, "fullScopeAllowed": True,
         "protocolMappers": [audience("idp-workspace"), audience("idp-host"), tenant_attr, roles_map, username]},
    ],
}
os.makedirs(out, exist_ok=True)
with open(os.path.join(out, "idp-realm.json"), "w") as f:
    json.dump(realm, f, indent=1)
os.chmod(os.path.join(out, "idp-realm.json"), 0o644)
with open(clients_file, "w") as f:
    json.dump({"users": passwords, "idp-workspace": ws_secret, "svc-eer": eer_secret, "e2e-cli": cli_secret}, f, indent=1)
os.chmod(clients_file, 0o600)
print("OK: realm idp with", len(realm["clients"]), "clients and", len(users), "users")
