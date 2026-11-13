// Package workspace holds the objects of the IDP Workspace (ADR-0001): workspaces and members, app
// manifests and releases (the app registry), installations, inbox items, work contexts, action
// invocations and the messages of the app bridge. The workspace owns these and nothing else: an
// inbox item points at an approval, run or task another system owns; an action is forwarded to the
// system that decides it.
package workspace

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Name is the shape of workspace, app and action names.
var Name = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// ActionName allows dots so actions group naturally (order.hold, approval.approve).
var ActionName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,62}[a-z0-9])?$`)

// Version is a semantic version.
var Version = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

// EntityType names a business object type (same shape as the CMI's).
var EntityType = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// Workspace is where a team works.
type Workspace struct {
	ID          string     `json:"workspaceId"`
	Name        string     `json:"name"`
	DisplayName string     `json:"displayName"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	ArchivedAt  *time.Time `json:"archivedAt,omitempty"`
}

// Member is a subject in a workspace with workspace-local roles.
type Member struct {
	Subject string    `json:"subject"`
	Roles   []string  `json:"roles"`
	AddedBy string    `json:"addedBy"`
	AddedAt time.Time `json:"addedAt"`
}

// Entity is a business object reference.
type Entity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Capabilities an app may request in its manifest; the shell grants nothing else over the bridge (D5).
const (
	CapContextRead   = "context.read"
	CapContextWrite  = "context.write"
	CapActionsInvoke = "actions.invoke"
	CapInboxRead     = "inbox.read"
	CapNotify        = "ui.notify"
	CapIdentityToken = "identity.token" // 1.1.0 (D15): short-lived tokens for the app's own backend
)

// Capabilities lists the known capabilities.
var Capabilities = []string{CapContextRead, CapContextWrite, CapActionsInvoke, CapInboxRead, CapNotify, CapIdentityToken}

// ManifestSchemaVersion is the manifest format this workspace reads.
const ManifestSchemaVersion = "idp.workspace.app/v1"

// Manifest describes an app (D2).
type Manifest struct {
	SchemaVersion string           `json:"schemaVersion"`
	Name          string           `json:"name"`
	Version       string           `json:"version"`
	Title         string           `json:"title"`
	Description   string           `json:"description,omitempty"`
	Publisher     string           `json:"publisher"`
	Entry         *Entry           `json:"entry,omitempty"` // the H5 page; absent = an app with actions only
	Capabilities  []string         `json:"capabilities"`
	Actions       []ActionContract `json:"actions"`
	Config        *ConfigSpec      `json:"config,omitempty"`
}

// Entry is where the app's page is served.
type Entry struct {
	URL string `json:"url"`
}

// Origin is the scheme://host[:port] of the entry (what the shell's frame and messages are bound to).
func (e Entry) Origin() string {
	u, err := url.Parse(e.URL)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// ConfigSpec is the JSON Schema an installation's configuration must satisfy.
type ConfigSpec struct {
	Schema json.RawMessage `json:"schema"`
}

// ActionContract is something a member can do through an app: a validated input forwarded to the
// system that owns the decision (D4).
type ActionContract struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input"` // JSON Schema (an object schema)
	Target      Target          `json:"target"`
	Roles       []string        `json:"roles,omitempty"` // workspace-local roles allowed to invoke; empty = every member
	Confirm     bool            `json:"confirm,omitempty"`
}

// Target kinds.
const (
	TargetHostRun     = "host.run"     // start an AgentRun of an agent on idp-host
	TargetEERDecision = "eer.decision" // decide an approval on idp-eer
	TargetWebhook     = "webhook"      // POST to the app's own backend
)

// Target is where an action is forwarded.
type Target struct {
	Kind string `json:"kind"`
	// host.run
	Agent string `json:"agent,omitempty"`
	// eer.decision
	Decision string `json:"decision,omitempty"` // approve | reject
	// webhook
	URL string `json:"url,omitempty"`
}

// Validate checks a manifest beyond its schema: names, versions, unique actions, known capabilities,
// HTTPS (or loopback HTTP) URLs, and that an action-invoking app asked for the capability.
func (m *Manifest) Validate() error {
	switch {
	case m.SchemaVersion != ManifestSchemaVersion:
		return fmt.Errorf("schemaVersion must be %s", ManifestSchemaVersion)
	case !Name.MatchString(m.Name):
		return fmt.Errorf("name %q is not a lowercase name", m.Name)
	case !Version.MatchString(m.Version):
		return fmt.Errorf("version %q is not a semantic version", m.Version)
	case strings.TrimSpace(m.Title) == "" || len(m.Title) > 200:
		return fmt.Errorf("title is required (at most 200 characters)")
	case strings.TrimSpace(m.Publisher) == "" || len(m.Publisher) > 200:
		return fmt.Errorf("publisher is required (at most 200 characters)")
	}
	if m.Entry != nil {
		if err := checkURL(m.Entry.URL); err != nil {
			return fmt.Errorf("entry.url: %w", err)
		}
	}
	known := map[string]bool{}
	for _, c := range Capabilities {
		known[c] = true
	}
	seen := map[string]bool{}
	for _, c := range m.Capabilities {
		if !known[c] {
			return fmt.Errorf("unknown capability %q", c)
		}
		if seen[c] {
			return fmt.Errorf("capability %q listed twice", c)
		}
		seen[c] = true
	}
	if len(m.Capabilities) > 0 && m.Entry == nil {
		return fmt.Errorf("bridge capabilities need an entry page")
	}
	names := map[string]bool{}
	for i, a := range m.Actions {
		if !ActionName.MatchString(a.Name) {
			return fmt.Errorf("actions[%d].name %q is not an action name", i, a.Name)
		}
		if names[a.Name] {
			return fmt.Errorf("action %q declared twice", a.Name)
		}
		names[a.Name] = true
		if strings.TrimSpace(a.Title) == "" {
			return fmt.Errorf("action %q needs a title", a.Name)
		}
		if err := a.Target.validate(); err != nil {
			return fmt.Errorf("action %q: %w", a.Name, err)
		}
		for _, r := range a.Roles {
			if !Name.MatchString(r) {
				return fmt.Errorf("action %q: role %q is not a lowercase name", a.Name, r)
			}
		}
	}
	return nil
}

func (t Target) validate() error {
	switch t.Kind {
	case TargetHostRun:
		if !Name.MatchString(t.Agent) {
			return fmt.Errorf("host.run needs an agent name")
		}
		if t.Decision != "" || t.URL != "" {
			return fmt.Errorf("host.run takes only an agent")
		}
	case TargetEERDecision:
		if t.Decision != "approve" && t.Decision != "reject" {
			return fmt.Errorf("eer.decision needs decision approve or reject")
		}
		if t.Agent != "" || t.URL != "" {
			return fmt.Errorf("eer.decision takes only a decision")
		}
	case TargetWebhook:
		if err := checkURL(t.URL); err != nil {
			return fmt.Errorf("webhook url: %w", err)
		}
		if t.Agent != "" || t.Decision != "" {
			return fmt.Errorf("webhook takes only a url")
		}
	default:
		return fmt.Errorf("unknown target kind %q", t.Kind)
	}
	return nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%q must not carry credentials or a fragment", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		h := u.Hostname()
		if h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return fmt.Errorf("%q: plain http only for loopback", raw)
		}
	default:
		return fmt.Errorf("%q: scheme must be https", raw)
	}
	return nil
}

// Action finds an action by name.
func (m *Manifest) Action(name string) (*ActionContract, bool) {
	for i := range m.Actions {
		if m.Actions[i].Name == name {
			return &m.Actions[i], true
		}
	}
	return nil, false
}

// Has reports whether the manifest requests capability c.
func (m *Manifest) Has(c string) bool {
	for _, x := range m.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// Release is a published, immutable app version.
type Release struct {
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Digest      string      `json:"digest"`
	Status      string      `json:"status"` // published | revoked
	Manifest    Manifest    `json:"manifest"`
	PublishedBy string      `json:"publishedBy"`
	PublishedAt time.Time   `json:"publishedAt"`
	Revocation  *Revocation `json:"revocation,omitempty"`
}

// Revocation records who withdrew a release and why.
type Revocation struct {
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// Installation is an app release installed into a workspace.
type Installation struct {
	ID          string          `json:"installationId"`
	WorkspaceID string          `json:"workspaceId"`
	App         string          `json:"app"`
	Version     string          `json:"version"`
	Config      json.RawMessage `json:"config"`
	Revision    int             `json:"revision"`
	InstalledBy string          `json:"installedBy"`
	InstalledAt time.Time       `json:"installedAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
	// Release status of the installed version (a revoked release stays installed but cannot be used).
	ReleaseStatus string `json:"releaseStatus"`
}

// Inbox item kinds, priorities and statuses (D3).
const (
	ItemApproval     = "approval"
	ItemTask         = "task"
	ItemRun          = "run"
	ItemNotification = "notification"

	ItemOpen      = "open"
	ItemResolved  = "resolved"
	ItemWithdrawn = "withdrawn"
)

// Ref points at the authoritative object an item is about.
type Ref struct {
	System string `json:"system"` // eer | host | cmi | app:<name> | external
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	URL    string `json:"url,omitempty"`
}

// ItemAction is an action a member may take from an item: an app's action with a preset input.
type ItemAction struct {
	App    string          `json:"app"`
	Action string          `json:"action"`
	Label  string          `json:"label,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"` // merged under the member's input; the member's fields win
}

// Assignees narrow who sees an item; both empty = every member of the workspace.
type Assignees struct {
	Subjects []string `json:"subjects"`
	Roles    []string `json:"roles"`
}

// Resolution is how the producer closed an item.
type Resolution struct {
	Outcome string    `json:"outcome"`
	By      string    `json:"by"`
	At      time.Time `json:"at"`
	Note    string    `json:"note,omitempty"`
}

// InboxItem is one entry of a workspace's work inbox.
type InboxItem struct {
	ID          string       `json:"itemId"`
	WorkspaceID string       `json:"workspaceId"`
	Producer    string       `json:"producer"`
	ExternalRef string       `json:"externalRef"`
	Kind        string       `json:"kind"`
	Title       string       `json:"title"`
	Summary     string       `json:"summary,omitempty"`
	Ref         Ref          `json:"ref"`
	Assignees   Assignees    `json:"assignees"`
	Priority    string       `json:"priority"`
	DueAt       *time.Time   `json:"dueAt,omitempty"`
	Actions     []ItemAction `json:"actions"`
	Entities    []Entity     `json:"entities"`
	Status      string       `json:"status"`
	Resolution  *Resolution  `json:"resolution,omitempty"`
	Version     int          `json:"version"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
	// The caller's own marks.
	ReadAt     *time.Time `json:"readAt,omitempty"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
}

// WorkContext is what a member is working on in a workspace (D5).
type WorkContext struct {
	WorkspaceID string    `json:"workspaceId"`
	Subject     string    `json:"subject"`
	Entities    []Entity  `json:"entities"`
	App         string    `json:"app,omitempty"`
	Thread      string    `json:"thread,omitempty"`
	ItemID      string    `json:"itemId,omitempty"`
	Version     int       `json:"version"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Invocation statuses.
const (
	InvocationSucceeded = "succeeded"
	InvocationFailed    = "failed"
)

// Invocation records an action a member invoked and what became of it (D4).
type Invocation struct {
	ID          string          `json:"invocationId"`
	WorkspaceID string          `json:"workspaceId"`
	Subject     string          `json:"subject"`
	App         string          `json:"app"`
	AppVersion  string          `json:"appVersion"`
	Action      string          `json:"action"`
	ItemID      string          `json:"itemId,omitempty"`
	InputDigest string          `json:"inputDigest"`
	Target      Target          `json:"target"`
	Status      string          `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *Problem        `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
}

// Conversation is what the workspace itself knows of a member's conversation with an agent (D22): the
// idp-host thread the member's host.run invocations from this workspace named, and when. The messages
// are idp-host's and are read from it.
type Conversation struct {
	ThreadID  string    `json:"threadId"`
	App       string    `json:"app"`
	Action    string    `json:"action"`
	Agent     string    `json:"agent"`
	Runs      int       `json:"runs"`
	FirstAt   time.Time `json:"firstAt"`
	LastAt    time.Time `json:"lastAt"`
	LastRunID string    `json:"lastRunId"`
}

// Problem is a stable error code with a safe message.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ValidEntities checks entity references.
func ValidEntities(es []Entity) error {
	if len(es) > 32 {
		return fmt.Errorf("at most 32 entities")
	}
	for _, e := range es {
		if !EntityType.MatchString(e.Type) || e.ID == "" || len(e.ID) > 256 {
			return fmt.Errorf("entity %s:%s is not a valid reference", e.Type, e.ID)
		}
	}
	return nil
}

// MemberFile is a member's file as the workspace describes it (D23; contract MemberFile): never its content.
type MemberFile struct {
	ID          string    `json:"id"`
	Workspace   string    `json:"workspace"` // the workspace's name
	Name        string    `json:"name"`
	MediaType   string    `json:"mediaType"`
	Size        int64     `json:"size"`
	SHA256      string    `json:"sha256"`
	Source      string    `json:"source"` // upload | run:<runId>
	Sensitivity string    `json:"sensitivity"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// FileSourceUpload: the member uploaded the file; a run's deliverable has the source run:<runId>.
const FileSourceUpload = "upload"
