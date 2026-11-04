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
