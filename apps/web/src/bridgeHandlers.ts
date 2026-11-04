// What the shell does for an app's bridge requests (ADR-0001 D5). Kept apart from the frame so it
// can be tested without a browser: every call goes to the workspace API with the shell's own
// credentials, scoped to this workspace and this app; the app never sees a token.
import { BridgeError } from '@idp-workspace/bridge/shell';
import type { ShellHandlers } from '@idp-workspace/bridge/shell';
import { ApiError } from './api.ts';
import type { Api } from './api.ts';
import type { Entity, InstalledApp, Workspace } from './types.ts';

export interface ShellUI {
  notify(message: string, level: string): void;
  confirm(question: string): Promise<boolean>;
  locale: string;
  theme: string;
}

function asBridgeError(e: unknown): never {
  if (e instanceof ApiError) throw new BridgeError(e.code, e.message);
  throw e;
}

function entities(v: unknown): Entity[] {
  if (!Array.isArray(v)) throw new BridgeError('INVALID_INPUT', 'entities must be an array of {type, id}');
  return v.map((x) => {
    if (typeof x !== 'object' || x === null || typeof (x as Entity).type !== 'string' || typeof (x as Entity).id !== 'string') {
      throw new BridgeError('INVALID_INPUT', 'entities must be an array of {type, id}');
    }
    return { type: (x as Entity).type, id: (x as Entity).id };
  });
}

export function shellHandlers(api: Api, ws: Workspace, app: InstalledApp, ui: ShellUI): ShellHandlers {
  return {
    hello: () => ({ workspace: { workspaceId: ws.workspaceId, name: ws.name }, app: app.manifest.name, version: app.manifest.version, locale: ui.locale, theme: ui.theme }),
    getContext: () => api.context(ws.name).then((c) => ({ entities: c.entities, thread: c.thread, itemId: c.itemId, version: c.version })).catch(asBridgeError),
    // an app sets the business objects in front of the member; the shell records which app did it
    setContext: async (p) => {
      const current = await api.context(ws.name).catch(asBridgeError);
      const next = await api.setContext(ws.name, { ...current, entities: entities(p.entities), app: app.manifest.name }, current.version).catch(asBridgeError);
      return { entities: next.entities, version: next.version };
    },
    invokeAction: async (p) => {
      const name = String(p.action ?? '');
      const contract = app.manifest.actions.find((a) => a.name === name);
      if (!contract) throw new BridgeError('ACTION_NOT_FOUND', `${app.manifest.name} declares no action ${name}`);
      // the member, not the app, agrees to an action marked confirm
      if (contract.confirm && !(await ui.confirm(`${app.manifest.title}: ${contract.title}?`))) {
        throw new BridgeError('DECLINED', 'the member declined the action');
      }
      const input = (p.input ?? {}) as Record<string, unknown>;
      const inv = await api.invoke(ws.name, { app: app.manifest.name, action: name, input, ...(typeof p.itemId === 'string' ? { itemId: p.itemId } : {}) }).catch(asBridgeError);
      return { invocationId: inv.invocationId, status: inv.status, result: inv.result };
    },
    listInbox: (p) => api.inbox(ws.name, typeof p.kind === 'string' ? { kind: p.kind } : {}).then((items) =>
      items.map((i) => ({ itemId: i.itemId, kind: i.kind, title: i.title, priority: i.priority, status: i.status, entities: i.entities }))).catch(asBridgeError),
    notify: (p) => {
      const message = String(p.message ?? '').slice(0, 300);
      if (!message) throw new BridgeError('INVALID_INPUT', 'message is required');
      ui.notify(`${app.manifest.title}: ${message}`, String(p.level ?? 'info'));
      return null;
    },
  };
}
