package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-workspace/internal/workspace"
)

const releaseCols = `name, version, digest, status, manifest, published_by, published_at, revoked_by, revoked_at, revoked_reason`

func scanRelease(row pgx.Row) (*workspace.Release, error) {
	var r workspace.Release
	var raw []byte
	var by, reason *string
	var at *time.Time
	if err := row.Scan(&r.Name, &r.Version, &r.Digest, &r.Status, &raw, &r.PublishedBy, &r.PublishedAt, &by, &at, &reason); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "app release not found")
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, &r.Manifest); err != nil {
		return nil, err
	}
	if by != nil && at != nil {
		r.Revocation = &workspace.Revocation{By: *by, At: *at, Reason: deref(reason)}
	}
	return &r, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// PublishRelease records an app release. The same name and version may be published again only
// with the same digest (an idempotent retry, created = false); anything else is a conflict.
func (s *Store) PublishRelease(ctx context.Context, tenant string, m workspace.Manifest, raw []byte, digest, by string) (out *workspace.Release, created bool, err error) {
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		existing, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM app_releases WHERE name = $1 AND version = $2`, m.Name, m.Version))
		switch {
		case err == nil:
			if existing.Digest != digest {
				return fail("CONFLICT", "%s %s is published with digest %s; releases are immutable", m.Name, m.Version, existing.Digest)
			}
			out, created = existing, false
			return nil
		case !isNotFound(err):
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO app_releases (tenant_id, name, version, digest, manifest, published_by) VALUES ($1, $2, $3, $4, $5, $6)`,
			tenant, m.Name, m.Version, digest, raw, by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "app.publish", m.Name+"@"+m.Version, map[string]any{"digest": digest}); err != nil {
			return err
		}
		out, err = scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM app_releases WHERE name = $1 AND version = $2`, m.Name, m.Version))
		created = true
		return err
	})
	return out, created, err
}

func isNotFound(err error) bool {
	var se *Error
	return errors.As(err, &se) && se.Code == "NOT_FOUND"
}

// GetRelease reads one release.
func (s *Store) GetRelease(ctx context.Context, tenant, name, version string) (*workspace.Release, error) {
	var out *workspace.Release
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		out, err = scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM app_releases WHERE name = $1 AND version = $2`, name, version))
		return err
	})
	return out, err
}

// ListReleases lists releases, of one app when name is given.
func (s *Store) ListReleases(ctx context.Context, tenant, name string) ([]workspace.Release, error) {
	out := []workspace.Release{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+releaseCols+` FROM app_releases WHERE $1 = '' OR name = $1 ORDER BY name, published_at DESC`, name)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRelease(rows)
			if err != nil {
				return err
			}
			out = append(out, *r)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeRelease withdraws a release: installations of it stay but can no longer be used.
func (s *Store) RevokeRelease(ctx context.Context, tenant, name, version, reason, by string) (*workspace.Release, error) {
	if strings.TrimSpace(reason) == "" || len(reason) > 500 {
		return nil, fail("INVALID", "reason is required (at most 500 characters)")
	}
	var out *workspace.Release
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE app_releases SET status = 'revoked', revoked_by = $3, revoked_at = now(), revoked_reason = $4 WHERE name = $1 AND version = $2 AND status = 'published'`, name, version, by, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if _, err := scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM app_releases WHERE name = $1 AND version = $2`, name, version)); err != nil {
				return err
			}
			return fail("CONFLICT", "%s %s is already revoked", name, version)
		}
		if err := Audit(ctx, tx, tenant, by, "app.revoke", name+"@"+version, map[string]any{"reason": reason}); err != nil {
			return err
		}
		out, err = scanRelease(tx.QueryRow(ctx, `SELECT `+releaseCols+` FROM app_releases WHERE name = $1 AND version = $2`, name, version))
		return err
	})
	return out, err
}

const installationCols = `i.id::text, i.workspace_id::text, i.app, i.version, i.config, i.revision, i.installed_by, i.installed_at, i.updated_at, r.status`

func scanInstallation(row pgx.Row) (*workspace.Installation, error) {
	var in workspace.Installation
	if err := row.Scan(&in.ID, &in.WorkspaceID, &in.App, &in.Version, &in.Config, &in.Revision, &in.InstalledBy, &in.InstalledAt, &in.UpdatedAt, &in.ReleaseStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "installation not found")
		}
		return nil, err
	}
	return &in, nil
}

const installationFrom = ` FROM installations i JOIN app_releases r ON r.tenant_id = i.tenant_id AND r.name = i.app AND r.version = i.version`

// Install puts an app release into a workspace, or moves an existing installation to another
// version or configuration (guarded by ifMatch when non-zero). The caller validated the configuration.
func (s *Store) Install(ctx context.Context, tenant, workspaceID, app, version string, config json.RawMessage, by string, ifMatch int) (out *workspace.Installation, created bool, err error) {
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM app_releases WHERE name = $1 AND version = $2`, app, version).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fail("NOT_FOUND", "%s %s is not published", app, version)
			}
			return err
		}
		if status != "published" {
			return fail("CONFLICT", "%s %s is revoked", app, version)
		}
		var id string
		var revision int
		err := tx.QueryRow(ctx, `SELECT id::text, revision FROM installations WHERE workspace_id = $1 AND app = $2 FOR UPDATE`, workspaceID, app).Scan(&id, &revision)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if ifMatch != 0 {
				return fail("CONFLICT", "%s is not installed", app)
			}
			id, created = NewUUID(), true
			if _, err := tx.Exec(ctx, `INSERT INTO installations (id, tenant_id, workspace_id, app, version, config, installed_by) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				id, tenant, workspaceID, app, version, []byte(config), by); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if ifMatch != 0 && ifMatch != revision {
				return fail("CONFLICT", "installation is at revision %d, not %d", revision, ifMatch)
			}
			if _, err := tx.Exec(ctx, `UPDATE installations SET version = $2, config = $3, revision = revision + 1, updated_at = now() WHERE id = $1`, id, version, []byte(config)); err != nil {
				return err
			}
		}
		if err := Audit(ctx, tx, tenant, by, "app.install", id, map[string]any{"workspaceId": workspaceID, "app": app, "version": version}); err != nil {
			return err
		}
		out, err = scanInstallation(tx.QueryRow(ctx, `SELECT `+installationCols+installationFrom+` WHERE i.id = $1`, id))
		return err
	})
	return out, created, err
}

// GetInstallation reads the installation of app in a workspace.
func (s *Store) GetInstallation(ctx context.Context, tenant, workspaceID, app string) (*workspace.Installation, error) {
	var out *workspace.Installation
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		out, err = scanInstallation(tx.QueryRow(ctx, `SELECT `+installationCols+installationFrom+` WHERE i.workspace_id = $1 AND i.app = $2`, workspaceID, app))
		return err
	})
	return out, err
}

// ListInstallations lists a workspace's installations.
func (s *Store) ListInstallations(ctx context.Context, tenant, workspaceID string) ([]workspace.Installation, error) {
	out := []workspace.Installation{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+installationCols+installationFrom+` WHERE i.workspace_id = $1 ORDER BY i.app`, workspaceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			in, err := scanInstallation(rows)
			if err != nil {
				return err
			}
			out = append(out, *in)
		}
		return rows.Err()
	})
	return out, err
}

// Uninstall removes an app from a workspace.
func (s *Store) Uninstall(ctx context.Context, tenant, workspaceID, app, by string) error {
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `DELETE FROM installations WHERE workspace_id = $1 AND app = $2 RETURNING id::text`, workspaceID, app).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fail("NOT_FOUND", "%s is not installed", app)
			}
			return err
		}
		return Audit(ctx, tx, tenant, by, "app.uninstall", id, map[string]any{"workspaceId": workspaceID, "app": app})
	})
}
