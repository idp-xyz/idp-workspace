package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-workspace/adapters/blob"
	"idp-workspace/internal/workspace"
)

// Blobs keeps file contents outside the database (D24); nil = in the database.
type Blobs interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string, max int64) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// BlobLister also lists what it keeps, so that objects without a row (an upload whose transaction failed, a delete
// whose object call failed) can be swept.
type BlobLister interface {
	List(ctx context.Context, prefix, token string) ([]blob.Object, string, error)
}

// ObjectKey is where a file's content lives in the object store.
func ObjectKey(tenant, id string) string { return "files/" + tenant + "/" + id }

// ErrFileCorrupt: what the object store returned is not the file whose checksum the database holds.
var ErrFileCorrupt = errors.New("the stored file does not match its checksum")

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
// under a lock on the owner, so concurrent uploads cannot both pass it. With an object store the content goes there,
// inside the transaction and before the row: a row never names a missing object, and an object whose row did not
// commit is deleted at once (or swept later).
func (s *Store) PutFile(ctx context.Context, tenant string, in FileInput) (*workspace.MemberFile, error) {
	sum := sha256.Sum256(in.Content)
	id := NewUUID()
	var out *workspace.MemberFile
	uploaded := ""
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
		content, key := any(in.Content), any(nil)
		if s.Blobs != nil {
			k := ObjectKey(tenant, id)
			if err := s.Blobs.Put(ctx, k, in.Content); err != nil {
				return err
			}
			uploaded, content, key = k, nil, k
		}
		if _, err := tx.Exec(ctx, `INSERT INTO member_files (id, tenant_id, workspace_id, owner, name, media_type, size, sha256, source, content, object_key, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now() + make_interval(secs => $12))`,
			id, tenant, in.WorkspaceID, in.Owner, in.Name, in.MediaType, len(in.Content), hex.EncodeToString(sum[:]), in.Source, content, key, in.Retention.Seconds()); err != nil {
			return err
		}
		var err error
		out, err = scanFile(tx.QueryRow(ctx, `SELECT `+fileCols+` FROM member_files f JOIN workspaces w ON w.id = f.workspace_id WHERE f.id = $1`, id))
		return err
	})
	if err != nil && uploaded != "" {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = s.Blobs.Delete(dctx, uploaded) // what is left over, the sweep finds
		cancel()
	}
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
	var key *string
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		f, err = scanFile(tx.QueryRow(ctx, `SELECT `+fileCols+` FROM member_files f JOIN workspaces w ON w.id = f.workspace_id
			WHERE f.id = $1 AND f.owner = $2 AND ($3 = '' OR f.workspace_id::text = $3) AND f.expires_at > now()`, id, owner, workspaceID))
		if err != nil || !content {
			return err
		}
		return tx.QueryRow(ctx, `SELECT content, object_key FROM member_files WHERE id = $1`, id).Scan(&data, &key)
	})
	if err != nil || !content {
		return f, nil, err
	}
	if key != nil { // D24: in the object store, read outside the transaction
		if s.Blobs == nil {
			return nil, nil, errors.New("the file is in the object store, which is not configured")
		}
		if data, err = s.Blobs.Get(ctx, *key, f.Size); err != nil {
			if errors.Is(err, blob.ErrTooLarge) {
				return nil, nil, ErrFileCorrupt
			}
			return nil, nil, err
		}
	}
	// whatever the store returns must be the file the database describes (a tampered or damaged object is refused)
	if sum := sha256.Sum256(data); int64(len(data)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 {
		return nil, nil, ErrFileCorrupt
	}
	return f, data, nil
}

// DeleteFile deletes one of the owner's files in a workspace.
func (s *Store) DeleteFile(ctx context.Context, tenant, workspaceID, owner, id string) error {
	if !uuidRE.MatchString(id) {
		return fail("NOT_FOUND", "file not found")
	}
	var key *string
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `DELETE FROM member_files WHERE id = $1 AND workspace_id = $2 AND owner = $3 RETURNING object_key`, id, workspaceID, owner).Scan(&key)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("NOT_FOUND", "file not found")
		}
		return err
	})
	if err == nil && key != nil && s.Blobs != nil {
		_ = s.Blobs.Delete(ctx, *key) // the row is gone: the file is; a failed object delete is swept
	}
	return err
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
		var keys []string
		err := s.InTenant(ctx, t, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `DELETE FROM member_files WHERE expires_at <= now() RETURNING coalesce(object_key, '')`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var k string
				if err := rows.Scan(&k); err != nil {
					return err
				}
				n++
				if k != "" {
					keys = append(keys, k)
				}
			}
			return rows.Err()
		})
		if err != nil {
			return n, err
		}
		for _, k := range keys {
			if s.Blobs != nil {
				_ = s.Blobs.Delete(ctx, k)
			}
		}
		if err := s.sweepObjects(ctx, t); err != nil {
			return n, err
		}
	}
	return n, nil
}

// OrphanAge is how old an object without a row must be before the sweep deletes it: an upload in progress has its
// object before its row commits.
var OrphanAge = time.Hour

// sweepObjects deletes the tenant's objects that no row names (D24).
func (s *Store) sweepObjects(ctx context.Context, tenant string) error {
	l, ok := s.Blobs.(BlobLister)
	if s.Blobs == nil || !ok {
		return nil
	}
	prefix := "files/" + tenant + "/"
	token := ""
	for {
		objs, next, err := l.List(ctx, prefix, token)
		if err != nil {
			return err
		}
		var old []string
		for _, o := range objs {
			id := strings.TrimPrefix(o.Key, prefix)
			if s.Now().Sub(o.LastModified) >= OrphanAge && uuidRE.MatchString(id) {
				old = append(old, id)
			}
		}
		if len(old) > 0 {
			known := map[string]bool{}
			err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT id::text FROM member_files WHERE id = ANY($1::uuid[])`, old)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						return err
					}
					known[id] = true
				}
				return rows.Err()
			})
			if err != nil {
				return err
			}
			for _, id := range old {
				if !known[id] {
					_ = s.Blobs.Delete(ctx, prefix+id)
				}
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}
