import { useState } from 'react';
import { ApiError } from './api.ts';
import type { Api } from './api.ts';
import type { InboxItem, InstalledApp, Workspace } from './types.ts';

const priorityLabel: Record<InboxItem['priority'], string> = { urgent: '紧急', high: '高', normal: '普通', low: '低' };
const kindLabel: Record<InboxItem['kind'], string> = { approval: '审批', task: '任务', run: '运行', notification: '通知' };

export interface InboxProps {
  api: Api;
  ws: Workspace;
  items: InboxItem[];
  apps: InstalledApp[];
  confirm(question: string): Promise<boolean>;
  notify(message: string, level: string): void;
  onChanged(item: InboxItem): void;
}

// The inbox shows what other systems own; its buttons forward actions, its marks are the member's
// own (ADR-0001 D3). An item changes status only when its producer says so.
export function Inbox({ api, ws, items, apps, confirm, notify, onChanged }: InboxProps) {
  const [busy, setBusy] = useState<string | null>(null);
  const visible = items.filter((i) => i.status === 'open' && !i.archivedAt);
  if (visible.length === 0) return <p className="muted">收件箱是空的。</p>;
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
      notify(e instanceof ApiError ? `${contract.title} 失败：${e.code} ${e.message}` : String(e), 'error');
    } finally {
      setBusy(null);
    }
  };
  const mark = async (item: InboxItem, marks: { read?: boolean; archived?: boolean }) => {
    try {
      onChanged(await api.mark(ws.name, item.itemId, marks));
    } catch (e) {
      notify(String(e), 'error');
    }
  };
  return (
    <ul className="inbox">
      {visible.map((item) => (
        <li key={item.itemId} className={`item ${item.readAt ? 'read' : 'unread'} p-${item.priority}`} onClick={() => !item.readAt && mark(item, { read: true })}>
          <div className="item-head">
            <span className="kind">{kindLabel[item.kind]}</span>
            <span className="prio">{priorityLabel[item.priority]}</span>
            <strong>{item.title}</strong>
          </div>
          {item.summary && <p className="summary">{item.summary}</p>}
          <div className="meta">
            {item.entities.map((e) => <span key={`${e.type}:${e.id}`} className="entity">{e.type}:{e.id}</span>)}
            {item.dueAt && <span>到期 {new Date(item.dueAt).toLocaleString()}</span>}
            <span>来自 {item.ref.system}</span>
          </div>
          <div className="actions" onClick={(ev) => ev.stopPropagation()}>
            {item.actions.map((a) => (
              <button key={`${a.app}/${a.action}`} disabled={busy === item.itemId} onClick={() => act(item, a)}>{a.label ?? a.action}</button>
            ))}
            <button className="secondary" onClick={() => mark(item, { archived: true })}>归档</button>
          </div>
        </li>
      ))}
    </ul>
  );
}
