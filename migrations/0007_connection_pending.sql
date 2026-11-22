-- idp-workspace schema v7 (ADR-0001 D30): the connects a member has started and not finished. A row says "this member began
-- authorizing this connection with a state whose SHA-256 is this" until the OAuth callback consumes it (one use) or it
-- expires (10 minutes). The callback of the third party carries no session of its own: the member's Workspace session is
-- the bearer token of the shell, so what binds the callback to the person who started is this row, keyed by tenant and
-- subject. No state, code or verifier is kept here: only the hash of the state. The service role cannot update a row.
CREATE TABLE connection_pending (
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  subject     text NOT NULL CHECK (length(subject) BETWEEN 1 AND 256),
  state_hash  text NOT NULL CHECK (state_hash ~ '^[0-9a-f]{64}$'),
  connection  text NOT NULL CHECK (connection ~ '^[A-Za-z0-9_-]{1,64}$'),
  reviewer    boolean NOT NULL,
  -- a relative path of the Workspace: one slash then not another slash or a backslash, no control characters
  return_path text NOT NULL CHECK (length(return_path) BETWEEN 1 AND 512 AND left(return_path, 1) = '/'
              AND substr(return_path, 2, 1) NOT IN ('/', E'\\') AND position(E'\\' in return_path) = 0 AND return_path !~ '[[:cntrl:]]'),
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, subject, state_hash)
);
CREATE INDEX connection_pending_expiry ON connection_pending (expires_at);
ALTER TABLE connection_pending ENABLE ROW LEVEL SECURITY;
ALTER TABLE connection_pending FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON connection_pending USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, DELETE ON connection_pending TO idp_ws_app;
