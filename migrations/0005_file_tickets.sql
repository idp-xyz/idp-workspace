-- idp-workspace schema v5 (ADR-0001 D27): what became of a run's file tickets. A read ticket is spent when it is
-- presented (once, by default), and a run that has ended takes its tickets with it. Both tables hold rows only until
-- the tickets they are about would have expired anyway.
CREATE TABLE file_ticket_uses (
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  jti        text NOT NULL CHECK (length(jti) BETWEEN 1 AND 128),
  run        text NOT NULL CHECK (length(run) BETWEEN 1 AND 160),
  uses       integer NOT NULL CHECK (uses >= 1),
  expires_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, jti)
);
CREATE INDEX file_ticket_uses_expiry ON file_ticket_uses (expires_at);
ALTER TABLE file_ticket_uses ENABLE ROW LEVEL SECURITY;
ALTER TABLE file_ticket_uses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON file_ticket_uses USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, UPDATE, DELETE ON file_ticket_uses TO idp_ws_app;

CREATE TABLE file_runs_ended (
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  run        text NOT NULL CHECK (length(run) BETWEEN 1 AND 160),
  ended_at   timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  PRIMARY KEY (tenant_id, run)
);
CREATE INDEX file_runs_ended_expiry ON file_runs_ended (expires_at);
ALTER TABLE file_runs_ended ENABLE ROW LEVEL SECURITY;
ALTER TABLE file_runs_ended FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON file_runs_ended USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, DELETE ON file_runs_ended TO idp_ws_app;
