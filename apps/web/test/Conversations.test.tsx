import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { Conversations, chatAgents } from '../src/Conversations.tsx';
import type { Api } from '../src/api.ts';
import type { AGUIEvent } from '../src/agent/events.ts';
import type { Conversation, InstalledApp } from '../src/types.ts';

const ws = { workspaceId: 'w1', name: 'us-ops', displayName: 'US' };
const message = { type: 'object', required: ['message'], properties: { message: { type: 'string' } }, additionalProperties: false };
const apps: InstalledApp[] = [
  { installation: { installationId: 'x', app: 'orders', version: '1.0.0' }, digest: 'd', manifest: { name: 'orders', version: '1.0.0', title: 'Orders', publisher: 'p', capabilities: [], actions: [
    { name: 'order.review', title: 'Ask the order assistant', target: { kind: 'host.run', agent: 'order-assistant' }, input: message },
    { name: 'order.refund', title: 'Refund', target: { kind: 'webhook', url: 'https://x/y' }, input: { type: 'object' } },
    { name: 'order.batch', title: 'Batch', target: { kind: 'host.run', agent: 'batch' }, input: { type: 'object', required: ['orderIds'], properties: { orderIds: { type: 'array' } } } },
  ] } },
  { installation: { installationId: 'y', app: 'coder', version: '1.0.0' }, digest: 'd', manifest: { name: 'coder', version: '1.0.0', title: 'Coder', publisher: 'p', capabilities: [], actions: [
    { name: 'code.run', title: 'Run code', target: { kind: 'host.run', agent: 'sandbox-assistant' }, input: message, roles: ['dev'] },
  ] } },
];

type Script = { events: AGUIEvent[]; last: string };

function fakeApi(opts: { conversations?: Conversation[]; scripts?: Script[] } = {}) {
  const calls: Record<string, unknown[][]> = { invoke: [], runEvents: [], decide: [], cancelRun: [] };
  const scripts = [...(opts.scripts ?? [])];
  let runs = 0;
  const started: Conversation[] = []; // what the workspace lists once an invocation is recorded
  const api = {
    workspace: async () => ({ workspace: ws, roles: ['approver'], member: true }),
    conversations: async () => [...started, ...(opts.conversations ?? [])],
    threadMessages: async (_w: string, t: string) => (t === 'thr_old'
      ? [{ id: 'u1', role: 'user', content: 'Where is order 1001?' }, { id: 'a1', role: 'assistant', content: 'Order 1001 waits for customs.' }]
      : [{ id: 'u1', role: 'user', content: 'first question' }]),
    threadRuns: async () => [{ runId: 'ws_old', status: 'COMPLETED', createdAt: '2026-11-07T01:00:00Z' }],
    invoke: async (w: string, call: Record<string, unknown>) => {
      calls.invoke.push([w, call]);
      runs++;
      const now = new Date().toISOString();
      const c = started.find((x) => x.threadId === 'thr_1');
      if (c) { c.runs++; c.lastAt = now; c.lastRunId = `ws_${runs}`; } else started.push({ threadId: 'thr_1', app: String(call.app), action: String(call.action), agent: 'order-assistant', runs: 1, firstAt: now, lastAt: now, lastRunId: `ws_${runs}` });
      return { invocationId: `inv-${runs}`, app: call.app, action: call.action, status: 'succeeded', result: { system: 'host', kind: 'run', id: `ws_${runs}`, threadId: 'thr_1' } };
    },
    runEvents: async (w: string, runId: string, after: string, onEvent: (ev: AGUIEvent, id: string) => void) => {
      calls.runEvents.push([runId, after]);
      const s = scripts.shift() ?? { events: [{ type: 'RUN_FINISHED' }], last: '1' };
      s.events.forEach((ev, i) => onEvent(ev, String(Number(after || 0) + i + 1)));
      return s.last;
    },
    decide: async (w: string, runId: string, call: string, decision: string) => { calls.decide.push([runId, call, decision]); return { toolCallId: call, state: 'CONFIRMED' }; },
    cancelRun: async (w: string, runId: string) => { calls.cancelRun.push([runId]); return { runId, cancelRequested: true }; },
  } as unknown as Api;
  return { api, calls };
}

const props = (api: Api) => ({ api, ws, apps, notify: vi.fn(), confirm: async () => true });

describe('chatAgents', () => {
  it('offers the host.run actions that take a message, as the member\'s roles allow', () => {
    expect(chatAgents(apps, null).map((a) => a.key)).toEqual(['orders/order.review', 'coder/code.run']);
    expect(chatAgents(apps, ['approver']).map((a) => a.key)).toEqual(['orders/order.review']);
    expect(chatAgents(apps, ['dev']).map((a) => a.title)).toEqual(['Orders · Ask the order assistant', 'Coder · Run code']);
  });
});

describe('Conversations', () => {
  it('sends a question, shows the run as it streams - context, words, a tool call - and continues the thread', async () => {
    const { api, calls } = fakeApi({ scripts: [{ events: [
      { type: 'RUN_STARTED' },
      { type: 'CUSTOM', name: 'idp.host/context.resolved', value: { packageId: 'pkg', items: 2 } },
      { type: 'TEXT_MESSAGE_START', messageId: 'm1' }, { type: 'TEXT_MESSAGE_CONTENT', messageId: 'm1', delta: 'Checking ' }, { type: 'TEXT_MESSAGE_CONTENT', messageId: 'm1', delta: '**now**.' }, { type: 'TEXT_MESSAGE_END', messageId: 'm1' },
      { type: 'TOOL_CALL_START', toolCallId: 'c1', toolCallName: 'sandbox__sandbox_run' }, { type: 'TOOL_CALL_ARGS', toolCallId: 'c1', delta: '{"code":"print(1)"}' }, { type: 'TOOL_CALL_END', toolCallId: 'c1' },
      { type: 'TOOL_CALL_RESULT', toolCallId: 'c1', content: '1' },
      { type: 'RUN_FINISHED', outcome: { type: 'success' } },
    ], last: '11' }, { events: [{ type: 'TEXT_MESSAGE_START', messageId: 'm2' }, { type: 'TEXT_MESSAGE_CONTENT', messageId: 'm2', delta: 'Second answer.' }, { type: 'RUN_FINISHED' }], last: '3' }] });
    render(<Conversations {...props(api)} />);
    expect(await screen.findByText('还没有对话')).toBeTruthy();
    expect((screen.getByLabelText('智能体') as HTMLSelectElement).value).toBe('orders/order.review');
    const box = screen.getByLabelText('消息');
    fireEvent.change(box, { target: { value: 'Is order 1001 late?' } });
    fireEvent.keyDown(box, { key: 'Enter' });
    await waitFor(() => expect(calls.invoke).toHaveLength(1));
    expect(calls.invoke[0][1]).toEqual({ app: 'orders', action: 'order.review', input: { message: 'Is order 1001 late?' } });
    expect(screen.getByText('Is order 1001 late?', { selector: '.bubble' })).toBeTruthy(); // the question, at once
    await waitFor(() => expect(screen.getByText('now').tagName).toBe('STRONG')); // the answer, rendered as Markdown
    expect(screen.getByText('依据了 2 条企业知识')).toBeTruthy();
    expect(screen.getByText('sandbox · sandbox_run')).toBeTruthy();
    expect(screen.getByText('完成')).toBeTruthy();
    expect(calls.runEvents).toEqual([['ws_1', '']]);
    // the conversation is now in the list, titled by its first question
    const list = screen.getByLabelText('对话列表');
    expect(within(list).getByText('Is order 1001 late?')).toBeTruthy();
    // the next question continues the same idp-host thread
    fireEvent.change(box, { target: { value: 'And 1002?' } });
    fireEvent.click(screen.getByLabelText('发送'));
    await waitFor(() => expect(calls.invoke).toHaveLength(2));
    expect(calls.invoke[1][1]).toMatchObject({ threadId: 'thr_1', input: { message: 'And 1002?' } });
    await screen.findByText('Second answer.');
  });

  it('stops at a tool call that needs the member, forwards their decision and reads on from the last event', async () => {
    const expires = new Date(Date.now() + 10 * 60_000).toISOString();
    const { api, calls } = fakeApi({ scripts: [
      { events: [
        { type: 'RUN_STARTED' },
        { type: 'TOOL_CALL_START', toolCallId: 'c7', toolCallName: 'sandbox__sandbox_run' }, { type: 'TOOL_CALL_ARGS', toolCallId: 'c7', delta: '{"code":"import os"}' }, { type: 'TOOL_CALL_END', toolCallId: 'c7' },
        { type: 'CUSTOM', name: 'idp.host/confirmation.required', value: { toolCallId: 'c7', tool: 'sandbox__sandbox_run', arguments: { code: 'import os' }, expiresAt: expires } },
        { type: 'RUN_FINISHED', outcome: { type: 'interrupt', interrupts: [{ type: 'confirmation', toolCallId: 'c7', tool: 'sandbox__sandbox_run', expiresAt: expires }] } },
      ], last: '6' },
      { events: [
        { type: 'CUSTOM', name: 'idp.host/confirmation.decided', value: { toolCallId: 'c7', decision: 'confirm', state: 'CONFIRMED' } },
        { type: 'RUN_STARTED' }, { type: 'TOOL_CALL_RESULT', toolCallId: 'c7', content: 'ok' },
        { type: 'TEXT_MESSAGE_START', messageId: 'm' }, { type: 'TEXT_MESSAGE_CONTENT', messageId: 'm', delta: 'Imported.' }, { type: 'RUN_FINISHED', outcome: { type: 'success' } },
      ], last: '12' },
    ] });
    render(<Conversations {...props(api)} />);
    await screen.findByText('还没有对话');
    fireEvent.change(screen.getByLabelText('消息'), { target: { value: 'import os please' } });
    fireEvent.click(screen.getByLabelText('发送'));
    const allow = await screen.findByText('允许');
    expect(screen.getByText('等待确认')).toBeTruthy();
    expect(screen.getByText(/过期视为拒绝/)).toBeTruthy();
    expect(screen.getByText('等待你的决定')).toBeTruthy();
    expect((screen.getByLabelText('发送') as HTMLButtonElement).disabled).toBe(true); // one run at a time in a conversation
    fireEvent.click(allow);
    await waitFor(() => expect(calls.decide).toEqual([['ws_1', 'c7', 'confirm']]));
    await screen.findByText('Imported.');
    expect(calls.runEvents).toEqual([['ws_1', ''], ['ws_1', '6']]); // resumed after the interruption, not from the start
    expect(screen.getByText('完成')).toBeTruthy();
    expect((screen.getByLabelText('发送') as HTMLButtonElement).disabled).toBe(true); // nothing typed yet
  });

  it('opens an earlier conversation from the list and shows its history', async () => {
    const { api, calls } = fakeApi({ conversations: [{ threadId: 'thr_old', app: 'orders', action: 'order.review', agent: 'order-assistant', runs: 1, firstAt: '2026-11-07T01:00:00Z', lastAt: '2026-11-07T01:00:00Z', lastRunId: 'ws_old' }] });
    render(<Conversations {...props(api)} />);
    const item = await screen.findByText('Where is order 1001?'); // the title, from the first message
    fireEvent.click(item);
    await screen.findByText('Order 1001 waits for customs.');
    expect(screen.getAllByText('Where is order 1001?').length).toBe(2); // in the list and as the question
    expect(calls.runEvents).toEqual([]); // a finished run is not followed
    fireEvent.click(screen.getByText('＋ 新对话'));
    await waitFor(() => expect(screen.queryByText('Order 1001 waits for customs.')).toBeNull());
  });

  it('tells a refused invocation in the member\'s words and offers a retry', async () => {
    const { api, calls } = fakeApi();
    (api as unknown as { invoke: unknown }).invoke = async () => { throw Object.assign(new Error('http://idp-host:8450 answered 403 FORBIDDEN'), { code: 'TARGET_REJECTED' }); };
    render(<Conversations {...props(api)} />);
    await screen.findByText('还没有对话');
    fireEvent.change(screen.getByLabelText('消息'), { target: { value: 'hi' } });
    fireEvent.click(screen.getByLabelText('发送'));
    await screen.findByText('重试');
    expect(screen.queryByText(/idp-host:8450/)).toBeNull();
    expect(calls.runEvents).toEqual([]);
  });
});
