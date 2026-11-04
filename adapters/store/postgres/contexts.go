package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"idp-workspace/internal/workspace"
)

type contextBody struct {
	Entities []workspace.Entity `json:"entities"`
	App      string             `json:"app,omitempty"`
	Thread   string             `json:"thread,omitempty"`
	ItemID   string             `json:"itemId,omitempty"`
}

// GetContext reads subject's work context in a workspace (an empty one at version 0 if never set).
func (s *Store) GetContext(ctx context.Context, tenant, workspaceID, subject string) (*workspace.WorkContext, error) {
	out := &workspace.WorkContext{WorkspaceID: workspaceID, Subject: subject, Entities: []workspace.Entity{}}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT context, version, updated_at FROM work_contexts WHERE workspace_id = $1 AND subject = $2`, workspaceID, subject).Scan(&raw, &out.Version, &out.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var b contextBody
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		out.Entities, out.App, out.Thread, out.ItemID = b.Entities, b.App, b.Thread, b.ItemID
		if out.Entities == nil {
			out.Entities = []workspace.Entity{}
		}
		return nil
	})
	return out, err
}

// SetContext replaces subject's work context, guarded by the expected version when ifMatch >= 0
// (0 = must not exist yet); ifMatch < 0 means unconditional.
func (s *Store) SetContext(ctx context.Context, tenant, workspaceID, subject string, in workspace.WorkContext, ifMatch int) (*workspace.WorkContext, error) {
	if err := workspace.ValidEntities(in.Entities); err != nil {
		return nil, fail("INVALID", "%s", err.Error())
	}
	if in.App != "" && !workspace.Name.MatchString(in.App) {
		return nil, fail("INVALID", "app must be an app name")
	}
	if len(in.Thread) > 256 {
		return nil, fail("INVALID", "thread is at most 256 characters")
	}
	if in.ItemID != "" && !IsUUID(in.ItemID) {
		return nil, fail("INVALID", "itemId must be an inbox item id")
	}
	if in.Entities == nil {
		in.Entities = []workspace.Entity{}
	}
	raw, _ := json.Marshal(contextBody{Entities: in.Entities, App: in.App, Thread: in.Thread, ItemID: in.ItemID})
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var current int
		err := tx.QueryRow(ctx, `SELECT version FROM work_contexts WHERE workspace_id = $1 AND subject = $2 FOR UPDATE`, workspaceID, subject).Scan(&current)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if ifMatch >= 0 && ifMatch != current {
			return fail("CONFLICT", "work context is at version %d, not %d", current, ifMatch)
		}
		_, err = tx.Exec(ctx, `INSERT INTO work_contexts (tenant_id, workspace_id, subject, context) VALUES ($1, $2, $3, $4)
			ON CONFLICT (workspace_id, subject) DO UPDATE SET context = EXCLUDED.context, version = work_contexts.version + 1, updated_at = now()`, tenant, workspaceID, subject, raw)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetContext(ctx, tenant, workspaceID, subject)
}
