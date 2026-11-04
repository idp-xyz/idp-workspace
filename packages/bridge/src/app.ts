// The app side of the bridge: requests with correlated responses and a timeout, events by name. It
// accepts messages only from the shell's origin.
import { PROTOCOL, parse } from './protocol.ts';
import type { BridgeRequest, Port, Problem } from './protocol.ts';

export class BridgeRequestError extends Error {
  code: string;
  constructor(problem: Problem) {
    super(problem.message);
    this.code = problem.code;
  }
}

export interface AppClientOptions {
  port: Port;
  /** The workspace shell's origin; messages from anywhere else are ignored. */
  shellOrigin: string;
  app: string;
  version: string;
  timeoutMs?: number;
}

export interface Hello {
  protocol: string;
  capabilities: string[];
  workspace?: { workspaceId: string; name: string };
  locale?: string;
  theme?: string;
  [k: string]: unknown;
}

export interface AppClient {
  hello(): Promise<Hello>;
  request<T = unknown>(method: string, params?: Record<string, unknown>): Promise<T>;
  getContext<T = unknown>(): Promise<T>;
  setContext<T = unknown>(context: Record<string, unknown>): Promise<T>;
  invokeAction<T = unknown>(action: string, input: Record<string, unknown>, itemId?: string): Promise<T>;
  listInbox<T = unknown>(query?: Record<string, unknown>): Promise<T>;
  notify(message: string, level?: 'info' | 'success' | 'warning' | 'error'): Promise<void>;
  on(event: string, listener: (data: unknown) => void): () => void;
  close(): void;
}

export function createAppClient(opts: AppClientOptions): AppClient {
  const timeout = opts.timeoutMs ?? 15000;
  let seq = 0;
  const pending = new Map<string, { resolve(v: unknown): void; reject(e: unknown): void; timer: ReturnType<typeof setTimeout> }>();
  const listeners = new Map<string, Set<(data: unknown) => void>>();
  const off = opts.port.onMessage((data, origin) => {
    if (origin !== opts.shellOrigin) return;
    const msg = parse(data);
    if (msg === null) return;
    if (msg.type === 'response') {
      const p = pending.get(msg.id);
      if (!p) return;
      pending.delete(msg.id);
      clearTimeout(p.timer);
      if (msg.error) p.reject(new BridgeRequestError(msg.error));
      else p.resolve(msg.result);
    } else if (msg.type === 'event') {
      for (const fn of listeners.get(msg.event) ?? []) fn(msg.data);
    }
  });
  const request = <T>(method: string, params?: Record<string, unknown>): Promise<T> =>
    new Promise<T>((resolve, reject) => {
      const id = `${opts.app}.${++seq}`;
      const timer = setTimeout(() => {
        pending.delete(id);
        reject(new BridgeRequestError({ code: 'TIMEOUT', message: `${method} got no answer within ${timeout} ms` }));
      }, timeout);
      pending.set(id, { resolve: resolve as (v: unknown) => void, reject, timer });
      const msg: BridgeRequest = { protocol: PROTOCOL, type: 'request', id, method };
      if (params) msg.params = params;
      opts.port.post(msg, opts.shellOrigin);
    });
  return {
    hello: () => request<Hello>('bridge.hello', { app: opts.app, version: opts.version }),
    request,
    getContext: () => request('context.get'),
    setContext: (context) => request('context.set', context),
    invokeAction: (action, input, itemId) => request('actions.invoke', { action, input, ...(itemId ? { itemId } : {}) }),
    listInbox: (query) => request('inbox.list', query ?? {}),
    notify: async (message, level = 'info') => {
      await request('ui.notify', { message, level });
    },
    on(event, listener) {
      let set = listeners.get(event);
      if (!set) listeners.set(event, (set = new Set()));
      set.add(listener);
      return () => set.delete(listener);
    },
    close() {
      off();
      for (const [, p] of pending) {
        clearTimeout(p.timer);
        p.reject(new BridgeRequestError({ code: 'CLOSED', message: 'the bridge was closed' }));
      }
      pending.clear();
    },
  };
}
