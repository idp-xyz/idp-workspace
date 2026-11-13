-- idp-workspace schema v3 (ADR-0001 D23; platform ADR-0002 F1): a member's files. A file belongs to its owner (one
-- subject of the tenant); it is kept in the workspace it was uploaded or delivered in, comes from an upload or from a
-- run, and expires. The content is kept here (PostgreSQL, at most 25 MB a file by default); listings never read it.
CREATE TABLE member_files (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  owner        text NOT NULL CHECK (length(owner) BETWEEN 1 AND 256),
  name         text NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
  media_type   text NOT NULL CHECK (length(media_type) BETWEEN 3 AND 255),
  size         bigint NOT NULL CHECK (size >= 0),
  sha256       text NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  source       text NOT NULL CHECK (source = 'upload' OR source ~ '^run:[A-Za-z0-9_.:-]{1,128}$'),
  sensitivity  text NOT NULL DEFAULT 'internal' CHECK (sensitivity IN ('public', 'internal', 'confidential', 'restricted')),
  content      bytea NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  CHECK (octet_length(content) = size)
);
CREATE INDEX member_files_owner ON member_files (tenant_id, workspace_id, owner, created_at DESC);
CREATE INDEX member_files_run ON member_files (tenant_id, source) WHERE source <> 'upload';
CREATE INDEX member_files_expiry ON member_files (expires_at);
ALTER TABLE member_files ENABLE ROW LEVEL SECURITY;
ALTER TABLE member_files FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON member_files USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, DELETE ON member_files TO idp_ws_app;
