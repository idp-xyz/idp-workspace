// The shell's client of the workspace API (same origin). The bearer token lives in sessionStorage
// for the tab only; it is never given to an app frame (ADR-0001 D5).
import type { AuditEvent, Conversation, InboxItem, InstalledApp, InstallationRow, Invocation, MemoryItem, Me, Member, Release, Workspace, WorkContext } from './types.ts';
import type { AGUIEvent, HostMessage, HostRun, HostToolCall } from './agent/events.ts';
import { sseParser } from './sse.ts';

export class ApiError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

const TOKEN = 'idp-workspace.token';

export interface Store { getItem(k: string): string | null; setItem(k: string, v: string): void; removeItem(k: string): void }

export class Api {
  private fetcher: typeof fetch;
  private store: Store;
  /** retryMs is how long the inbox stream waits before reconnecting. */
  retryMs: number;
  constructor(fetcher: typeof fetch = (...a) => fetch(...a), store: Store = sessionStorage, retryMs = 2000) {
    this.fetcher = fetcher;
    this.store = store;
    this.retryMs = retryMs;
  }

  get token(): string | null { return this.store.getItem(TOKEN); }
  signIn(token: string) { this.store.setItem(TOKEN, token.trim()); }
  signOut() { this.store.removeItem(TOKEN); }

  async request<T>(method: string, path: string, body?: unknown, headers: Record<string, string> = {}): Promise<T> {
    const res = await this.fetcher(path, {
      method,
      headers: { Authorization: `Bearer ${this.token ?? ''}`, ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}), ...headers },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    const text = await res.text();
    const json = text ? JSON.parse(text) : null; // 204 has no body
    if (!res.ok) {
      const err = json?.error ?? {};
      throw new ApiError(res.status, err.code ?? `HTTP_${res.status}`, err.message ?? res.statusText);
    }
    return json as T;
  }

  /** the member's own conversation memory in idp-cmi; ApiError 404 NOT_CONFIGURED when the workspace has none (ADR-0001 D21) */
  memory() { return this.request<{ memories: MemoryItem[] }>('GET', '/v1/me/memory').then((r) => r.memories); }
  /** the member's memory as the CMI's export document (ADR-0001 D36) */
  exportMemory() { return this.request<Record<string, unknown>>('GET', '/v1/me/memory/export'); }
  deleteMemory(id: string) { return this.request<{ revoked: number }>('DELETE', `/v1/me/memory/${encodeURIComponent(id)}`); }
  deleteAllMemory() { return this.request<{ revoked: number }>('DELETE', '/v1/me/memory'); }

  me() { return this.request<Me>('GET', '/v1/me'); }

  // ---- administration (ws-admin; the server enforces it) ----
  createWorkspace(name: string, displayName: string) { return this.request<Workspace>('POST', '/api/v1/workspaces', { name, displayName }); }
  members(ws: string) { return this.request<{ members: Member[] }>('GET', `/api/v1/workspaces/${encodeURIComponent(ws)}/members`).then((r) => r.members); }
  putMember(ws: string, subject: string, roles: string[]) { return this.request<Member>('PUT', `/api/v1/workspaces/${encodeURIComponent(ws)}/members/${encodeURIComponent(subject)}`, { roles }); }
  removeMember(ws: string, subject: string) { return this.request<null>('DELETE', `/api/v1/workspaces/${encodeURIComponent(ws)}/members/${encodeURIComponent(subject)}`); }
  releases() { return this.request<{ releases: Release[] }>('GET', '/api/v1/apps').then((r) => r.releases); }
  publish(manifest: unknown) { return this.request<Release>('POST', '/api/v1/apps', manifest); }
  revoke(name: string, version: string, reason: string) { return this.request<Release>('POST', `/api/v1/apps/${encodeURIComponent(name)}/${encodeURIComponent(version)}/revoke`, { reason }); }
  installations(ws: string) { return this.request<{ installations: InstallationRow[] }>('GET', `/api/v1/workspaces/${encodeURIComponent(ws)}/installations`).then((r) => r.installations); }
  install(ws: string, app: string, version: string, config: Record<string, unknown>, revision?: number) {
    return this.request<InstallationRow>('PUT', `/api/v1/workspaces/${encodeURIComponent(ws)}/installations/${encodeURIComponent(app)}`, { version, config }, revision ? { 'If-Match': String(revision) } : {});
  }
  uninstall(ws: string, app: string) { return this.request<null>('DELETE', `/api/v1/workspaces/${encodeURIComponent(ws)}/installations/${encodeURIComponent(app)}`); }
  audit(limit = 50) { return this.request<{ events: AuditEvent[] }>('GET', `/api/v1/audit?limit=${limit}`).then((r) => r.events); }

  shellConfig() { return this.request<{ devMode: boolean; version: string; oidc?: { issuer: string; clientId: string; scope: string } }>('GET', '/shell-config.json'); }
  appToken(ws: string, app: string) { return this.request<{ token: string; expiresAt: string }>('POST', `/v1/workspaces/${encodeURIComponent(ws)}/apps/${encodeURIComponent(app)}/token`); }
  workspaces() { return this.request<{ workspaces: Workspace[] }>('GET', '/v1/workspaces').then((r) => r.workspaces); }
  workspace(ws: string) { return this.request<{ workspace: Workspace; roles: string[]; member: boolean }>('GET', `/v1/workspaces/${encodeURIComponent(ws)}`); }
  apps(ws: string) { return this.request<{ apps: InstalledApp[] }>('GET', `/v1/workspaces/${encodeURIComponent(ws)}/apps`).then((r) => r.apps); }
  inbox(ws: string, query: Record<string, string> = {}) {
    const q = new URLSearchParams(query).toString();
    return this.request<{ items: InboxItem[] }>('GET', `/v1/workspaces/${encodeURIComponent(ws)}/inbox${q ? `?${q}` : ''}`).then((r) => r.items);
  }
  mark(ws: string, itemId: string, marks: { read?: boolean; archived?: boolean }) {
    return this.request<InboxItem>('POST', `/v1/workspaces/${encodeURIComponent(ws)}/inbox/${itemId}/marks`, marks);
  }
  context(ws: string) { return this.request<WorkContext>('GET', `/v1/workspaces/${encodeURIComponent(ws)}/context`); }
  setContext(ws: string, ctx: Partial<WorkContext>, version?: number) {
    return this.request<WorkContext>('PUT', `/v1/workspaces/${encodeURIComponent(ws)}/context`,
      { entities: ctx.entities ?? [], app: ctx.app, thread: ctx.thread, itemId: ctx.itemId }, version !== undefined ? { 'If-Match': String(version) } : {});
  }
  /** the member's own invocations in the workspace, newest first */
  invocations(ws: string, limit = 100) {
    return this.request<{ invocations: Invocation[] }>('GET', `/v1/workspaces/${encodeURIComponent(ws)}/invocations?limit=${limit}`).then((r) => r.invocations);
  }
  /** invoke an action; a fresh idempotency key per user gesture makes a double click harmless. */
  invoke(ws: string, call: { app: string; action: string; input?: Record<string, unknown>; itemId?: string; threadId?: string }, key: string = crypto.randomUUID()): Promise<Invocation> {
    return this.request<Invocation>('POST', `/v1/workspaces/${encodeURIComponent(ws)}/actions/invoke`, call, { 'Idempotency-Key': key });
  }

  // ---- conversations with the agents (D22): the member's own runs and threads, relayed to idp-host ----
  conversations(ws: string, limit = 50) {
    return this.request<{ conversations: Conversation[] }>('GET', `/v1/workspaces/${encodeURIComponent(ws)}/conversations?limit=${limit}`).then((r) => r.conversations);
  }
  run(ws: string, runId: string) { return this.request<HostRun>('GET', `/v1/workspaces/${encodeURIComponent(ws)}/runs/${encodeURIComponent(runId)}`); }
  runToolCalls(ws: string, runId: string) {
    return this.request<{ toolCalls: HostToolCall[] }>('GET', `/v1/workspaces/${encodeURIComponent(ws)}/runs/${encodeURIComponent(runId)}/tool-calls`).then((r) => r.toolCalls);
  }
  decide(ws: string, runId: string, toolCallId: string, decision: 'confirm' | 'reject') {
    return this.request<HostToolCall>('POST', `/v1/workspaces/${encodeURIComponent(ws)}/runs/${encodeURIComponent(runId)}/tool-calls/${encodeURIComponent(toolCallId)}/decide`, { decision });
  }
  cancelRun(ws: string, runId: string) { return this.request<HostRun>('POST', `/v1/workspaces/${encodeURIComponent(ws)}/runs/${encodeURIComponent(runId)}/cancel`); }
  threadMessages(ws: string, threadId: string) {
    return this.request<{ messages: HostMessage[] }>('GET', `/v1/workspaces/${encodeURIComponent(ws)}/threads/${encodeURIComponent(threadId)}/messages`).then((r) => r.messages);
  }
  threadRuns(ws: string, threadId: string) {
    return this.request<{ runs: HostRun[] }>('GET', `/v1/workspaces/${encodeURIComponent(ws)}/threads/${encodeURIComponent(threadId)}/runs`).then((r) => r.runs);
  }
  /** follow a run's events once, from after the given event id: onEvent for each until idp-host closes
   * the stream (the run ended or stopped for a decision) or the signal aborts. Returns the last event id. */
  async runEvents(ws: string, runId: string, after: string, onEvent: (ev: AGUIEvent, id: string) => void, signal: AbortSignal): Promise<string> {
    const res = await this.fetcher(`/v1/workspaces/${encodeURIComponent(ws)}/runs/${encodeURIComponent(runId)}/events${after ? `?after=${encodeURIComponent(after)}` : ''}`, {
      headers: { Authorization: `Bearer ${this.token ?? ''}`, Accept: 'text/event-stream', ...(after ? { 'Last-Event-ID': after } : {}) }, signal,
    });
    if (!res.ok || !res.body) {
      const text = await res.text().catch(() => '');
      let err: { code?: string; message?: string } = {};
      try { err = (JSON.parse(text) as { error?: typeof err }).error ?? {}; } catch { /* not a problem document */ }
      throw new ApiError(res.status, err.code ?? `HTTP_${res.status}`, err.message ?? 'the run\'s events are not available');
    }
    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    const parse = sseParser();
    let last = after;
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      for (const ev of parse(value)) {
        if (ev.id) last = ev.id;
        try { onEvent(JSON.parse(ev.data) as AGUIEvent, ev.id); } catch { /* a line that is not an event */ }
      }
    }
    return last;
  }

  /** stream the inbox: onItem for each change until the signal aborts; reconnects from the last id. */
  async stream(ws: string, onItem: (item: InboxItem) => void, signal: AbortSignal): Promise<void> {
    let last = '';
    while (!signal.aborted) {
      try {
        const res = await this.fetcher(`/v1/workspaces/${encodeURIComponent(ws)}/inbox/stream${last ? `?after=${encodeURIComponent(last)}` : ''}`, {
          headers: { Authorization: `Bearer ${this.token ?? ''}`, Accept: 'text/event-stream' }, signal,
        });
        if (!res.ok || !res.body) throw new ApiError(res.status, `HTTP_${res.status}`, 'stream refused');
        const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
        const parse = sseParser();
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          for (const ev of parse(value)) {
            if (ev.event !== 'item') continue;
            last = ev.id;
            onItem(JSON.parse(ev.data) as InboxItem);
          }
        }
      } catch (e) {
        if (signal.aborted) return;
        if (e instanceof ApiError && (e.status === 401 || e.status === 403 || e.status === 404)) throw e;
      }
      await new Promise((r) => setTimeout(r, this.retryMs));
    }
  }
}
