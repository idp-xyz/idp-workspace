// What the shell shows of a conversation with an agent (ADR-0001 D22): the turns of an idp-host thread,
// built from the thread's messages (history) and from a run's AG-UI events as they arrive (live). Pure
// functions: a reducer over events and a builder over messages, so that the rendering is testable
// without a browser and the event vocabulary of idp-host is read in one place.

export interface AGUIEvent { type: string; [k: string]: unknown }

export interface HostMessage {
  id: string;
  role: 'user' | 'assistant' | 'tool' | 'system' | string;
  content?: string;
  name?: string;
  toolCalls?: { id: string; type?: string; function: { name: string; arguments: string } }[];
  toolCallId?: string;
  error?: string;
}
export interface HostRun {
  runId: string; threadId?: string; status: string; failureCode?: string; failureMessage?: string;
  contextPackageId?: string; createdAt?: string; finishedAt?: string; cancelRequested?: boolean;
}
export interface HostToolCall {
  runId: string; toolCallId: string; tool: string; arguments?: string; state: string;
  confirmationExpiresAt?: string; decidedBy?: string; result?: string; isError?: boolean;
}

export type ToolState = 'running' | 'pending' | 'confirmed' | 'rejected' | 'expired' | 'denied' | 'done' | 'failed';
export interface ToolStep {
  toolCallId: string;
  tool: string;
  args: string;
  argsDone: boolean;
  state: ToolState;
  result?: string;
  isError?: boolean;
  expiresAt?: string;
  reason?: string;
  decidedBy?: string;
}
export type Block =
  | { kind: 'text'; id: string; text: string; done: boolean }
  | { kind: 'tool'; call: ToolStep }
  | { kind: 'context'; packageId?: string; items?: number; tokens?: number; skipped?: string; stale?: string }
  | { kind: 'notice'; level: 'info' | 'warn'; text: string };

export type RunStatus = 'queued' | 'running' | 'awaiting' | 'done' | 'failed' | 'canceled';
export interface Usage { input: number; output: number; models: string[] }
export interface AssistantTurn {
  kind: 'assistant';
  runId: string;
  status: RunStatus;
  blocks: Block[];
  steps: number;
  usage?: Usage;
  error?: { code: string; message: string };
  lastEventId: string;
}
export interface UserTurn { kind: 'user'; id: string; text: string }
export type Turn = UserTurn | AssistantTurn;

export function newAssistantTurn(runId: string, status: RunStatus = 'queued'): AssistantTurn {
  return { kind: 'assistant', runId, status, blocks: [], steps: 0, lastEventId: '' };
}

const str = (v: unknown): string => (typeof v === 'string' ? v : v === undefined || v === null ? '' : typeof v === 'object' ? JSON.stringify(v) : String(v));
const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

function withTool(turn: AssistantTurn, toolCallId: string, tool: string, update: (c: ToolStep) => ToolStep): AssistantTurn {
  const i = turn.blocks.findIndex((b) => b.kind === 'tool' && b.call.toolCallId === toolCallId);
  const blocks = turn.blocks.slice();
  if (i >= 0) {
    const b = blocks[i] as Extract<Block, { kind: 'tool' }>;
    blocks[i] = { kind: 'tool', call: update({ ...b.call, tool: b.call.tool || tool }) };
  } else {
    blocks.push({ kind: 'tool', call: update({ toolCallId, tool, args: '', argsDone: false, state: 'running' }) });
  }
  return { ...turn, blocks };
}

function withText(turn: AssistantTurn, id: string, f: (t: Extract<Block, { kind: 'text' }>) => Extract<Block, { kind: 'text' }>, create: boolean): AssistantTurn {
  let i = turn.blocks.findIndex((b) => b.kind === 'text' && b.id === id);
  if (i < 0 && !id) for (let j = turn.blocks.length - 1; j >= 0; j--) if (turn.blocks[j].kind === 'text' && !(turn.blocks[j] as { done: boolean }).done) { i = j; break; }
  const blocks = turn.blocks.slice();
  if (i >= 0) blocks[i] = f(blocks[i] as Extract<Block, { kind: 'text' }>);
  else if (create) blocks.push(f({ kind: 'text', id, text: '', done: false }));
  return { ...turn, blocks };
}

function closeText(turn: AssistantTurn): AssistantTurn {
  return { ...turn, blocks: turn.blocks.map((b) => (b.kind === 'text' && !b.done ? { ...b, done: true } : b)) };
}

/** the interrupts of a RUN_FINISHED outcome ({"type":"interrupt","interrupts":[…]} or the bare string), or null when the run ended */
export function interruptsOf(outcome: unknown): Record<string, unknown>[] | null {
  if (outcome === 'interrupt') return [];
  if (typeof outcome === 'object' && outcome !== null && (outcome as { type?: string }).type === 'interrupt') {
    const list = (outcome as { interrupts?: unknown }).interrupts;
    return Array.isArray(list) ? (list as Record<string, unknown>[]) : [];
  }
  return null;
}

/** applies one AG-UI event of the turn's run; id is the event's SSE id when it came from a stream */
export function applyEvent(turn: AssistantTurn, ev: AGUIEvent, id = ''): AssistantTurn {
  let t: AssistantTurn = id ? { ...turn, lastEventId: id } : turn;
  switch (ev.type) {
    case 'RUN_STARTED':
      return { ...t, status: t.status === 'awaiting' ? 'running' : t.status === 'queued' ? 'running' : t.status };
    case 'STEP_STARTED':
      return { ...t, steps: t.steps + 1 };
    case 'TEXT_MESSAGE_START':
      if (ev.role && ev.role !== 'assistant') return t;
      return withText(t, str(ev.messageId), (b) => b, true);
    case 'TEXT_MESSAGE_CONTENT':
      return withText(t, str(ev.messageId), (b) => ({ ...b, text: b.text + str(ev.delta) }), true);
    case 'TEXT_MESSAGE_END':
      return withText(t, str(ev.messageId), (b) => ({ ...b, done: true }), false);
    case 'TOOL_CALL_START':
      return withTool(closeText(t), str(ev.toolCallId), str(ev.toolCallName), (c) => c);
    case 'TOOL_CALL_ARGS':
      return withTool(t, str(ev.toolCallId), '', (c) => ({ ...c, args: c.args + str(ev.delta) }));
    case 'TOOL_CALL_END':
      return withTool(t, str(ev.toolCallId), '', (c) => ({ ...c, argsDone: true }));
    case 'TOOL_CALL_RESULT':
      return withTool(t, str(ev.toolCallId), '', (c) => ({ ...c, argsDone: true, result: str(ev.content),
        state: c.state === 'rejected' || c.state === 'denied' || c.state === 'expired' ? c.state : 'done' }));
    case 'CUSTOM':
      return custom(t, str(ev.name), (ev.value ?? ev) as Record<string, unknown>);
    case 'RUN_FINISHED': {
      const interrupts = interruptsOf(ev.outcome);
      t = closeText(t);
      if (interrupts === null) return { ...t, status: 'done' };
      for (const it of interrupts) {
        if (it.type !== 'confirmation') continue;
        t = withTool(t, str(it.toolCallId), str(it.tool), (c) => ({ ...c, state: c.state === 'running' || c.state === 'pending' ? 'pending' : c.state,
          argsDone: true, args: c.args || str(it.arguments), expiresAt: str(it.expiresAt) || c.expiresAt }));
      }
      return { ...t, status: 'awaiting' };
    }
    case 'RUN_ERROR': {
      const code = str(ev.code);
      return { ...closeText(t), status: code === 'CANCELED' ? 'canceled' : 'failed', error: { code, message: str(ev.message) } };
    }
    default:
      return t;
  }
}

function custom(t: AssistantTurn, name: string, v: Record<string, unknown>): AssistantTurn {
  switch (name) {
    case 'idp.host/confirmation.required':
      return { ...withTool(closeText(t), str(v.toolCallId), str(v.tool), (c) => ({ ...c, state: 'pending', argsDone: true, args: c.args || str(v.arguments), expiresAt: str(v.expiresAt) || c.expiresAt })), status: 'awaiting' };
    case 'idp.host/confirmation.decided': {
      const decision = str(v.decision), state = str(v.state);
      return { ...withTool(t, str(v.toolCallId), str(v.tool), (c) => ({ ...c, decidedBy: str(v.decidedBy) || c.decidedBy,
        state: decision === 'reject' || state === 'REJECTED' ? 'rejected' : state === 'EXPIRED' ? 'expired' : 'confirmed' })), status: 'running' };
    }
    case 'idp.host/tool.denied':
      return withTool(t, str(v.toolCallId), str(v.tool), (c) => ({ ...c, state: 'denied', argsDone: true, reason: str(v.reason) }));
    case 'idp.host/context.resolved':
      return { ...t, blocks: [...t.blocks, { kind: 'context', packageId: str(v.packageId), items: num(v.items), tokens: num(v.tokens) }] };
    case 'idp.host/context.skipped':
      return { ...t, blocks: [...t.blocks, { kind: 'context', skipped: str(v.reason) || 'skipped' }] };
    case 'idp.host/context.stale':
      return { ...t, blocks: [...t.blocks, { kind: 'context', stale: str(v.reason) || 'stale' }] };
    case 'idp.host/model.usage': {
      const input = num(v.inputTokens ?? v.promptTokens ?? v.input), output = num(v.outputTokens ?? v.completionTokens ?? v.output);
      const model = str(v.model);
      const u = t.usage ?? { input: 0, output: 0, models: [] };
      return { ...t, usage: { input: u.input + input, output: u.output + output, models: model && !u.models.includes(model) ? [...u.models, model] : u.models } };
    }
    case 'idp.host/guard.flagged':
      return { ...t, blocks: [...t.blocks, { kind: 'notice', level: 'warn', text: `输入护栏：${str(v.source)} ${str(v.outcome)}${v.findings ? ' · ' + str(v.findings) : ''}` }] };
    case 'idp.host/guard.blocked':
      return { ...t, blocks: [...t.blocks, { kind: 'notice', level: 'warn', text: `输出被护栏拦下：${str(v.rule)}${v.detail ? ' · ' + str(v.detail) : ''}` }] };
    case 'idp.host/subagent.started':
      return { ...t, blocks: [...t.blocks, { kind: 'notice', level: 'info', text: `委派给子智能体 ${str(v.agent ?? v.agentId ?? v.childRunId)}` }] };
    case 'idp.host/subagent.finished':
      return { ...t, blocks: [...t.blocks, { kind: 'notice', level: 'info', text: `子智能体完成${v.status ? '：' + str(v.status) : ''}` }] };
    default:
      return t;
  }
}

/** the tool calls of the turn that wait for the member */
export function pendingConfirmations(turn: AssistantTurn): ToolStep[] {
  return turn.blocks.flatMap((b) => (b.kind === 'tool' && b.call.state === 'pending' ? [b.call] : []));
}

/** the assistant's words of the turn, text blocks joined */
export function assistantText(turn: AssistantTurn): string {
  return turn.blocks.flatMap((b) => (b.kind === 'text' ? [b.text] : [])).join('\n\n').trim();
}

const terminal = (status: string) => status === 'COMPLETED' || status === 'FAILED' || status === 'CANCELED';
export const runStatusOf = (s: string): RunStatus =>
  s === 'COMPLETED' ? 'done' : s === 'FAILED' ? 'failed' : s === 'CANCELED' ? 'canceled' : s === 'AWAITING_INPUT' ? 'awaiting' : s === 'QUEUED' ? 'queued' : 'running';
export const isTerminal = (s: string) => terminal(s);

/** the turns of a thread from idp-host's messages: a user message opens a turn, what follows until the
 * next one is the agent's; runs (oldest first) give the agent's turns their run id and outcome */
export function turnsFromMessages(messages: HostMessage[], runs: HostRun[] = []): Turn[] {
  const out: Turn[] = [];
  let current: AssistantTurn | null = null;
  let users = 0;
  const runFor = (i: number): HostRun | undefined => runs[i];
  for (const m of messages) {
    if (m.role === 'user') {
      if (current === null && out.length > 0) { // a run without an answer (it failed, or still runs): its turn is empty
        out.push(turnOf(runFor(users - 1), newAssistantTurn(runFor(users - 1)?.runId ?? '', 'done')));
      }
      out.push({ kind: 'user', id: m.id, text: m.content ?? '' });
      users++;
      current = null;
      continue;
    }
    if (m.role === 'system') continue;
    if (current === null) {
      current = newAssistantTurn(runFor(users - 1)?.runId ?? '', 'done');
      out.push(current);
    }
    let next: AssistantTurn = current;
    if (m.role === 'assistant') {
      if (m.content) next = withText(next, m.id, (b) => ({ ...b, text: m.content ?? '', done: true }), true);
      for (const c of m.toolCalls ?? []) next = withTool(next, c.id, c.function.name, (s) => ({ ...s, args: c.function.arguments ?? '', argsDone: true }));
      if (m.error) next = { ...next, blocks: [...next.blocks, { kind: 'notice', level: 'warn', text: m.error }] };
    } else if (m.role === 'tool') {
      next = withTool(next, m.toolCallId ?? '', m.name ?? '', (s) => ({ ...s, result: m.content ?? '', argsDone: true, state: s.state === 'running' ? 'done' : s.state }));
    }
    out[out.length - 1] = next;
    current = next;
  }
  if (current === null && out.length > 0 && users > 0) out.push(turnOf(runFor(users - 1), newAssistantTurn(runFor(users - 1)?.runId ?? '', 'done')));
  // the outcome of each run, by order
  let i = 0;
  return out.map((t) => (t.kind === 'assistant' ? turnOf(runFor(i++), t) : t));
}

function turnOf(run: HostRun | undefined, turn: AssistantTurn): AssistantTurn {
  if (!run) return turn;
  const status = runStatusOf(run.status);
  const error = run.failureCode || run.failureMessage ? { code: run.failureCode ?? '', message: run.failureMessage ?? '' } : turn.error;
  return { ...turn, runId: run.runId || turn.runId, status, error };
}
