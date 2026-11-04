-- idp-workspace schema v1 (ADR-0001 D1–D8). Run by the schema owner (`idp-workspace migrate`). The
-- service connects as a login role that is a member of idp_ws_app: not a superuser, no BYPASSRLS.

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'idp_ws_app') THEN
    CREATE ROLE idp_ws_app NOLOGIN NOBYPASSRLS;
  END IF;
END $$;

CREATE FUNCTION current_tenant() RETURNS uuid LANGUAGE sql STABLE AS $$
  SELECT nullif(current_setting('app.tenant_id', true), '')::uuid
$$;

CREATE TABLE tenants (
  id         uuid PRIMARY KEY,
  slug       text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
  created_at timestamptz NOT NULL DEFAULT now()
);

-- D1: a workspace is where a team works; members see its inbox, apps and context.
CREATE TABLE workspaces (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  name         text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
  display_name text NOT NULL CHECK (length(display_name) BETWEEN 1 AND 200),
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  archived_at  timestamptz,
  UNIQUE (tenant_id, name)
);

CREATE TABLE workspace_members (
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  subject      text NOT NULL CHECK (length(subject) BETWEEN 1 AND 256),
  roles        text[] NOT NULL DEFAULT '{}',   -- workspace-local roles, matched by inbox assignees and action contracts
  added_by     text NOT NULL,
  added_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, subject)
);

-- D2: the app registry. A release is immutable; its digest is the sha256 of the canonical manifest.
CREATE TABLE app_releases (
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  name         text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
  version      text NOT NULL CHECK (length(version) <= 64),
  digest       text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  manifest     jsonb NOT NULL,
  status       text NOT NULL DEFAULT 'published' CHECK (status IN ('published', 'revoked')),
  published_by text NOT NULL,
  published_at timestamptz NOT NULL DEFAULT now(),
  revoked_by   text,
  revoked_at   timestamptz,
  revoked_reason text,
  PRIMARY KEY (tenant_id, name, version)
);

-- D2: an app release installed into a workspace, with its configuration (validated against the
-- manifest's config schema). One installation per app and workspace; upgrading moves the version.
CREATE TABLE installations (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  app          text NOT NULL,
  version      text NOT NULL,
  config       jsonb NOT NULL DEFAULT '{}',
  installed_by text NOT NULL,
  installed_at timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  revision     int NOT NULL DEFAULT 1,
  UNIQUE (workspace_id, app),
  FOREIGN KEY (tenant_id, app, version) REFERENCES app_releases(tenant_id, name, version)
);

-- D3: the work inbox. An item is a view of something another system owns (an EER approval, a HOST
-- run, an app's task); the producer creates and resolves it, the workspace never decides it.
CREATE TABLE inbox_items (
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  producer      text NOT NULL,                 -- the authenticated producer (subject)
  external_ref  text NOT NULL CHECK (length(external_ref) BETWEEN 1 AND 256),  -- the producer's key: idempotent upsert
  kind          text NOT NULL CHECK (kind IN ('approval', 'task', 'run', 'notification')),
  title         text NOT NULL CHECK (length(title) BETWEEN 1 AND 300),
  summary       text NOT NULL DEFAULT '' CHECK (length(summary) <= 4000),
  ref           jsonb NOT NULL,                -- {system, kind, id, url?}: the authoritative object
  assignee_subjects text[] NOT NULL DEFAULT '{}',
  assignee_roles    text[] NOT NULL DEFAULT '{}',  -- both empty = every member of the workspace
  priority      text NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high', 'urgent')),
  due_at        timestamptz,
  actions       jsonb NOT NULL DEFAULT '[]',   -- [{app, action, label?, input?}]: what a member may do from the item
  entities      jsonb NOT NULL DEFAULT '[]',   -- [{type, id}]: business objects the item is about
  status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'resolved', 'withdrawn')),
  resolution    jsonb,                         -- {outcome, by, at, note?}, set by the producer
  version       int NOT NULL DEFAULT 1,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (workspace_id, producer, external_ref)
);
CREATE INDEX inbox_items_open ON inbox_items (workspace_id, status, updated_at DESC);

-- per member: read and archived marks (the member's own view of the item, never its outcome)
CREATE TABLE inbox_marks (
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  item_id     uuid NOT NULL REFERENCES inbox_items(id) ON DELETE CASCADE,
  subject     text NOT NULL,
  read_at     timestamptz,
  archived_at timestamptz,
  PRIMARY KEY (item_id, subject)
);

-- D5: what a member is working on in a workspace (business objects, the app in front, a thread).
CREATE TABLE work_contexts (
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  subject      text NOT NULL,
  context      jsonb NOT NULL,
  version      int NOT NULL DEFAULT 1,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (workspace_id, subject)
);

-- D4: every action a member invoked, what it was forwarded to and how it ended. Idempotent per
-- (subject, idempotency key).
CREATE TABLE action_invocations (
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  subject       text NOT NULL,
  app           text NOT NULL,
  app_version   text NOT NULL,
  action        text NOT NULL,
  item_id       uuid REFERENCES inbox_items(id) ON DELETE SET NULL,
  input_digest  text NOT NULL,
  target        jsonb NOT NULL,                -- {kind, ...}: where it was forwarded
  status        text NOT NULL CHECK (status IN ('succeeded', 'failed')),
  result        jsonb,                         -- the target's reference (run id, decision id, status code)
  error         jsonb,                         -- {code, message} when failed
  idempotency_key text,
  request_sha256  text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, subject, idempotency_key)
);

CREATE TABLE audit_events (
  id          bigserial PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  at          timestamptz NOT NULL DEFAULT now(),
  subject     text NOT NULL,
  action      text NOT NULL,
  resource_id text,
  detail      jsonb
);

-- D7: every tenant table is bound to the transaction's tenant; the service role cannot bypass it.
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['workspaces', 'workspace_members', 'app_releases', 'installations', 'inbox_items', 'inbox_marks', 'work_contexts', 'action_invocations', 'audit_events'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_isolation ON %I USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant())', t);
  END LOOP;
END $$;

GRANT SELECT ON tenants TO idp_ws_app;
GRANT SELECT, INSERT, UPDATE ON workspaces, installations, inbox_items, work_contexts TO idp_ws_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_members, inbox_marks TO idp_ws_app;
GRANT DELETE ON installations TO idp_ws_app;
GRANT SELECT, INSERT, UPDATE ON app_releases TO idp_ws_app;
GRANT SELECT, INSERT ON action_invocations, audit_events TO idp_ws_app;
GRANT USAGE ON SEQUENCE audit_events_id_seq TO idp_ws_app;
