package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"idp-workspace/internal/workspace"
)

const workspaceCols = `id::text, name, display_name, created_by, created_at, archived_at`

func scanWorkspace(row pgx.Row) (*workspace.Workspace, error) {
	var w workspace.Workspace
	if err := row.Scan(&w.ID, &w.Name, &w.DisplayName, &w.CreatedBy, &w.CreatedAt, &w.ArchivedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "workspace not found")
		}
		return nil, err
	}
	return &w, nil
}

// CreateWorkspace creates a workspace; its creator becomes its first member with role owner.
func (s *Store) CreateWorkspace(ctx context.Context, tenant, name, displayName, by string) (*workspace.Workspace, error) {
	if !workspace.Name.MatchString(name) {
		return nil, fail("INVALID", "name must be a lowercase name")
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || len(displayName) > 200 {
		return nil, fail("INVALID", "displayName is required (at most 200 characters)")
	}
	var out *workspace.Workspace
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		id := NewUUID()
		if _, err := tx.Exec(ctx, `INSERT INTO workspaces (id, tenant_id, name, display_name, created_by) VALUES ($1, $2, $3, $4, $5)`, id, tenant, name, displayName, by); err != nil {
			if isUniqueViolation(err) {
				return fail("CONFLICT", "a workspace named %s exists", name)
			}
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_members (tenant_id, workspace_id, subject, roles, added_by) VALUES ($1, $2, $3, '{owner}', $3)`, tenant, id, by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "workspace.create", id, map[string]any{"name": name}); err != nil {
			return err
		}
		var err error
		out, err = scanWorkspace(tx.QueryRow(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE id = $1`, id))
		return err
	})
	return out, err
}

// resolveWorkspace accepts a workspace id or name.
func resolveWorkspace(ctx context.Context, tx pgx.Tx, ref string) (*workspace.Workspace, error) {
	if IsUUID(ref) {
		return scanWorkspace(tx.QueryRow(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE id = $1`, ref))
	}
	return scanWorkspace(tx.QueryRow(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE name = $1`, ref))
}

// GetWorkspace reads a workspace by id or name.
func (s *Store) GetWorkspace(ctx context.Context, tenant, ref string) (*workspace.Workspace, error) {
	var out *workspace.Workspace
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		out, err = resolveWorkspace(ctx, tx, ref)
		return err
	})
	return out, err
}

// ListWorkspaces lists the tenant's workspaces (all = every one; otherwise those subject belongs to).
func (s *Store) ListWorkspaces(ctx context.Context, tenant, subject string, all bool) ([]workspace.Workspace, error) {
	out := []workspace.Workspace{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+workspaceCols+` FROM workspaces w
			WHERE archived_at IS NULL AND ($1 OR EXISTS (SELECT 1 FROM workspace_members m WHERE m.workspace_id = w.id AND m.subject = $2)) ORDER BY name`, all, subject)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			w, err := scanWorkspace(rows)
			if err != nil {
				return err
			}
			out = append(out, *w)
		}
		return rows.Err()
	})
	return out, err
}

// Membership is a subject's standing in a workspace.
type Membership struct {
	Workspace workspace.Workspace
	Member    bool
	Roles     []string
}

// MembershipOf resolves the workspace and the subject's membership in it.
func (s *Store) MembershipOf(ctx context.Context, tenant, ref, subject string) (*Membership, error) {
	var out *Membership
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		w, err := resolveWorkspace(ctx, tx, ref)
		if err != nil {
			return err
		}
		out = &Membership{Workspace: *w, Roles: []string{}}
		err = tx.QueryRow(ctx, `SELECT roles FROM workspace_members WHERE workspace_id = $1 AND subject = $2`, w.ID, subject).Scan(&out.Roles)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		out.Member = true
		return nil
	})
	return out, err
}

// PutMember adds a member or replaces its workspace roles.
func (s *Store) PutMember(ctx context.Context, tenant, workspaceID, subject string, roles []string, by string) (*workspace.Member, error) {
	if subject == "" || len(subject) > 256 {
		return nil, fail("INVALID", "subject is required (at most 256 characters)")
	}
	if roles == nil {
		roles = []string{}
	}
	for _, r := range roles {
		if !workspace.Name.MatchString(r) {
			return nil, fail("INVALID", "role %q is not a lowercase name", r)
		}
	}
	var out workspace.Member
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO workspace_members (tenant_id, workspace_id, subject, roles, added_by) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (workspace_id, subject) DO UPDATE SET roles = EXCLUDED.roles
			RETURNING subject, roles, added_by, added_at`, tenant, workspaceID, subject, roles, by).Scan(&out.Subject, &out.Roles, &out.AddedBy, &out.AddedAt); err != nil {
			return err
		}
		return Audit(ctx, tx, tenant, by, "member.put", workspaceID, map[string]any{"subject": subject, "roles": roles})
	})
	return &out, err
}

// RemoveMember removes a member.
func (s *Store) RemoveMember(ctx context.Context, tenant, workspaceID, subject, by string) error {
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM workspace_members WHERE workspace_id = $1 AND subject = $2`, workspaceID, subject)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fail("NOT_FOUND", "%s is not a member", subject)
		}
		return Audit(ctx, tx, tenant, by, "member.remove", workspaceID, map[string]any{"subject": subject})
	})
}

// ListMembers lists a workspace's members.
func (s *Store) ListMembers(ctx context.Context, tenant, workspaceID string) ([]workspace.Member, error) {
	out := []workspace.Member{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT subject, roles, added_by, added_at FROM workspace_members WHERE workspace_id = $1 ORDER BY subject`, workspaceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m workspace.Member
			if err := rows.Scan(&m.Subject, &m.Roles, &m.AddedBy, &m.AddedAt); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}
