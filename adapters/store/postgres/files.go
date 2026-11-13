package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-workspace/internal/workspace"
)

// FileInput is a file to keep (D23).
type FileInput struct {
	WorkspaceID string
	Owner       string
	Name        string
	MediaType   string
	Source      string // upload | run:<runId>
	Content     []byte
	Retention   time.Duration
	// limits: the owner's total bytes and files in the tenant, and the files one run may deliver (0 = none)
	QuotaBytes int64
	QuotaFiles int
	RunFiles   int
}

const fileCols = `f.id::text, w.name, f.name, f.media_type, f.size, f.sha256, f.source, f.sensitivity, f.created_at, f.expires_at`

func scanFile(row pgx.Row) (*workspace.MemberFile, error) {
	var f workspace.MemberFile
	if err := row.Scan(&f.ID, &f.Workspace, &f.Name, &f.MediaType, &f.Size, &f.SHA256, &f.Source, &f.Sensitivity, &f.CreatedAt, &f.ExpiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "file not found")
		}
		return nil, err
	}
	return &f, nil
}

// PutFile keeps a file for its owner. The owner's quota (and, for a run's deliverable, the run's limit) is checked
// under a lock on the owner, so concurrent uploads cannot both pass it.
func (s *Store) PutFile(ctx context.Context, tenant string, in FileInput) (*workspace.MemberFile, error) {
	sum := sha256.Sum256(in.Content)
	id := NewUUID()
	var out *workspace.MemberFile
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('member_files:' || $1 || ':' || $2, 0))`, tenant, in.Owner); err != nil {
			return err
		}
		var bytes int64
		var count int
		if err := tx.QueryRow(ctx, `SELECT coalesce(sum(size), 0), count(*) FROM member_files WHERE owner = $1 AND expires_at > now()`, in.Owner).Scan(&bytes, &count); err != nil {
			return err
		}
		if in.QuotaFiles > 0 && count+1 > in.QuotaFiles || in.QuotaBytes > 0 && bytes+int64(len(in.Content)) > in.QuotaBytes {
			return fail("QUOTA", "your files would exceed your quota (%d files, %d MB); delete some first", in.QuotaFiles, in.QuotaBytes>>20)
		}
		if in.RunFiles > 0 {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM member_files WHERE source = $1`, in.Source).Scan(&n); err != nil {
				return err
			}
			if n >= in.RunFiles {
				return fail("QUOTA", "the run already delivered %d files", n)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO member_files (id, tenant_id, workspace_id, owner, name, media_type, size, sha256, source, content, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now() + make_interval(secs => $11))`,
			id, tenant, in.WorkspaceID, in.Owner, in.Name, in.MediaType, len(in.Content), hex.EncodeToString(sum[:]), in.Source, in.Content, in.Retention.Seconds()); err != nil {
			return err
		}
		var err error
		out, err = scanFile(tx.QueryRow(ctx, `SELECT `+fileCols+` FROM member_files f JOIN workspaces w ON w.id = f.workspace_id WHERE f.id = $1`, id))
		return err
	})
	return out, err
}

// ListFiles lists the owner's files in a workspace, newest first; source filters (upload, run:<id>) when set.
func (s *Store) ListFiles(ctx context.Context, tenant, workspaceID, owner, source string, limit int) ([]workspace.MemberFile, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	out := []workspace.MemberFile{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+fileCols+` FROM member_files f JOIN workspaces w ON w.id = f.workspace_id
			WHERE f.workspace_id = $1 AND f.owner = $2 AND f.expires_at > now() AND ($3 = '' OR f.source = $3) ORDER BY f.created_at DESC LIMIT $4`, workspaceID, owner, source, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			f, err := scanFile(rows)
			if err != nil {
				return err
			}
			out = append(out, *f)
		}
		return rows.Err()
	})
	return out, err
}

// GetFile returns one of the owner's files (workspaceID "" = in any workspace of the tenant), with its content when asked.
func (s *Store) GetFile(ctx context.Context, tenant, workspaceID, owner, id string, content bool) (*workspace.MemberFile, []byte, error) {
	if !uuidRE.MatchString(id) {
		return nil, nil, fail("NOT_FOUND", "file not found")
	}
	var f *workspace.MemberFile
	var data []byte
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		f, err = scanFile(tx.QueryRow(ctx, `SELECT `+fileCols+` FROM member_files f JOIN workspaces w ON w.id = f.workspace_id
			WHERE f.id = $1 AND f.owner = $2 AND ($3 = '' OR f.workspace_id::text = $3) AND f.expires_at > now()`, id, owner, workspaceID))
		if err != nil || !content {
			return err
		}
		return tx.QueryRow(ctx, `SELECT content FROM member_files WHERE id = $1`, id).Scan(&data)
	})
	return f, data, err
}

// DeleteFile deletes one of the owner's files in a workspace.
func (s *Store) DeleteFile(ctx context.Context, tenant, workspaceID, owner, id string) error {
	if !uuidRE.MatchString(id) {
		return fail("NOT_FOUND", "file not found")
	}
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM member_files WHERE id = $1 AND workspace_id = $2 AND owner = $3`, id, workspaceID, owner)
		if err == nil && tag.RowsAffected() == 0 {
			return fail("NOT_FOUND", "file not found")
		}
		return err
	})
}

// PurgeExpiredFiles deletes the files past their expiry in every tenant; it returns how many.
func (s *Store) PurgeExpiredFiles(ctx context.Context) (int64, error) {
	var tenants []string
	rows, err := s.Pool.Query(ctx, `SELECT id::text FROM tenants`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return 0, err
		}
		tenants = append(tenants, t)
	}
	rows.Close()
	var n int64
	for _, t := range tenants {
		err := s.InTenant(ctx, t, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM member_files WHERE expires_at <= now()`)
			n += tag.RowsAffected()
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
