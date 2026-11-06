import { describe, expect, it } from 'vitest';
import { BridgeError } from '@idp-workspace/bridge/shell';
import { ApiError } from '../src/api.ts';
import type { Api } from '../src/api.ts';
import { shellHandlers } from '../src/bridgeHandlers.ts';
import type { InstalledApp } from '../src/types.ts';

const ws = { workspaceId: 'w1', name: 'us-ops', displayName: 'US' };
const app: InstalledApp = {
  installation: { installationId: 'i', app: 'orders', version: '1.2.0' }, digest: 'sha256:x', origin: 'https://orders.example',
  manifest: { name: 'orders', version: '1.2.0', title: 'Orders', publisher: 'p', entry: { url: 'https://orders.example/' }, capabilities: ['context.read', 'context.write', 'actions.invoke'],
    actions: [
      { name: 'order.hold', title: 'Hold', input: { type: 'object' }, target: { kind: 'webhook', url: 'https://orders.example/h' } },
      { name: 'approval.approve', title: 'Approve', input: { type: 'object' }, target: { kind: 'eer.decision', decision: 'approve' }, confirm: true },
    ] },
};

function fakeApi() {
  const calls: unknown[][] = [];
  let ctx = { workspaceId: 'w1', subject: 'bob', entities: [{ type: 'order', id: '1001' }], version: 3 };
  const api = {
    context: async () => ctx,
    setContext: async (_ws: string, c: typeof ctx & { app?: string }, version: number) => { calls.push(['setContext', c, version]); ctx = { ...ctx, entities: c.entities, version: version + 1 }; return ctx; },
    invoke: async (_ws: string, call: Record<string, unknown>) => {
      calls.push(['invoke', call]);
      if (call.action === 'order.hold' && (call.input as Record<string, unknown>).orderId === 'x') throw new ApiError(422, 'INVALID_INPUT', '/orderId: does not match pattern');
      return { invocationId: 'inv-1', app: 'orders', action: String(call.action), status: 'succeeded', result: { httpStatus: 200 } };
    },
    inbox: async () => [
      { itemId: 'i1', kind: 'approval', title: 'T', priority: 'high', status: 'open', entities: [], summary: 'secret detail', producer: 'svc', externalRef: 'execution:1/approval' },
      { itemId: 'i2', kind: 'run', title: 'Ask — finished', priority: 'normal', status: 'open', entities: [], summary: 'the answer', producer: 'idp-workspace', externalRef: 'run:r1' },
      { itemId: 'i3', kind: 'run', title: 'Other — finished', priority: 'normal', status: 'open', entities: [], summary: 'not yours', producer: 'idp-workspace', externalRef: 'run:r2' },
    ],
    // the member's invocation records: run r1 was started by this app, r2 by another one
    invocations: async () => [
      { invocationId: 'v1', app: 'orders', action: 'order.ask', target: { kind: 'host.run', agent: 'a' }, status: 'succeeded', result: { id: 'r1' } },
      { invocationId: 'v2', app: 'notes', action: 'notes.ask', target: { kind: 'host.run', agent: 'a' }, status: 'succeeded', result: { id: 'r2' } },
    ],
  } as unknown as Api;
  return { api, calls };
}

describe('shellHandlers', () => {
  it('scopes every call to the workspace and the app; never exposes a token', async () => {
    const { api, calls } = fakeApi();
    const notes: string[] = [];
    const h = shellHandlers(api, ws, app, { notify: (m) => notes.push(m), confirm: async () => true, locale: 'zh-CN', theme: 'light' });
    const hello = await h.hello!({});
    expect(JSON.stringify(hello)).not.toMatch(/token|Bearer/i);
    expect(hello).toMatchObject({ workspace: { name: 'us-ops' }, app: 'orders' });
    expect(await h.getContext!()).toEqual({ entities: [{ type: 'order', id: '1001' }], thread: undefined, itemId: undefined, version: 3 });
    expect(await h.setContext!({ entities: [{ type: 'order', id: '1002' }] })).toEqual({ entities: [{ type: 'order', id: '1002' }], version: 4 });
    expect(calls[0]).toEqual(['setContext', expect.objectContaining({ app: 'orders', entities: [{ type: 'order', id: '1002' }] }), 3]);
    await expect(h.setContext!({ entities: 'order:1' })).rejects.toBeInstanceOf(BridgeError);
    expect(await h.invokeAction!({ action: 'order.hold', input: { orderId: '1001' } })).toEqual({ invocationId: 'inv-1', status: 'succeeded', result: { httpStatus: 200 } });
    expect(calls.at(-1)).toEqual(['invoke', { app: 'orders', action: 'order.hold', input: { orderId: '1001' } }]);
    await expect(h.invokeAction!({ action: 'order.hold', input: { orderId: 'x' } })).rejects.toMatchObject({ code: 'INVALID_INPUT' });
    await expect(h.invokeAction!({ action: 'order.delete' })).rejects.toMatchObject({ code: 'ACTION_NOT_FOUND' });
    const list = await h.listInbox!({}) as Array<Record<string, unknown>>;
    expect(list[0]).not.toHaveProperty('summary');
    h.notify!({ message: 'saved', level: 'success' });
    expect(notes).toEqual(['Orders: saved']);
  });

  it('shows an app the outcome of the runs it started for the member, and of no other item (D20)', async () => {
    const { api } = fakeApi();
    const h = shellHandlers(api, ws, app, { notify: () => {}, confirm: async () => true, locale: 'en', theme: 'light' });
    const list = await h.listInbox!({}) as Array<Record<string, unknown>>;
    expect(list.map((i) => i.itemId)).toEqual(['i1', 'i2', 'i3']);
    expect(list[0]).not.toHaveProperty('summary');
    expect(list[1]).toMatchObject({ externalRef: 'run:r1', summary: 'the answer' });
    expect(list[2]).not.toHaveProperty('summary');
    expect(list[2]).not.toHaveProperty('externalRef');
  });

  it('asks the member before a confirm action; a decline never reaches the API', async () => {
    const { api, calls } = fakeApi();
    let asked = '';
    const h = shellHandlers(api, ws, app, { notify: () => {}, confirm: async (q) => { asked = q; return false; }, locale: 'en', theme: 'light' });
    await expect(h.invokeAction!({ action: 'approval.approve', input: {}, itemId: 'i1' })).rejects.toMatchObject({ code: 'DECLINED' });
    expect(asked).toContain('Approve');
    expect(calls).toHaveLength(0);
  });
});
