// Shapes of idp.telos.workspace 1.0.0 the shell uses (api/contracts/schema.json is authoritative).

export interface Entity { type: string; id: string }
export interface Workspace { workspaceId: string; name: string; displayName: string }
export interface Target { kind: 'host.run' | 'eer.decision' | 'webhook'; agent?: string; decision?: 'approve' | 'reject'; url?: string }
export interface ActionContract { name: string; title: string; description?: string; input: Record<string, unknown>; target: Target; roles?: string[]; confirm?: boolean }
export interface Manifest { name: string; version: string; title: string; description?: string; publisher: string; entry?: { url: string }; capabilities: string[]; actions: ActionContract[] }
export interface InstalledApp { installation: { installationId: string; app: string; version: string }; manifest: Manifest; digest: string; origin?: string }
export interface ItemAction { app: string; action: string; label?: string; input?: Record<string, unknown> }
export interface InboxItem {
  itemId: string; workspaceId: string; producer: string; externalRef: string;
  kind: 'approval' | 'task' | 'run' | 'notification'; title: string; summary?: string;
  ref: { system: string; kind: string; id: string; url?: string };
  priority: 'low' | 'normal' | 'high' | 'urgent'; dueAt?: string; actions: ItemAction[]; entities: Entity[];
  status: 'open' | 'resolved' | 'withdrawn'; resolution?: { outcome: string; by: string; at: string; note?: string };
  version: number; createdAt: string; updatedAt: string; readAt?: string; archivedAt?: string;
}
export interface WorkContext { workspaceId: string; subject: string; entities: Entity[]; app?: string; thread?: string; itemId?: string; version: number }
export interface Invocation { invocationId: string; app: string; action: string; target?: Target; status: 'succeeded' | 'failed'; result?: Record<string, unknown>; error?: { code: string; message: string } }

export interface Me { subject: string; tenant: string; roles: string[] }
export interface Member { subject: string; roles: string[]; addedBy: string; addedAt: string }
export interface Release { name: string; version: string; digest: string; status: 'published' | 'revoked'; manifest: Manifest; publishedBy: string; publishedAt: string; revocation?: { by: string; at: string; reason: string } }
export interface InstallationRow { installationId: string; app: string; version: string; config: Record<string, unknown>; revision: number; releaseStatus: 'published' | 'revoked' }
export interface AuditEvent { id: number; at: string; subject: string; action: string; resourceId: string | null; detail: unknown }
