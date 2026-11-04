package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"idp-workspace/internal/workspace"
)

const invocationCols = `id::text, workspace_id::text, subject, app, app_version, action, coalesce(item_id::text, ''), input_digest, target, status, result, error, created_at`

func scanInvocation(row pgx.Row) (*workspace.Invocation, error) {
	var v workspace.Invocation
	var target, result, perr []byte
	if err := row.Scan(&v.ID, &v.WorkspaceID, &v.Subject, &v.App, &v.AppVersion, &v.Action, &v.ItemID, &v.InputDigest, &target, &v.Status, &result, &perr, &v.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "invocation not found")
		}
		return nil, err
	}
	if err := json.Unmarshal(target, &v.Target); err != nil {
		return nil, err
	}
	if result != nil {
		v.Result = result
	}
	if perr != nil {
		v.Error = &workspace.Problem{}
		if err := json.Unmarshal(perr, v.Error); err != nil {
			return nil, err
		}
	}
	return &v, nil
}

// PriorInvocation returns the invocation recorded under subject's idempotency key, with the hash
// of the request that made it; nil when there is none.
func (s *Store) PriorInvocation(ctx context.Context, tenant, subject, key string) (*workspace.Invocation, string, error) {
	if key == "" {
		return nil, "", nil
	}
	var out *workspace.Invocation
	var hash string
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT request_sha256 FROM action_invocations WHERE subject = $1 AND idempotency_key = $2`, subject, key).Scan(&hash); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		var err error
		out, err = scanInvocation(tx.QueryRow(ctx, `SELECT `+invocationCols+` FROM action_invocations WHERE subject = $1 AND idempotency_key = $2`, subject, key))
		return err
	})
	return out, hash, err
}

// RecordInvocation stores an invocation. A concurrent request with the same key loses: the stored
// one is returned instead (replayed = true).
func (s *Store) RecordInvocation(ctx context.Context, tenant string, v workspace.Invocation, key, requestHash string) (out *workspace.Invocation, replayed bool, err error) {
	target, _ := json.Marshal(v.Target)
	var perr []byte
	if v.Error != nil {
		perr, _ = json.Marshal(v.Error)
	}
	var result []byte
	if len(v.Result) > 0 {
		result = v.Result
	}
	var keyArg, itemArg any
	if key != "" {
		keyArg = key
	}
	if v.ItemID != "" {
		itemArg = v.ItemID
	}
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO action_invocations (id, tenant_id, workspace_id, subject, app, app_version, action, item_id, input_digest, target, status, result, error, idempotency_key, request_sha256)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) ON CONFLICT (tenant_id, subject, idempotency_key) DO NOTHING`,
			v.ID, tenant, v.WorkspaceID, v.Subject, v.App, v.AppVersion, v.Action, itemArg, v.InputDigest, target, v.Status, result, perr, keyArg, requestHash)
		if err != nil {
			return err
		}
		id := v.ID
		if tag.RowsAffected() == 0 {
			replayed = true
			if err := tx.QueryRow(ctx, `SELECT id::text FROM action_invocations WHERE subject = $1 AND idempotency_key = $2`, v.Subject, key).Scan(&id); err != nil {
				return err
			}
		}
		out, err = scanInvocation(tx.QueryRow(ctx, `SELECT `+invocationCols+` FROM action_invocations WHERE id = $1`, id))
		return err
	})
	return out, replayed, err
}

// ListInvocations lists a workspace's invocations, of one subject when given.
func (s *Store) ListInvocations(ctx context.Context, tenant, workspaceID, subject string, limit int) ([]workspace.Invocation, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []workspace.Invocation{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+invocationCols+` FROM action_invocations WHERE workspace_id = $1 AND ($2 = '' OR subject = $2) ORDER BY created_at DESC LIMIT $3`, workspaceID, subject, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanInvocation(rows)
			if err != nil {
				return err
			}
			out = append(out, *v)
		}
		return rows.Err()
	})
	return out, err
}
