import { describe, expect, it } from 'vitest';
import { applyEvent, assistantText, newAssistantTurn, pendingConfirmations, turnsFromMessages } from '../src/agent/events.ts';
import type { AGUIEvent, AssistantTurn } from '../src/agent/events.ts';

const play = (events: AGUIEvent[], start: AssistantTurn = newAssistantTurn('ws_1')) => events.reduce((t, ev, i) => applyEvent(t, ev, String(i + 1)), start);

describe('applyEvent', () => {
  it('builds the agent turn in the order things happened: context, words, a tool call with its result, more words', () => {
    const t = play([
      { type: 'RUN_STARTED', threadId: 'thr', runId: 'ws_1' },
      { type: 'CUSTOM', name: 'idp.host/context.resolved', value: { packageId: 'pkg_1', items: 2, tokens: 340 } },
      { type: 'STEP_STARTED', stepName: 'step-1' },
      { type: 'TEXT_MESSAGE_START', messageId: 'm1', role: 'assistant' },
      { type: 'TEXT_MESSAGE_CONTENT', messageId: 'm1', delta: 'Let me ' },
      { type: 'TEXT_MESSAGE_CONTENT', messageId: 'm1', delta: 'check.' },
      { type: 'TEXT_MESSAGE_END', messageId: 'm1' },
      { type: 'TOOL_CALL_START', toolCallId: 'c1', toolCallName: 'sandbox__sandbox_run' },
      { type: 'TOOL_CALL_ARGS', toolCallId: 'c1', delta: '{"code":' },
      { type: 'TOOL_CALL_ARGS', toolCallId: 'c1', delta: '"print(1)"}' },
      { type: 'TOOL_CALL_END', toolCallId: 'c1' },
      { type: 'TOOL_CALL_RESULT', toolCallId: 'c1', content: 'exit 0\n1' },
      { type: 'CUSTOM', name: 'idp.host/model.usage', value: { step: 1, model: 'qwen', inputTokens: 100, outputTokens: 20 } },
      { type: 'STEP_STARTED', stepName: 'step-2' },
      { type: 'TEXT_MESSAGE_START', messageId: 'm2', role: 'assistant' },
      { type: 'TEXT_MESSAGE_CONTENT', messageId: 'm2', delta: 'It printed 1.' },
      { type: 'TEXT_MESSAGE_END', messageId: 'm2' },
      { type: 'RUN_FINISHED', outcome: { type: 'success' } },
    ]);
    expect(t.status).toBe('done');
    expect(t.steps).toBe(2);
    expect(t.lastEventId).toBe('18');
    expect(t.blocks.map((b) => b.kind)).toEqual(['context', 'text', 'tool', 'text']);
    expect(t.blocks[0]).toMatchObject({ kind: 'context', packageId: 'pkg_1', items: 2 });
    expect(t.blocks[1]).toMatchObject({ kind: 'text', text: 'Let me check.', done: true });
    expect(t.blocks[2]).toMatchObject({ kind: 'tool', call: { toolCallId: 'c1', tool: 'sandbox__sandbox_run', args: '{"code":"print(1)"}', argsDone: true, state: 'done', result: 'exit 0\n1' } });
    expect(t.usage).toEqual({ input: 100, output: 20, models: ['qwen'] });
    expect(assistantText(t)).toBe('Let me check.\n\nIt printed 1.');
  });

  it('stops for a confirmation, resumes after the decision, and reads the interrupt of RUN_FINISHED', () => {
    const paused = play([
      { type: 'RUN_STARTED' },
      { type: 'TOOL_CALL_START', toolCallId: 'c1', toolCallName: 'sandbox__sandbox_run' },
      { type: 'TOOL_CALL_ARGS', toolCallId: 'c1', delta: '{"code":"rm -rf /"}' },
      { type: 'TOOL_CALL_END', toolCallId: 'c1' },
      { type: 'CUSTOM', name: 'idp.host/confirmation.required', value: { toolCallId: 'c1', tool: 'sandbox__sandbox_run', arguments: { code: 'rm -rf /' }, expiresAt: '2026-11-07T12:00:00Z' } },
      { type: 'RUN_FINISHED', outcome: { type: 'interrupt', interrupts: [{ type: 'confirmation', toolCallId: 'c1', tool: 'sandbox__sandbox_run', expiresAt: '2026-11-07T12:00:00Z' }] } },
    ]);
    expect(paused.status).toBe('awaiting');
    expect(pendingConfirmations(paused)).toMatchObject([{ toolCallId: 'c1', expiresAt: '2026-11-07T12:00:00Z', args: '{"code":"rm -rf /"}' }]);
    const resumed = play([
      { type: 'CUSTOM', name: 'idp.host/confirmation.decided', value: { toolCallId: 'c1', decision: 'confirm', state: 'CONFIRMED' } },
      { type: 'RUN_STARTED' },
      { type: 'TOOL_CALL_RESULT', toolCallId: 'c1', content: 'refused by the sandbox' },
      { type: 'TEXT_MESSAGE_START', messageId: 'm1' },
      { type: 'TEXT_MESSAGE_CONTENT', messageId: 'm1', delta: 'Done.' },
      { type: 'RUN_FINISHED', outcome: 'success' },
    ], paused);
    expect(resumed.status).toBe('done');
    expect(pendingConfirmations(resumed)).toEqual([]);
    expect(resumed.blocks[0]).toMatchObject({ kind: 'tool', call: { state: 'done', result: 'refused by the sandbox' } });
    const rejected = play([{ type: 'CUSTOM', name: 'idp.host/confirmation.decided', value: { toolCallId: 'c1', decision: 'reject' } }, { type: 'TOOL_CALL_RESULT', toolCallId: 'c1', content: 'the user rejected the call' }], paused);
    expect(rejected.blocks[0]).toMatchObject({ kind: 'tool', call: { state: 'rejected' } });
    // an interrupt the shell cannot resolve is still a pause, not an end
    expect(play([{ type: 'RUN_FINISHED', outcome: 'interrupt' }]).status).toBe('awaiting');
  });

  it('tells an error and a cancellation apart, and keeps denied tools and guard notices', () => {
    const failed = play([{ type: 'TEXT_MESSAGE_START', messageId: 'm' }, { type: 'TEXT_MESSAGE_CONTENT', messageId: 'm', delta: 'half' }, { type: 'RUN_ERROR', code: 'RUN_TIMEOUT', message: 'the run exceeded 300s' }]);
    expect(failed.status).toBe('failed');
    expect(failed.error).toEqual({ code: 'RUN_TIMEOUT', message: 'the run exceeded 300s' });
    expect(failed.blocks[0]).toMatchObject({ kind: 'text', done: true });
    expect(play([{ type: 'RUN_ERROR', code: 'CANCELED', message: 'canceled by the owner' }]).status).toBe('canceled');
    const guarded = play([
      { type: 'CUSTOM', name: 'idp.host/tool.denied', value: { toolCallId: 'c9', tool: 'fs__delete', reason: 'tool not available to this agent' } },
      { type: 'CUSTOM', name: 'idp.host/guard.flagged', value: { source: 'tool', id: 'c1', outcome: 'redacted' } },
      { type: 'CUSTOM', name: 'idp.host/context.skipped', value: { reason: 'resolve failed' } },
    ]);
    expect(guarded.blocks).toMatchObject([
      { kind: 'tool', call: { tool: 'fs__delete', state: 'denied', reason: 'tool not available to this agent' } },
      { kind: 'notice', level: 'warn' },
      { kind: 'context', skipped: 'resolve failed' },
    ]);
  });
});

describe('turnsFromMessages', () => {
  it('turns a thread into user and agent turns, tool results attached to their calls, outcomes from the runs', () => {
    const turns = turnsFromMessages([
      { id: 'u1', role: 'user', content: 'Run print(6*7)' },
      { id: 'a1', role: 'assistant', content: '', toolCalls: [{ id: 'c1', type: 'function', function: { name: 'sandbox__sandbox_create', arguments: '{}' } }] },
      { id: 't1', role: 'tool', toolCallId: 'c1', content: '{"workspaceId":"w"}' },
      { id: 'a2', role: 'assistant', content: 'The answer is 42.' },
      { id: 'u2', role: 'user', content: 'And 7*7?' },
    ], [
      { runId: 'ws_1', status: 'COMPLETED', createdAt: '2026-11-07T01:00:00Z' },
      { runId: 'ws_2', status: 'FAILED', failureCode: 'RUN_TIMEOUT', failureMessage: 'too slow', createdAt: '2026-11-07T01:05:00Z' },
    ]);
    expect(turns.map((t) => t.kind)).toEqual(['user', 'assistant', 'user', 'assistant']);
    const first = turns[1] as AssistantTurn;
    expect(first.runId).toBe('ws_1');
    expect(first.status).toBe('done');
    expect(first.blocks).toMatchObject([
      { kind: 'tool', call: { toolCallId: 'c1', tool: 'sandbox__sandbox_create', args: '{}', result: '{"workspaceId":"w"}', state: 'done' } },
      { kind: 'text', text: 'The answer is 42.', done: true },
    ]);
    const second = turns[3] as AssistantTurn;
    expect(second).toMatchObject({ runId: 'ws_2', status: 'failed', error: { code: 'RUN_TIMEOUT', message: 'too slow' }, blocks: [] });
  });
});
