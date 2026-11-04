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

// ItemInput is what a producer puts into an inbox (D3).
type ItemInput struct {
	ExternalRef string                 `json:"externalRef"`
	Kind        string                 `json:"kind"`
	Title       string                 `json:"title"`
	Summary     string                 `json:"summary"`
	Ref         workspace.Ref          `json:"ref"`
	Assignees   workspace.Assignees    `json:"assignees"`
	Priority    string                 `json:"priority"`
	DueAt       *time.Time             `json:"dueAt"`
	Actions     []workspace.ItemAction `json:"actions"`
	Entities    []workspace.Entity     `json:"entities"`
}

func (in *ItemInput) validate() error {
	switch {
	case in.ExternalRef == "" || len(in.ExternalRef) > 256:
		return fail("INVALID", "externalRef is required (at most 256 characters)")
	case in.Kind != workspace.ItemApproval && in.Kind != workspace.ItemTask && in.Kind != workspace.ItemRun && in.Kind != workspace.ItemNotification:
		return fail("INVALID", "kind must be approval, task, run or notification")
	case strings.TrimSpace(in.Title) == "" || len(in.Title) > 300:
		return fail("INVALID", "title is required (at most 300 characters)")
	case len(in.Summary) > 4000:
		return fail("INVALID", "summary is at most 4000 characters")
	case in.Ref.System == "" || in.Ref.Kind == "" || in.Ref.ID == "" || len(in.Ref.ID) > 256:
		return fail("INVALID", "ref needs system, kind and id")
	case len(in.Assignees.Subjects) > 100 || len(in.Assignees.Roles) > 20:
		return fail("INVALID", "too many assignees")
	case len(in.Actions) > 10:
		return fail("INVALID", "at most 10 actions")
	}
	if !refSystem(in.Ref.System) {
		return fail("INVALID", "ref.system must be eer, host, cmi, external or app:<name>")
	}
	if in.Priority == "" {
		in.Priority = "normal"
	}
	switch in.Priority {
	case "low", "normal", "high", "urgent":
	default:
		return fail("INVALID", "priority must be low, normal, high or urgent")
	}
	for _, r := range in.Assignees.Roles {
		if !workspace.Name.MatchString(r) {
			return fail("INVALID", "assignee role %q is not a lowercase name", r)
		}
	}
	for _, a := range in.Actions {
		if !workspace.Name.MatchString(a.App) || !workspace.ActionName.MatchString(a.Action) {
			return fail("INVALID", "item action %s/%s is not an app action", a.App, a.Action)
		}
		if len(a.Input) > 0 {
			var obj map[string]any
			if err := json.Unmarshal(a.Input, &obj); err != nil {
				return fail("INVALID", "item action %s/%s: preset input must be an object", a.App, a.Action)
			}
		}
	}
	if err := workspace.ValidEntities(in.Entities); err != nil {
		return fail("INVALID", "%s", err.Error())
	}
	if in.Assignees.Subjects == nil {
		in.Assignees.Subjects = []string{}
	}
	if in.Assignees.Roles == nil {
		in.Assignees.Roles = []string{}
	}
	if in.Actions == nil {
		in.Actions = []workspace.ItemAction{}
	}
	if in.Entities == nil {
		in.Entities = []workspace.Entity{}
	}
	return nil
}

func refSystem(s string) bool {
	switch s {
	case "eer", "host", "cmi", "external":
		return true
	}
	app, ok := strings.CutPrefix(s, "app:")
	return ok && workspace.Name.MatchString(app)
}

const itemCols = `i.id::text, i.workspace_id::text, i.producer, i.external_ref, i.kind, i.title, i.summary, i.ref, i.assignee_subjects, i.assignee_roles,
	i.priority, i.due_at, i.actions, i.entities, i.status, i.resolution, i.version, i.created_at, i.updated_at, m.read_at, m.archived_at`

func itemFrom(subjectParam string) string {
	return ` FROM inbox_items i LEFT JOIN inbox_marks m ON m.item_id = i.id AND m.subject = ` + subjectParam
}

func scanItem(row pgx.Row) (*workspace.InboxItem, error) {
	var it workspace.InboxItem
	var ref, actions, entities, resolution []byte
	if err := row.Scan(&it.ID, &it.WorkspaceID, &it.Producer, &it.ExternalRef, &it.Kind, &it.Title, &it.Summary, &ref, &it.Assignees.Subjects, &it.Assignees.Roles,
		&it.Priority, &it.DueAt, &actions, &entities, &it.Status, &resolution, &it.Version, &it.CreatedAt, &it.UpdatedAt, &it.ReadAt, &it.ArchivedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "inbox item not found")
		}
		return nil, err
	}
	if err := json.Unmarshal(ref, &it.Ref); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(actions, &it.Actions); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(entities, &it.Entities); err != nil {
		return nil, err
	}
	if resolution != nil {
		it.Resolution = &workspace.Resolution{}
		if err := json.Unmarshal(resolution, it.Resolution); err != nil {
			return nil, err
		}
	}
	return &it, nil
}

// PutItem creates an item or, for the same (producer, externalRef), updates an open one. A closed
// item is not reopened (409). created reports whether the item is new.
func (s *Store) PutItem(ctx context.Context, tenant, workspaceID, producer string, in ItemInput) (out *workspace.InboxItem, created bool, err error) {
	if err := in.validate(); err != nil {
		return nil, false, err
	}
	ref, _ := json.Marshal(in.Ref)
	actions, _ := json.Marshal(in.Actions)
	entities, _ := json.Marshal(in.Entities)
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var id, status string
		err := tx.QueryRow(ctx, `SELECT id::text, status FROM inbox_items WHERE workspace_id = $1 AND producer = $2 AND external_ref = $3 FOR UPDATE`, workspaceID, producer, in.ExternalRef).Scan(&id, &status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			id, created = NewUUID(), true
			if _, err := tx.Exec(ctx, `INSERT INTO inbox_items (id, tenant_id, workspace_id, producer, external_ref, kind, title, summary, ref, assignee_subjects, assignee_roles, priority, due_at, actions, entities)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
				id, tenant, workspaceID, producer, in.ExternalRef, in.Kind, in.Title, in.Summary, ref, in.Assignees.Subjects, in.Assignees.Roles, in.Priority, in.DueAt, actions, entities); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if status != workspace.ItemOpen {
				return fail("CONFLICT", "item %s is %s; a closed item is not reopened", in.ExternalRef, status)
			}
			if _, err := tx.Exec(ctx, `UPDATE inbox_items SET kind = $2, title = $3, summary = $4, ref = $5, assignee_subjects = $6, assignee_roles = $7, priority = $8, due_at = $9,
				actions = $10, entities = $11, version = version + 1, updated_at = now() WHERE id = $1`,
				id, in.Kind, in.Title, in.Summary, ref, in.Assignees.Subjects, in.Assignees.Roles, in.Priority, in.DueAt, actions, entities); err != nil {
				return err
			}
		}
		out, err = scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+itemFrom("$2")+` WHERE i.id = $1`, id, producer))
		return err
	})
	return out, created, err
}

// CloseItem resolves or withdraws an open item; only its producer may (D3).
func (s *Store) CloseItem(ctx context.Context, tenant, workspaceID, producer, ref string, byExternalRef bool, status, outcome, note string) (*workspace.InboxItem, error) {
	if status != workspace.ItemResolved && status != workspace.ItemWithdrawn {
		return nil, fail("INVALID", "status must be resolved or withdrawn")
	}
	if status == workspace.ItemResolved && (strings.TrimSpace(outcome) == "" || len(outcome) > 64) {
		return nil, fail("INVALID", "a resolution needs an outcome (at most 64 characters)")
	}
	if len(note) > 2000 {
		return nil, fail("INVALID", "note is at most 2000 characters")
	}
	var out *workspace.InboxItem
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		where := `workspace_id = $1 AND id::text = $2`
		if byExternalRef {
			where = `workspace_id = $1 AND external_ref = $2 AND producer = $3`
		}
		args := []any{workspaceID, ref}
		if byExternalRef {
			args = append(args, producer)
		}
		var id, owner, current string
		if err := tx.QueryRow(ctx, `SELECT id::text, producer, status FROM inbox_items WHERE `+where+` FOR UPDATE`, args...).Scan(&id, &owner, &current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fail("NOT_FOUND", "inbox item not found")
			}
			return err
		}
		if owner != producer {
			return fail("FORBIDDEN", "only the item's producer closes it")
		}
		if current != workspace.ItemOpen {
			return fail("CONFLICT", "item is already %s", current)
		}
		res, _ := json.Marshal(workspace.Resolution{Outcome: outcome, By: producer, At: s.now().UTC(), Note: note})
		if _, err := tx.Exec(ctx, `UPDATE inbox_items SET status = $2, resolution = $3, version = version + 1, updated_at = now() WHERE id = $1`, id, status, res); err != nil {
			return err
		}
		var err error
		out, err = scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+itemFrom("$2")+` WHERE i.id = $1`, id, producer))
		return err
	})
	return out, err
}

// InboxQuery filters a member's inbox.
type InboxQuery struct {
	Status   string // "" = open
	Kind     string
	Archived bool // include archived
	Limit    int
	Producer string // a producer listing its own items
}

// visibleItemSQL: assigned to the subject, to one of its workspace roles, or to nobody in particular.
const visibleItemSQL = `(cardinality(i.assignee_subjects) = 0 AND cardinality(i.assignee_roles) = 0 OR $2 = ANY(i.assignee_subjects) OR i.assignee_roles && $3::text[])`

// ListInbox lists what subject (with its workspace roles) sees in a workspace.
func (s *Store) ListInbox(ctx context.Context, tenant, workspaceID, subject string, roles []string, q InboxQuery) ([]workspace.InboxItem, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	if q.Status == "" {
		q.Status = workspace.ItemOpen
	}
	if roles == nil {
		roles = []string{}
	}
	out := []workspace.InboxItem{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		visible := visibleItemSQL
		if q.Producer != "" {
			visible = `$3::text[] IS NOT NULL` // a producer sees every item it put, whoever it is assigned to
		}
		rows, err := tx.Query(ctx, `SELECT `+itemCols+itemFrom("$2")+`
			WHERE i.workspace_id = $1 AND `+visible+` AND ($4 = 'all' OR i.status = $4) AND ($5 = '' OR i.kind = $5) AND ($6 OR m.archived_at IS NULL) AND ($8 = '' OR i.producer = $8)
			ORDER BY CASE i.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END, i.updated_at DESC LIMIT $7`,
			workspaceID, subject, roles, q.Status, q.Kind, q.Archived, q.Limit, q.Producer)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				return err
			}
			out = append(out, *it)
		}
		return rows.Err()
	})
	return out, err
}

// GetItem reads an item as subject sees it (not visible = not found).
func (s *Store) GetItem(ctx context.Context, tenant, workspaceID, id, subject string, roles []string) (*workspace.InboxItem, error) {
	if !IsUUID(id) {
		return nil, fail("NOT_FOUND", "inbox item not found")
	}
	if roles == nil {
		roles = []string{}
	}
	var out *workspace.InboxItem
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var err error
		out, err = scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+itemFrom("$2")+` WHERE i.workspace_id = $1 AND i.id = $4 AND `+visibleItemSQL, workspaceID, subject, roles, id))
		return err
	})
	return out, err
}

// MarkItem sets or clears the subject's read and archived marks on a visible item.
func (s *Store) MarkItem(ctx context.Context, tenant, workspaceID, id, subject string, roles []string, read, archived *bool) (*workspace.InboxItem, error) {
	if _, err := s.GetItem(ctx, tenant, workspaceID, id, subject, roles); err != nil {
		return nil, err
	}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO inbox_marks (tenant_id, item_id, subject) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, tenant, id, subject); err != nil {
			return err
		}
		if read != nil {
			if _, err := tx.Exec(ctx, `UPDATE inbox_marks SET read_at = CASE WHEN $3 THEN coalesce(read_at, now()) END WHERE item_id = $1 AND subject = $2`, id, subject, *read); err != nil {
				return err
			}
		}
		if archived != nil {
			if _, err := tx.Exec(ctx, `UPDATE inbox_marks SET archived_at = CASE WHEN $3 THEN coalesce(archived_at, now()) END WHERE item_id = $1 AND subject = $2`, id, subject, *archived); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetItem(ctx, tenant, workspaceID, id, subject, roles)
}

// Cursor is a position in a workspace's stream of item changes.
type Cursor struct {
	At time.Time
	ID string
}

// ChangedItems lists the items subject sees in a workspace that changed at or after since (oldest
// first), for the inbox stream (D10). The caller keeps an overlap window and drops what it already
// sent, because a transaction's now() can precede an earlier-committed one's.
func (s *Store) ChangedItems(ctx context.Context, tenant, workspaceID, subject string, roles []string, since time.Time, limit int) ([]workspace.InboxItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	if roles == nil {
		roles = []string{}
	}
	out := []workspace.InboxItem{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+itemCols+itemFrom("$2")+` WHERE i.workspace_id = $1 AND `+visibleItemSQL+` AND i.updated_at >= $4
			ORDER BY i.updated_at, i.id LIMIT $5`, workspaceID, subject, roles, since, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanItem(rows)
			if err != nil {
				return err
			}
			out = append(out, *it)
		}
		return rows.Err()
	})
	return out, err
}
