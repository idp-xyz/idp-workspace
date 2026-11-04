// The app bridge protocol idp.bridge/v1 (idp-workspace ADR-0001 D5; contract idp.telos.workspace
// BridgeMessage). An app page runs in a sandboxed frame and talks to the workspace shell only through
// these postMessage messages.

export const PROTOCOL = 'idp.bridge/v1';

export type Capability = 'context.read' | 'context.write' | 'actions.invoke' | 'inbox.read' | 'ui.notify' | 'identity.token';

/** Requests an app may send, and the capability each needs (null = none). */
export const METHODS: Readonly<Record<string, Capability | null>> = Object.freeze({
  'bridge.hello': null,
  'context.get': 'context.read',
  'context.set': 'context.write',
  'actions.invoke': 'actions.invoke',
  'inbox.list': 'inbox.read',
  'ui.notify': 'ui.notify',
  'identity.token': 'identity.token', // 1.1.0: a short-lived token for the app's own backend
});

/** Events the shell sends, and the capability an app needs to receive each (null = none). */
export const EVENTS: Readonly<Record<string, Capability | null>> = Object.freeze({
  'context.changed': 'context.read',
  'theme.changed': null,
});

export interface Problem {
  code: string;
  message: string;
}

export interface BridgeRequest {
  protocol: typeof PROTOCOL;
  type: 'request';
  id: string;
  method: string;
  params?: Record<string, unknown>;
}

export interface BridgeResponse {
  protocol: typeof PROTOCOL;
  type: 'response';
  id: string;
  result?: unknown;
  error?: Problem;
}

export interface BridgeEvent {
  protocol: typeof PROTOCOL;
  type: 'event';
  event: string;
  data?: unknown;
}

export type BridgeMessage = BridgeRequest | BridgeResponse | BridgeEvent;

const ID = /^[A-Za-z0-9_.:-]{1,64}$/;
const CODE = /^[A-Za-z0-9_]{1,64}$/;

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function only(m: Record<string, unknown>, allowed: string[]): boolean {
  return Object.keys(m).every((k) => allowed.includes(k));
}

/** parse checks an incoming message against the contract; anything else is null (ignored). */
export function parse(data: unknown): BridgeMessage | null {
  if (!isObject(data) || data.protocol !== PROTOCOL) return null;
  switch (data.type) {
    case 'request':
      if (!only(data, ['protocol', 'type', 'id', 'method', 'params'])) return null;
      if (typeof data.id !== 'string' || !ID.test(data.id) || typeof data.method !== 'string' || !(data.method in METHODS)) return null;
      if (data.params !== undefined && !isObject(data.params)) return null;
      return data as unknown as BridgeRequest;
    case 'response': {
      if (!only(data, ['protocol', 'type', 'id', 'result', 'error'])) return null;
      if (typeof data.id !== 'string' || !ID.test(data.id)) return null;
      const hasResult = 'result' in data;
      const hasError = 'error' in data;
      if (hasResult === hasError) return null;
      if (hasError) {
        const e = data.error;
        if (!isObject(e) || typeof e.code !== 'string' || !CODE.test(e.code) || typeof e.message !== 'string' || !only(e, ['code', 'message'])) return null;
      }
      return data as unknown as BridgeResponse;
    }
    case 'event':
      if (!only(data, ['protocol', 'type', 'event', 'data'])) return null;
      if (typeof data.event !== 'string' || !(data.event in EVENTS)) return null;
      return data as unknown as BridgeEvent;
    default:
      return null;
  }
}

/**
 * A Port is one side of a postMessage channel. In a browser use windowPort; tests use an in-memory
 * pair. onMessage delivers the data together with the sender's origin and source.
 */
export interface Port {
  post(message: BridgeMessage, targetOrigin: string): void;
  onMessage(listener: (data: unknown, origin: string, source: unknown) => void): () => void;
}

interface MessageWindow {
  postMessage(message: unknown, targetOrigin: string): void;
  addEventListener(type: 'message', listener: (ev: { data: unknown; origin: string; source: unknown }) => void): void;
  removeEventListener(type: 'message', listener: (ev: { data: unknown; origin: string; source: unknown }) => void): void;
}

/** windowPort posts to target (the app frame's contentWindow, or window.parent) and listens on self. */
export function windowPort(self: MessageWindow, target: { postMessage(message: unknown, targetOrigin: string): void }): Port {
  return {
    post: (message, targetOrigin) => target.postMessage(message, targetOrigin),
    onMessage(listener) {
      const fn = (ev: { data: unknown; origin: string; source: unknown }) => listener(ev.data, ev.origin, ev.source);
      self.addEventListener('message', fn);
      return () => self.removeEventListener('message', fn);
    },
  };
}
