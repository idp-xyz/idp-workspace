package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"idp-workspace/internal/workspace"
)

// MaxPendingConnects is how many unfinished connects one member keeps: the oldest go (a member who opens the page in
// several tabs is fine; a loop that starts connects is bounded).
const MaxPendingConnects = 20

// PutPendingConnect records a connect the member started (ADR-0001 D30). What has expired goes first, and past
// MaxPendingConnects the oldest. The hash is the primary key: a second connect with the same state is refused.
func (s *Store) PutPendingConnect(ctx context.Context, tenant, subject string, p workspace.PendingConnect) error {
	if subject == "" {
		return fail("FORBIDDEN", "subject required")
	}
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM connection_pending WHERE subject = $1 AND expires_at <= $2`, subject, s.now()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM connection_pending WHERE subject = $1 AND state_hash IN
			(SELECT state_hash FROM connection_pending WHERE subject = $1 ORDER BY created_at DESC, state_hash OFFSET $2)`, subject, MaxPendingConnects-1); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO connection_pending (tenant_id, subject, state_hash, connection, reviewer, return_path, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, tenant, subject, p.StateHash, p.Connection, p.Reviewer, p.ReturnPath, s.now(), p.ExpiresAt)
		if isUniqueViolation(err) {
			return fail("CONFLICT", "this authorization was already started")
		}
		return err
	})
}

// TakePendingConnect consumes the member's pending connect with this state hash: the row is deleted in the same statement
// that reads it, so two callbacks cannot both have it. nil when the member has none (it never was theirs, was used, or
// was purged). A row that expired is consumed too and returned: the caller says it expired.
func (s *Store) TakePendingConnect(ctx context.Context, tenant, subject, stateHash string) (*workspace.PendingConnect, error) {
	var out *workspace.PendingConnect
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var p workspace.PendingConnect
		err := tx.QueryRow(ctx, `DELETE FROM connection_pending WHERE subject = $1 AND state_hash = $2
			RETURNING state_hash, connection, reviewer, return_path, expires_at`, subject, stateHash).Scan(&p.StateHash, &p.Connection, &p.Reviewer, &p.ReturnPath, &p.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = &p
		return nil
	})
	return out, err
}
