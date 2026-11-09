import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent, KeyboardEvent } from 'react';
import type { Api } from './api.ts';
import { friendly, runFailure } from './errors.ts';
import { launchable } from './Launcher.tsx';
import { Markdown } from './agent/markdown.tsx';
import { applyEvent, assistantText, isTerminal, newAssistantTurn, pendingConfirmations, turnsFromMessages } from './agent/events.ts';
import type { AssistantTurn, Block, RunStatus, ToolStep, Turn } from './agent/events.ts';
import type { ActionContract, Conversation, InstalledApp, Workspace } from './types.ts';

// Conversations with the agents (ADR-0001 D22): a member talks to an agent an installed app offers
// (a host.run action that takes a message), sees the run as it happens - the words as they stream, each
// tool call with its arguments and result, the enterprise context it drew on - decides the tool calls
// that wait for them, cancels, and comes back to the conversation later. The workspace relays; idp-host
// owns the thread, the runs and the decisions.

export interface ChatAgent {
  key: string;
  app: InstalledApp;
  contract: ActionContract;
  agent: string;
  title: string;
}

/** the agents a member can talk to: host.run actions whose input is a message */
export function chatAgents(apps: InstalledApp[], roles: string[] | null): ChatAgent[] {
  return apps.flatMap((app) => launchable(app, roles)
    .filter((c) => c.target.kind === 'host.run' && takesMessage(c.input))
    .map((c) => ({ key: `${app.manifest.name}/${c.name}`, app, contract: c, agent: c.target.agent ?? '', title: `${app.manifest.title} · ${c.title}` })));
}

function takesMessage(schema: Record<string, unknown>): boolean {
  const props = (schema.properties ?? {}) as Record<string, { type?: string }>;
  const m = props.message;
  if (!m || (m.type && m.type !== 'string')) return false;
  const required = Array.isArray(schema.required) ? (schema.required as string[]) : [];
  return required.every((r) => r === 'message');
}

const TITLE_KEY = (ws: string, thread: string) => `idp.conversation-title/${ws}/${thread}`;
const AGENT_KEY = (ws: string) => `idp.conversation-agent/${ws}`;

export interface ConversationsProps {
  api: Api;
  ws: Workspace;
  apps: InstalledApp[];
  notify(message: string, level: string): void;
  confirm(question: string): Promise<boolean>;
  /** a conversation to open (from the inbox); the thread id */
  open?: string | null;
}

export function Conversations({ api, ws, apps, notify, confirm, open }: ConversationsProps) {
  const [roles, setRoles] = useState<string[] | null | undefined>(undefined);
  const [list, setList] = useState<Conversation[]>([]);
  const [titles, setTitles] = useState<Record<string, string>>({});
  const [agentKey, setAgentKey] = useState<string>(() => localStorage.getItem(AGENT_KEY(ws.name)) ?? '');
  const [threadId, setThreadId] = useState<string | null>(open ?? null);
  const [turns, setTurns] = useState<Turn[]>([]);
  const [loading, setLoading] = useState(false);
  const [sideOpen, setSideOpen] = useState(false);
  const [draft, setDraft] = useState('');
  const follow = useRef<AbortController | null>(null);
  const resume = useRef<(() => void) | null>(null);
  const skipLoad = useRef<string | null>(null);
  const turnsRef = useRef<Turn[]>([]);
  turnsRef.current = turns;

  const agents = useMemo(() => chatAgents(apps, roles ?? null), [apps, roles]);
  const agent = agents.find((a) => a.key === agentKey) ?? agents[0] ?? null;
  const live = turns.length ? turns[turns.length - 1] : null;
  const busy = live?.kind === 'assistant' && (live.status === 'queued' || live.status === 'running' || live.status === 'awaiting');

  // the member's roles decide which actions are offered (the server decides again)
  useEffect(() => {
    let on = true;
    setRoles(undefined);
    api.workspace(ws.name).then((w) => { if (on) setRoles(w.roles); }).catch(() => { if (on) setRoles(null); });
    return () => { on = false; };
  }, [api, ws]);

  const refreshList = useCallback(() => api.conversations(ws.name).then(setList).catch(() => {}), [api, ws]);
  useEffect(() => { void refreshList(); }, [refreshList]);

  // a conversation's title is its first question; read once from idp-host, kept in this browser
  useEffect(() => {
    const missing = list.filter((c) => !titles[c.threadId]).slice(0, 25);
    if (missing.length === 0) return;
    let on = true;
    (async () => {
      for (const c of missing) {
        const cached = localStorage.getItem(TITLE_KEY(ws.name, c.threadId));
        let title = cached ?? '';
        if (!title) {
          try {
            const msgs = await api.threadMessages(ws.name, c.threadId);
            title = msgs.find((m) => m.role === 'user')?.content?.trim() ?? '';
            if (title) localStorage.setItem(TITLE_KEY(ws.name, c.threadId), title.slice(0, 200));
          } catch { title = ''; }
        }
        if (!on) return;
        setTitles((t) => ({ ...t, [c.threadId]: title || '（无标题）' }));
      }
    })();
    return () => { on = false; };
  }, [api, ws, list, titles]);

  useEffect(() => { if (open) { setThreadId(open); setSideOpen(false); } }, [open]);
  useEffect(() => { if (agent) localStorage.setItem(AGENT_KEY(ws.name), agent.key); }, [agent, ws]);

  const stopFollowing = useCallback(() => {
    follow.current?.abort();
    follow.current = null;
    resume.current = null;
  }, []);

  /** reads the run's events into the turn at index until the run ends; waits through a decision */
  const followRun = useCallback((runId: string, index: number, from: string, start: AssistantTurn) => {
    stopFollowing();
    const ctrl = new AbortController();
    follow.current = ctrl;
    let turn = start;
    let last = from;
    const show = () => { const t = turn; setTurns((ts) => ts.map((x, i) => (i === index ? t : x))); };
    (async () => {
      for (;;) {
        try {
          last = await api.runEvents(ws.name, runId, last, (ev, id) => { turn = applyEvent(turn, ev, id); show(); }, ctrl.signal);
        } catch (e) {
          if (ctrl.signal.aborted) return;
          turn = { ...turn, status: 'failed', error: { code: 'STREAM', message: friendly(e) } };
          show();
          return;
        }
        if (ctrl.signal.aborted) return;
        if (turn.status === 'done' || turn.status === 'failed' || turn.status === 'canceled') { void refreshList(); return; }
        if (turn.status === 'awaiting') {
          await new Promise<void>((r) => { resume.current = r; }); // the member decides (or cancels); then read on
          if (ctrl.signal.aborted) return;
          continue;
        }
        await new Promise((r) => setTimeout(r, 1500)); // the stream broke while the run goes on: back after a moment
        if (ctrl.signal.aborted) return;
      }
    })();
  }, [api, ws, stopFollowing, refreshList]);

  // the history of the selected conversation, and the run still going at its end
  useEffect(() => {
    if (threadId && skipLoad.current === threadId) { skipLoad.current = null; return; } // the conversation this run just opened: already followed
    stopFollowing();
    if (!threadId) { setTurns([]); return; }
    let on = true;
    setLoading(true);
    setTurns([]);
    (async () => {
      try {
        const [messages, runs] = await Promise.all([api.threadMessages(ws.name, threadId), api.threadRuns(ws.name, threadId).catch(() => [])]);
        if (!on) return;
        const ordered = runs.slice().sort((a, b) => (a.createdAt ?? '').localeCompare(b.createdAt ?? ''));
        let ts = turnsFromMessages(messages, ordered);
        const lastRun = ordered[ordered.length - 1];
        if (lastRun && !isTerminal(lastRun.status)) {
          // what the run did so far comes from its events, not from the messages it has not written yet
          const lastUser = ts.map((t) => t.kind).lastIndexOf('user');
          ts = ts.slice(0, lastUser + 1);
          const start = newAssistantTurn(lastRun.runId, 'running');
          ts.push(start);
          setTurns(ts);
          setLoading(false);
          followRun(lastRun.runId, ts.length - 1, '', start);
          return;
        }
        setTurns(ts);
      } catch (e) {
        if (on) notify(friendly(e, '读取对话失败'), 'error');
      } finally {
        if (on) setLoading(false);
      }
    })();
    return () => { on = false; };
  }, [api, ws, threadId, followRun, stopFollowing, notify]);
  useEffect(() => () => stopFollowing(), [stopFollowing]);

  const send = async (text: string) => {
    if (!agent || busy) return;
    const question = text.trim();
    if (!question) return;
    setDraft('');
    const index = turnsRef.current.length + 1;
    const pending = newAssistantTurn('', 'queued');
    setTurns((ts) => [...ts, { kind: 'user', id: `local-${Date.now()}`, text: question }, pending]);
    try {
      const inv = await api.invoke(ws.name, { app: agent.app.manifest.name, action: agent.contract.name, input: { message: question }, ...(threadId ? { threadId } : {}) });
      const res = (inv.result ?? {}) as { id?: string; threadId?: string };
      if (inv.status !== 'succeeded' || !res.id) {
        const message = inv.error ? friendly(Object.assign(new Error(inv.error.message), { code: inv.error.code })) : '没有拿到运行的编号';
        setTurns((ts) => ts.map((t, i) => (i === index && t.kind === 'assistant' ? { ...t, status: 'failed', error: { code: inv.error?.code ?? 'INVOKE', message } } : t)));
        return;
      }
      if (!threadId && res.threadId) {
        skipLoad.current = res.threadId;
        localStorage.setItem(TITLE_KEY(ws.name, res.threadId), question.slice(0, 200));
        setTitles((t) => ({ ...t, [res.threadId!]: question }));
        setList((l) => [{ threadId: res.threadId!, app: agent.app.manifest.name, action: agent.contract.name, agent: agent.agent, runs: 1, firstAt: new Date().toISOString(), lastAt: new Date().toISOString(), lastRunId: res.id! }, ...l]);
        setThreadId(res.threadId);
      }
      const start = { ...pending, runId: res.id, status: 'running' as RunStatus };
      setTurns((ts) => ts.map((t, i) => (i === index ? start : t)));
      followRun(res.id, index, '', start);
    } catch (e) {
      const message = friendly(e);
      setTurns((ts) => ts.map((t, i) => (i === index && t.kind === 'assistant' ? { ...t, status: 'failed', error: { code: 'INVOKE', message } } : t)));
    }
  };

  const decide = async (turn: AssistantTurn, call: ToolStep, decision: 'confirm' | 'reject') => {
    try {
      await api.decide(ws.name, turn.runId, call.toolCallId, decision);
      setTurns((ts) => ts.map((t) => (t === turn ? { ...t, status: 'running', blocks: t.blocks.map((b) => (b.kind === 'tool' && b.call.toolCallId === call.toolCallId ? { kind: 'tool', call: { ...b.call, state: decision === 'confirm' ? 'confirmed' : 'rejected' } } : b)) } : t)));
      resume.current?.();
    } catch (e) {
      notify(friendly(e, decision === 'confirm' ? '确认失败' : '拒绝失败'), 'error');
    }
  };

  const cancel = async (turn: AssistantTurn) => {
    if (!(await confirm('停止这次运行？'))) return;
    try {
      await api.cancelRun(ws.name, turn.runId);
      resume.current?.();
      notify('已请求停止', 'info');
    } catch (e) {
      notify(friendly(e, '停止失败'), 'error');
    }
  };

  const retry = (index: number) => {
    const prev = turns[index - 1];
    if (prev?.kind === 'user') void send(prev.text);
  };

  const newConversation = () => { stopFollowing(); setThreadId(null); setTurns([]); setSideOpen(false); };
  const select = (c: Conversation) => {
    const key = `${c.app}/${c.action}`;
    if (agents.some((a) => a.key === key)) setAgentKey(key);
    setThreadId(c.threadId);
    setSideOpen(false);
  };

  if (roles === undefined) return <p className="muted">正在读取你在本工作空间的角色…</p>;
  if (agents.length === 0) return <p className="muted">没有可以对话的智能体。管理员在工作空间里安装带 <code>host.run</code> 动作的应用后，它们会出现在这里。</p>;
  const current = list.find((c) => c.threadId === threadId);
  const shownAgent = current ? agents.find((a) => a.key === `${current.app}/${current.action}`) ?? agent : agent;
  return (
    <div className={`chat ${sideOpen ? 'side-open' : ''}`}>
      <aside className="chat-side" aria-label="对话列表">
        <div className="chat-side-head">
          <button type="button" className="primary-wide" onClick={newConversation}>＋ 新对话</button>
          <label className="field">
            <span>智能体</span>
            <select aria-label="智能体" value={agent?.key ?? ''} onChange={(e) => { setAgentKey(e.target.value); if (threadId) newConversation(); }}>
              {agents.map((a) => <option key={a.key} value={a.key}>{a.title}</option>)}
            </select>
          </label>
        </div>
        <ul className="chat-list">
          {list.length === 0 && <li className="muted empty">还没有对话</li>}
          {list.map((c) => (
            <li key={c.threadId}>
              <button type="button" className={`chat-item ${c.threadId === threadId ? 'active' : ''}`} onClick={() => select(c)}>
                <span className="chat-item-title">{titles[c.threadId] ?? '…'}</span>
                <span className="chat-item-meta">{agents.find((a) => a.key === `${c.app}/${c.action}`)?.title ?? c.agent} · {when(c.lastAt)}</span>
              </button>
            </li>
          ))}
        </ul>
      </aside>
      <section className="chat-main">
        <div className="chat-head">
          <button type="button" className="ghost side-toggle" onClick={() => setSideOpen((v) => !v)} aria-label="显示对话列表">☰</button>
          <div className="chat-head-title">
            <strong>{shownAgent?.title ?? '对话'}</strong>
            {shownAgent && <span className="muted"> · 智能体 {shownAgent.agent}</span>}
          </div>
          {live?.kind === 'assistant' && busy && <button type="button" className="secondary" onClick={() => cancel(live)}>停止</button>}
        </div>
        <div className="transcript" role="log" aria-live="polite">
          {loading && <p className="muted">正在读取对话…</p>}
          {!loading && turns.length === 0 && (
            <div className="chat-welcome">
              <h2>{shownAgent?.contract.title}</h2>
              <p className="muted">{shownAgent?.contract.description ?? '提出问题或交代任务。智能体的每一步 — 调用了什么工具、依据了哪些企业知识 — 都会显示在这里；需要你确认的操作会停下来等你。'}</p>
            </div>
          )}
          {turns.map((t, i) => (t.kind === 'user'
            ? <div key={`u${i}`} className="turn user"><div className="bubble">{t.text}</div></div>
            : <AssistantView key={`a${i}`} turn={t} onDecide={(call, d) => decide(t, call, d)} onRetry={() => retry(i)} onCancel={() => cancel(t)} />))}
        </div>
        <Composer agent={shownAgent} disabled={busy || !shownAgent} draft={draft} setDraft={setDraft} onSend={send} />
      </section>
    </div>
  );
}

function Composer({ agent, disabled, draft, setDraft, onSend }: { agent: ChatAgent | null; disabled: boolean; draft: string; setDraft(v: string): void; onSend(text: string): Promise<void> }) {
  const ref = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
  }, [draft]);
  const submit = (e: FormEvent) => { e.preventDefault(); if (!disabled) void onSend(draft); };
  const key = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); if (!disabled) void onSend(draft); }
  };
  return (
    <form className="composer" onSubmit={submit}>
      <textarea ref={ref} rows={1} aria-label="消息" placeholder={agent ? `问 ${agent.contract.title}…` : ''} value={draft} disabled={!agent}
        onChange={(e) => setDraft(e.target.value)} onKeyDown={key} />
      <button type="submit" disabled={disabled || !draft.trim()} aria-label="发送">{disabled ? '运行中…' : '发送'}</button>
      <small className="muted hint">Enter 发送，Shift+Enter 换行</small>
    </form>
  );
}

function AssistantView({ turn, onDecide, onRetry, onCancel }: { turn: AssistantTurn; onDecide(call: ToolStep, d: 'confirm' | 'reject'): void; onRetry(): void; onCancel(): void }) {
  const pending = pendingConfirmations(turn);
  return (
    <div className={`turn assistant status-${turn.status}`}>
      <div className="answer">
        {turn.blocks.map((b, i) => <BlockView key={i} block={b} streaming={turn.status === 'running' && i === turn.blocks.length - 1} onDecide={onDecide} />)}
        {turn.status === 'queued' && <p className="muted thinking">正在排队…</p>}
        {turn.status === 'running' && turn.blocks.length === 0 && <p className="muted thinking">思考中…</p>}
        {turn.status === 'awaiting' && pending.length === 0 && <p className="muted">智能体在等待一个工作台里无法给出的输入。</p>}
        {(turn.status === 'failed' || turn.status === 'canceled') && (
          <div className={`run-end ${turn.status}`}>
            <span>{turn.status === 'canceled' ? '已取消' : runFailure(turn.error?.code ?? '', turn.error?.message ?? '')}</span>
            <button type="button" className="secondary" onClick={onRetry}>重试</button>
          </div>
        )}
        <RunFooter turn={turn} onCancel={onCancel} />
      </div>
    </div>
  );
}

function RunFooter({ turn, onCancel }: { turn: AssistantTurn; onCancel(): void }) {
  const tools = turn.blocks.filter((b) => b.kind === 'tool').length;
  const parts: string[] = [];
  if (turn.steps > 1) parts.push(`${turn.steps} 步`);
  if (tools) parts.push(`${tools} 次工具调用`);
  if (turn.usage && (turn.usage.input || turn.usage.output)) parts.push(`${turn.usage.input + turn.usage.output} tokens`);
  if (turn.usage?.models.length) parts.push(turn.usage.models.join(', '));
  if (turn.status === 'done' && parts.length === 0) return null;
  return (
    <div className="run-foot muted">
      {turn.status === 'running' && <span className="spinner" aria-hidden="true" />}
      {turn.status === 'running' && <span>运行中</span>}
      {turn.status === 'awaiting' && <span>等待你的决定</span>}
      {parts.length > 0 && <span>{parts.join(' · ')}</span>}
      {(turn.status === 'running' || turn.status === 'awaiting') && <button type="button" className="ghost" onClick={onCancel}>停止</button>}
    </div>
  );
}

function BlockView({ block, streaming, onDecide }: { block: Block; streaming: boolean; onDecide(call: ToolStep, d: 'confirm' | 'reject'): void }) {
  switch (block.kind) {
    case 'text':
      return <div className={`text ${!block.done && streaming ? 'streaming' : ''}`}><Markdown text={block.text} /></div>;
    case 'tool':
      return <ToolCard call={block.call} onDecide={onDecide} />;
    case 'context':
      if (block.skipped) return <div className="context-chip muted" title={block.skipped}>没有带上企业知识（{block.skipped}）</div>;
      if (block.stale) return <div className="context-chip muted">企业知识在运行中有更新</div>;
      return <div className="context-chip" title={block.packageId}>依据了 {block.items ?? 0} 条企业知识{block.tokens ? ` · ${block.tokens} tokens` : ''}</div>;
    case 'notice':
      return <div className={`notice ${block.level}`}>{block.text}</div>;
  }
}

const TOOL_STATE: Record<ToolStep['state'], string> = { running: '调用中', pending: '等待确认', confirmed: '已确认', rejected: '已拒绝', expired: '已过期', denied: '不允许', done: '完成', failed: '失败' };

export function toolLabel(tool: string): string {
  const [server, ...rest] = tool.split('__');
  return rest.length ? `${server} · ${rest.join('__')}` : tool;
}

function pretty(args: string): string {
  try { return JSON.stringify(JSON.parse(args), null, 2); } catch { return args; }
}

function ToolCard({ call, onDecide }: { call: ToolStep; onDecide(call: ToolStep, d: 'confirm' | 'reject'): void }) {
  const [open, setOpen] = useState(call.state === 'pending');
  useEffect(() => { if (call.state === 'pending') setOpen(true); }, [call.state]);
  const args = pretty(call.args);
  return (
    <div className={`tool state-${call.state}`}>
      <button type="button" className="tool-head" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
        <span className="tool-icon" aria-hidden="true">{call.state === 'running' ? '◌' : call.state === 'pending' ? '⏸' : call.state === 'done' || call.state === 'confirmed' ? '✓' : '×'}</span>
        <span className="tool-name">{toolLabel(call.tool)}</span>
        <span className={`tool-state ${call.state}`}>{TOOL_STATE[call.state]}</span>
        <span className="tool-caret" aria-hidden="true">{open ? '▾' : '▸'}</span>
      </button>
      {open && (
        <div className="tool-body">
          {call.args && <div className="tool-section"><span className="label">参数</span><pre className="mono">{args}</pre></div>}
          {call.reason && <div className="tool-section"><span className="label">原因</span><span>{call.reason}</span></div>}
          {call.result !== undefined && <Result text={call.result} />}
          {call.state === 'pending' && <Approval call={call} onDecide={onDecide} />}
        </div>
      )}
    </div>
  );
}

function Result({ text }: { text: string }) {
  const [all, setAll] = useState(false);
  const limit = 1200;
  const shown = all || text.length <= limit ? text : text.slice(0, limit);
  return (
    <div className="tool-section">
      <span className="label">结果</span>
      <pre className="mono">{shown}{!all && text.length > limit && '…'}</pre>
      {text.length > limit && <button type="button" className="ghost" onClick={() => setAll((v) => !v)}>{all ? '收起' : `展开全部（${text.length} 字符）`}</button>}
    </div>
  );
}

function Approval({ call, onDecide }: { call: ToolStep; onDecide(call: ToolStep, d: 'confirm' | 'reject'): void }) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => { const t = setInterval(() => setNow(Date.now()), 1000); return () => clearInterval(t); }, []);
  const [busy, setBusy] = useState<'confirm' | 'reject' | null>(null);
  const left = call.expiresAt ? Math.max(0, Math.floor((new Date(call.expiresAt).getTime() - now) / 1000)) : null;
  const mmss = left === null ? '' : `${String(Math.floor(left / 60)).padStart(2, '0')}:${String(left % 60).padStart(2, '0')}`;
  const go = (d: 'confirm' | 'reject') => { setBusy(d); onDecide(call, d); };
  return (
    <div className="approval" role="group" aria-label="需要你的确认">
      <p><strong>智能体想要调用 {toolLabel(call.tool)}。</strong> 允许它执行吗？{left !== null && (left > 0 ? ` 请求将在 ${mmss} 后过期，过期视为拒绝。` : ' 请求已过期。')}</p>
      <div className="actions">
        <button type="button" disabled={busy !== null || left === 0} onClick={() => go('confirm')}>允许</button>
        <button type="button" className="secondary" disabled={busy !== null || left === 0} onClick={() => go('reject')}>拒绝</button>
      </div>
    </div>
  );
}

export function when(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return '';
  const d = Date.now() - t;
  if (d < 60_000) return '刚刚';
  if (d < 3_600_000) return `${Math.floor(d / 60_000)} 分钟前`;
  if (d < 86_400_000) return `${Math.floor(d / 3_600_000)} 小时前`;
  if (d < 7 * 86_400_000) return `${Math.floor(d / 86_400_000)} 天前`;
  return new Date(iso).toLocaleDateString();
}

export { assistantText };
