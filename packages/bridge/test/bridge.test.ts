import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createShellHost, BridgeError } from '../src/shell.ts';
import { createAppClient, BridgeRequestError } from '../src/app.ts';
import { EVENTS, METHODS, PROTOCOL, parse } from '../src/protocol.ts';
import type { BridgeMessage, Port, Capability } from '../src/protocol.ts';
import { sign, verify } from '../src/webhook.ts';

const SHELL = 'https://workspace.example';
const APP = 'https://orders.apps.example';
const FRAME = { name: 'orders-frame' };

type Listener = (data: unknown, origin: string, source: unknown) => void;

/** A pair of ports: what one side posts arrives at the other with the poster's origin and source. */
function channel() {
  const shellListeners = new Set<Listener>();
  const appListeners = new Set<Listener>();
  const deliver = (to: Set<Listener>, data: unknown, origin: string, source: unknown) =>
    queueMicrotask(() => to.forEach((l) => l(structuredClone(data), origin, source)));
  const shell: Port = {
    post: (m, target) => { assert.equal(target, APP, 'the shell posts only to the app origin'); deliver(appListeners, m, SHELL, 'parent'); },
    onMessage: (l) => { shellListeners.add(l); return () => shellListeners.delete(l); },
  };
  const app: Port = {
    post: (m, target) => { assert.equal(target, SHELL, 'the app posts only to the shell origin'); deliver(shellListeners, m, APP, FRAME); },
    onMessage: (l) => { appListeners.add(l); return () => appListeners.delete(l); },
  };
  // an attacker page posting to the shell under its own origin, or from another frame
  const forge = (data: unknown, origin: string, source: unknown = FRAME) => deliver(shellListeners, data, origin, source);
  const toApp = (data: unknown, origin: string) => deliver(appListeners, data, origin, 'parent');
  return { shell, app, forge, toApp };
}

function setup(capabilities: Capability[], extra: Partial<Parameters<typeof createShellHost>[0]['handlers']> = {}) {
  const ch = channel();
  const calls: Array<[string, unknown]> = [];
  const refused: string[] = [];
  let context = { entities: [{ type: 'order', id: '1001' }], version: 1 };
  const host = createShellHost({
    port: ch.shell, origin: APP, source: FRAME, capabilities,
    handlers: {
      hello: (p) => { calls.push(['hello', p]); return { workspace: { workspaceId: 'w1', name: 'us-ops' }, locale: 'zh-CN', theme: 'light' }; },
      getContext: () => context,
      setContext: (p) => { context = { ...(p as typeof context), version: context.version + 1 }; return context; },
      invokeAction: (p) => { calls.push(['invoke', p]); if (p.action === 'order.fail') throw new BridgeError('INVALID_INPUT', 'orderId must be digits'); if (p.action === 'order.crash') throw new Error('secret internals'); return { invocationId: 'i1', status: 'succeeded' }; },
      notify: (p) => { calls.push(['notify', p]); },
      ...extra,
    },
    onRefused: (_r, p) => refused.push(p.code),
  });
  const app = createAppClient({ port: ch.app, shellOrigin: SHELL, app: 'orders', version: '1.2.0', timeoutMs: 200 });
  return { ch, host, app, calls, refused };
}

test('hello reports the granted capabilities and the shell context', async () => {
  const { app, calls } = setup(['context.read', 'actions.invoke']);
  const hello = await app.hello();
  assert.equal(hello.protocol, PROTOCOL);
  assert.deepEqual(hello.capabilities, ['context.read', 'actions.invoke']);
  assert.equal(hello.workspace?.name, 'us-ops');
  assert.deepEqual(calls[0], ['hello', { app: 'orders', version: '1.2.0' }]);
});

test('granted methods run; others are refused with CAPABILITY_DENIED', async () => {
  const { app, calls, refused } = setup(['context.read', 'actions.invoke']);
  assert.deepEqual(await app.getContext(), { entities: [{ type: 'order', id: '1001' }], version: 1 });
  assert.deepEqual(await app.invokeAction('order.hold', { orderId: '1001' }, 'item-1'), { invocationId: 'i1', status: 'succeeded' });
  assert.deepEqual(calls.at(-1), ['invoke', { action: 'order.hold', input: { orderId: '1001' }, itemId: 'item-1' }]);
  await assert.rejects(app.setContext({ entities: [] }), (e: BridgeRequestError) => e.code === 'CAPABILITY_DENIED');
  await assert.rejects(app.notify('hi'), (e: BridgeRequestError) => e.code === 'CAPABILITY_DENIED');
  assert.deepEqual(refused, ['CAPABILITY_DENIED', 'CAPABILITY_DENIED']);
});

test('handler errors: a BridgeError keeps its code, anything else is a generic FAILED', async () => {
  const { app } = setup(['actions.invoke']);
  await assert.rejects(app.invokeAction('order.fail', {}), (e: BridgeRequestError) => e.code === 'INVALID_INPUT' && e.message === 'orderId must be digits');
  await assert.rejects(app.invokeAction('order.crash', {}), (e: BridgeRequestError) => e.code === 'FAILED' && !e.message.includes('secret'));
});

test('a granted method the shell does not implement is UNSUPPORTED', async () => {
  const { app } = setup(['inbox.read']);
  await assert.rejects(app.listInbox(), (e: BridgeRequestError) => e.code === 'UNSUPPORTED');
});

test('the shell ignores other origins, other frames and malformed messages', async () => {
  const { ch, calls, refused } = setup(['actions.invoke']);
  const req = { protocol: PROTOCOL, type: 'request', id: 'x1', method: 'actions.invoke', params: { action: 'order.hold', input: {} } };
  ch.forge(req, 'https://evil.example');
  ch.forge(req, APP, { name: 'another-frame' });
  ch.forge({ ...req, method: 'token.get' }, APP);
  ch.forge({ ...req, extra: true }, APP);
  ch.forge('not even an object', APP);
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(calls.length, 0);
  assert.deepEqual(refused, ['MALFORMED', 'MALFORMED', 'MALFORMED']);
});

test('the app ignores responses and events from other origins, and times out', async () => {
  const { ch, app } = setup(['context.read'], { getContext: () => new Promise(() => {}) });
  const seen: unknown[] = [];
  app.on('context.changed', (d) => seen.push(d));
  ch.toApp({ protocol: PROTOCOL, type: 'event', event: 'context.changed', data: { forged: true } }, 'https://evil.example');
  await assert.rejects(app.getContext(), (e: BridgeRequestError) => e.code === 'TIMEOUT');
  assert.deepEqual(seen, []);
});

test('events reach the app only with the capability', async () => {
  const withRead = setup(['context.read']);
  const got: unknown[] = [];
  withRead.app.on('context.changed', (d) => got.push(d));
  assert.equal(withRead.host.emit('context.changed', { version: 2 }), true);
  assert.equal(withRead.host.emit('theme.changed', { theme: 'dark' }), true);
  await new Promise((r) => setTimeout(r, 10));
  assert.deepEqual(got, [{ version: 2 }]);
  const without = setup([]);
  assert.equal(without.host.emit('context.changed', { version: 2 }), false);
  assert.throws(() => without.host.emit('token.changed', {}));
});

test('close rejects what is pending', async () => {
  const { app } = setup(['context.read'], { getContext: () => new Promise(() => {}) });
  const p = app.getContext();
  app.close();
  await assert.rejects(p, (e: BridgeRequestError) => e.code === 'CLOSED');
});

test('parse follows the contract', () => {
  const ok: BridgeMessage[] = [
    { protocol: PROTOCOL, type: 'request', id: '1', method: 'context.get' },
    { protocol: PROTOCOL, type: 'response', id: '1', result: null },
    { protocol: PROTOCOL, type: 'response', id: '1', error: { code: 'X', message: 'x' } },
    { protocol: PROTOCOL, type: 'event', event: 'theme.changed' },
  ];
  for (const m of ok) assert.notEqual(parse(m), null, JSON.stringify(m));
  for (const bad of [
    { protocol: 'idp.bridge/v2', type: 'event', event: 'theme.changed' },
    { protocol: PROTOCOL, type: 'response', id: '1' },
    { protocol: PROTOCOL, type: 'response', id: '1', result: 1, error: { code: 'X', message: 'x' } },
    { protocol: PROTOCOL, type: 'request', id: 'bad id', method: 'context.get' },
    { protocol: PROTOCOL, type: 'request', id: '1', method: 'context.get', params: [] },
    { protocol: PROTOCOL, type: 'event', event: 'context.changed', id: '1' },
  ]) assert.equal(parse(bad), null, JSON.stringify(bad));
});

test('methods and events match the published contract', () => {
  const schema = JSON.parse(readFileSync(new URL('../../../api/contracts/schema.json', import.meta.url), 'utf8'));
  const props = schema.$defs.BridgeMessage.properties;
  assert.deepEqual([...props.method.enum].sort(), Object.keys(METHODS).sort());
  assert.deepEqual([...props.event.enum].sort(), Object.keys(EVENTS).sort());
  assert.equal(props.protocol.const, PROTOCOL);
});

test('webhook signatures: the same vector as the Go dispatcher', async () => {
  const secret = 'webhook-secret-of-at-least-32-bytes!!';
  const body = '{"invocationId":"x","input":{"orderId":"1001"}}';
  assert.equal(await sign(secret, '1790000000', body), '3a706ee8ac4b2e1d5c86db84aaac52f0ee5f6810586931f2574e621b071f27c1');
  const header = 't=1790000000,v1=3a706ee8ac4b2e1d5c86db84aaac52f0ee5f6810586931f2574e621b071f27c1';
  await verify(secret, header, body, { now: new Date(1790000000 * 1000 + 60_000) });
  await assert.rejects(verify(secret, header, body + ' ', { now: new Date(1790000000 * 1000) }), /mismatch/);
  await assert.rejects(verify(secret, header, body, { now: new Date(1790000000 * 1000 + 3_600_000) }), /tolerance/);
  await assert.rejects(verify(secret, 't=1,v1=zz', body), /malformed/);
  await assert.rejects(verify(secret, null, body), /malformed/);
});
