import { useMemo, useState } from 'react';
import type { Api } from './api.ts';
import { Markdown } from './agent/markdown.tsx';
import { friendly } from './errors.ts';
import type { InboxItem, InstalledApp, Workspace } from './types.ts';

const priorityLabel: Record<InboxItem['priority'], string> = { urgent: '紧急', high: '高', normal: '普通', low: '低' };
const kindLabel: Record<InboxItem['kind'], string> = { approval: '审批', task: '任务', run: '运行', notification: '通知' };
// what a run's producer appends to the item's title as the run goes on (internal/runs, D11 / D18)
const runStates: [string, string, string][] = [
  [' — running', '运行中', 'running'], [' — awaiting confirmation', '等待确认', 'awaiting'], [' — waiting for input', '等待输入', 'awaiting'],
  [' — finished', '已完成', 'done'], [' — failed', '失败', 'failed'], [' — result unavailable', '结果不可用', 'failed'],
];
type Tab = 'open' | 'all' | 'archived';

export interface InboxProps {
  api: Api;
  ws: Workspace;
  items: InboxItem[];
  apps: InstalledApp[];
  confirm(question: string): Promise<boolean>;
  notify(message: string, level: string): void;
  onChanged(item: InboxItem): void;
  /** opens the conversation a run item belongs to (D22); absent when there is nothing to talk to */
  onOpenRun?(runId: string): void;
}

/** a run item's title split from the state its producer appended */
export function splitTitle(title: string): { base: string; state?: string; tone?: string } {
  for (const [suffix, label, tone] of runStates) if (title.endsWith(suffix)) return { base: title.slice(0, -suffix.length), state: label, tone };
  return { base: title };
}

// The inbox shows what other systems own; its buttons forward actions, its marks are the member's
// own (ADR-0001 D3). An item changes status only when its producer says so.
export function Inbox({ api, ws, items, apps, confirm, notify, onChanged, onOpenRun }: InboxProps) {
  const [busy, setBusy] = useState<string | null>(null);
  const [tab, setTab] = useState<Tab>('open');
  const [kind, setKind] = useState<InboxItem['kind'] | ''>('');
  const [query, setQuery] = useState('');
  const counts = useMemo(() => ({
    open: items.filter((i) => i.status === 'open' && !i.archivedAt).length,
    all: items.filter((i) => !i.archivedAt).length,
    archived: items.filter((i) => i.archivedAt).length,
  }), [items]);
  const visible = items.filter((i) => (tab === 'archived' ? !!i.archivedAt : tab === 'all' ? !i.archivedAt : i.status === 'open' && !i.archivedAt))
    .filter((i) => !kind || i.kind === kind)
    .filter((i) => !query.trim() || `${i.title}\n${i.summary ?? ''}`.toLowerCase().includes(query.trim().toLowerCase()));
  const act = async (item: InboxItem, a: InboxItem['actions'][number]) => {
    const app = apps.find((x) => x.manifest.name === a.app);
    const contract = app?.manifest.actions.find((c) => c.name === a.action);
    if (!app || !contract) {
      notify(`${a.app}/${a.action} 不可用（应用未安装或已撤销）`, 'error');
      return;
    }
    if (contract.confirm && !(await confirm(`${contract.title}：${item.title}？`))) return;
    setBusy(item.itemId);
    try {
      const inv = await api.invoke(ws.name, { app: a.app, action: a.action, itemId: item.itemId, input: {} });
      notify(`${contract.title}：已提交（${inv.invocationId.slice(0, 8)}）`, 'success');
    } catch (e) {
      notify(friendly(e, `${contract.title} 失败`), 'error');
    } finally {
      setBusy(null);
    }
  };
  const mark = async (item: InboxItem, marks: { read?: boolean; archived?: boolean }) => {
    try {
      onChanged(await api.mark(ws.name, item.itemId, marks));
    } catch (e) {
      notify(friendly(e), 'error');
    }
  };
  const filtered = kind !== '' || query.trim() !== '';
  return (
    <section className="inbox-view">
      <div className="inbox-bar">
        <div className="tabs" role="tablist" aria-label="收件箱">
          {([['open', '待处理', counts.open], ['all', '全部', counts.all], ['archived', '已归档', counts.archived]] as const).map(([id, label, n]) => (
            <button key={id} role="tab" aria-selected={tab === id} className={tab === id ? 'active' : 'secondary'} onClick={() => setTab(id)}>{label}{n > 0 && <span className="count">{n}</span>}</button>
          ))}
        </div>
        <div className="inbox-filters">
          <select aria-label="种类" value={kind} onChange={(e) => setKind(e.target.value as InboxItem['kind'] | '')}>
            <option value="">所有种类</option>
            {(Object.keys(kindLabel) as InboxItem['kind'][]).map((k) => <option key={k} value={k}>{kindLabel[k]}</option>)}
          </select>
          <input type="search" aria-label="搜索" placeholder="搜索标题或内容" value={query} onChange={(e) => setQuery(e.target.value)} />
        </div>
      </div>
      {visible.length === 0 && <p className="muted">{filtered ? '没有符合条件的事项。' : tab === 'open' ? '收件箱是空的。' : tab === 'archived' ? '没有归档的事项。' : '还没有事项。'}</p>}
      <ul className="inbox">
        {visible.map((item) => {
          const { base, state, tone } = splitTitle(item.title);
          const run = item.ref.system === 'host' && item.ref.kind === 'run';
          return (
            <li key={item.itemId} className={`item ${item.readAt ? 'read' : 'unread'} p-${item.priority} ${item.status !== 'open' ? 'closed' : ''}`} onClick={() => !item.readAt && mark(item, { read: true })}>
              <div className="item-head">
                <span className="kind">{kindLabel[item.kind]}</span>
                {item.priority !== 'normal' && <span className="prio">{priorityLabel[item.priority]}</span>}
                <strong>{base}</strong>
                {state && <span className={`state ${tone}`}>{state}</span>}
                {item.status === 'resolved' && <span className="state done">已处理{item.resolution?.outcome ? `：${item.resolution.outcome}` : ''}</span>}
                {item.status === 'withdrawn' && <span className="state failed">已撤回</span>}
              </div>
              {item.summary && <div className="summary"><Markdown text={item.summary} /></div>}
              <div className="meta">
                {item.entities.map((e) => <span key={`${e.type}:${e.id}`} className="entity">{e.type}:{e.id}</span>)}
                {item.dueAt && <span>到期 {new Date(item.dueAt).toLocaleString()}</span>}
                <span>来自 {item.ref.system}</span>
                <span title={new Date(item.updatedAt).toLocaleString()}>{ago(item.updatedAt)}</span>
              </div>
              <div className="actions" onClick={(ev) => ev.stopPropagation()}>
                {item.status === 'open' && item.actions.map((a) => (
                  <button key={`${a.app}/${a.action}`} disabled={busy === item.itemId} onClick={() => act(item, a)}>{a.label ?? a.action}</button>
                ))}
                {run && onOpenRun && <button className="secondary" onClick={() => onOpenRun(item.ref.id)}>在对话中打开</button>}
                {!item.archivedAt
                  ? <button className="secondary" onClick={() => mark(item, { archived: true })}>归档</button>
                  : <button className="secondary" onClick={() => mark(item, { archived: false })}>取消归档</button>}
              </div>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function ago(iso: string): string {
  const d = Date.now() - new Date(iso).getTime();
  if (!Number.isFinite(d)) return '';
  if (d < 60_000) return '刚刚';
  if (d < 3_600_000) return `${Math.floor(d / 60_000)} 分钟前`;
  if (d < 86_400_000) return `${Math.floor(d / 3_600_000)} 小时前`;
  return new Date(iso).toLocaleDateString();
}
