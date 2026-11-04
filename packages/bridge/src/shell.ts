// The shell side of the bridge: it answers an app's requests, but only from the app's own origin and
// frame, only for methods the app's manifest asked for, and never hands the user's credentials over.
import { EVENTS, METHODS, PROTOCOL, parse } from './protocol.ts';
import type { BridgeEvent, BridgeRequest, Capability, Port, Problem } from './protocol.ts';

export class BridgeError extends Error {
  code: string;
  constructor(code: string, message: string) {
    super(message);
    this.code = code;
  }
}

/** What the shell does for each method; each receives the request's params. */
export interface ShellHandlers {
  hello?(params: Record<string, unknown>): unknown | Promise<unknown>;
  getContext?(): unknown | Promise<unknown>;
  setContext?(params: Record<string, unknown>): unknown | Promise<unknown>;
  invokeAction?(params: Record<string, unknown>): unknown | Promise<unknown>;
  listInbox?(params: Record<string, unknown>): unknown | Promise<unknown>;
  notify?(params: Record<string, unknown>): unknown | Promise<unknown>;
  identityToken?(params: Record<string, unknown>): unknown | Promise<unknown>;
}

export interface ShellOptions {
  port: Port;
  /** The app entry's origin (scheme://host[:port]); messages from anywhere else are ignored. */
  origin: string;
  /** The app frame's window; when given, messages from any other source are ignored. */
  source?: unknown;
  /** The capabilities granted: the manifest's list, as installed. */
  capabilities: readonly Capability[];
  handlers: ShellHandlers;
  /** Called for every request that was refused or failed (for the shell's own log). */
  onRefused?(request: BridgeRequest | null, problem: Problem): void;
}

export interface ShellHost {
  /** emit sends an event to the app if it holds the event's capability; returns whether it was sent. */
  emit(event: string, data?: unknown): boolean;
  close(): void;
}

const dispatch: Record<string, keyof ShellHandlers> = {
  'bridge.hello': 'hello',
  'context.get': 'getContext',
  'context.set': 'setContext',
  'actions.invoke': 'invokeAction',
  'inbox.list': 'listInbox',
  'ui.notify': 'notify',
  'identity.token': 'identityToken',
};

export function createShellHost(opts: ShellOptions): ShellHost {
  if (!/^https?:\/\/[^/]+$/.test(opts.origin)) throw new Error('origin must be scheme://host[:port]');
  const granted = new Set<Capability>(opts.capabilities);
  const respond = (id: string, body: { result: unknown } | { error: Problem }) => {
    opts.port.post({ protocol: PROTOCOL, type: 'response', id, ...body }, opts.origin);
  };
  const refuse = (req: BridgeRequest, problem: Problem) => {
    opts.onRefused?.(req, problem);
    respond(req.id, { error: problem });
  };
  const off = opts.port.onMessage(async (data, origin, source) => {
    if (origin !== opts.origin) return; // not the app: ignore silently
    if (opts.source !== undefined && source !== opts.source) return;
    const msg = parse(data);
    if (msg === null) {
      opts.onRefused?.(null, { code: 'MALFORMED', message: 'not an idp.bridge/v1 message' });
      return;
    }
    if (msg.type !== 'request') return; // apps only send requests
    const needed = METHODS[msg.method];
    if (needed && !granted.has(needed)) {
      refuse(msg, { code: 'CAPABILITY_DENIED', message: `the app did not request ${needed}` });
      return;
    }
    const handler = opts.handlers[dispatch[msg.method]];
    if (!handler) {
      refuse(msg, { code: 'UNSUPPORTED', message: `the shell does not implement ${msg.method}` });
      return;
    }
    try {
      let result = await (handler as (p: Record<string, unknown>) => unknown).call(opts.handlers, msg.params ?? {});
      if (msg.method === 'bridge.hello') {
        result = { ...(result as object), protocol: PROTOCOL, capabilities: [...granted] };
      }
      respond(msg.id, { result: result === undefined ? null : result });
    } catch (err) {
      const problem = err instanceof BridgeError ? { code: err.code, message: err.message } : { code: 'FAILED', message: 'the shell could not complete the request' };
      refuse(msg, problem);
    }
  });
  return {
    emit(event, data) {
      if (!(event in EVENTS)) throw new Error(`unknown event ${event}`);
      const needed = EVENTS[event];
      if (needed && !granted.has(needed)) return false;
      const msg: BridgeEvent = { protocol: PROTOCOL, type: 'event', event };
      if (data !== undefined) msg.data = data;
      opts.port.post(msg, opts.origin);
      return true;
    },
    close: off,
  };
}
