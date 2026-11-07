import { useCallback, useEffect, useState } from 'react';
import { ApiError } from './api.ts';
import type { Api } from './api.ts';
import type { MemoryItem } from './types.ts';

const CUT = 300;

export interface MemoryProps {
  api: Api;
  confirm(question: string): Promise<boolean>;
  notify(message: string, level: string): void;
}

const day = (iso?: string) => (iso ? new Date(iso).toLocaleDateString() : '');

// "My memory" (ADR-0001 D21): what idp-host remembered of the member's conversations in idp-cmi.
// The member lists and deletes it; the server calls idp-cmi with the member's own token.
export function MemoryView({ api, confirm, notify }: MemoryProps) {
  const [items, setItems] = useState<MemoryItem[] | null>(null);
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => {
    try {
      setItems(await api.memory());
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? `${e.code} ${e.message}` : String(e));
    }
  }, [api]);
  useEffect(() => { void load(); }, [load]);

  const fail = (e: unknown) => notify(e instanceof ApiError ? `删除失败：${e.code} ${e.message}` : String(e), 'error');
  const removeOne = async (m: MemoryItem) => {
    setBusy(true);
    try {
      await api.deleteMemory(m.id);
      notify('已删除', 'success');
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
      await load(); // refresh: the server's list is the truth (a 404 means it is already gone)
    }
  };
  const removeAll = async () => {
    if (!(await confirm('删除全部记忆？此操作不能撤销。'))) return;
    setBusy(true);
    try {
      const r = await api.deleteAllMemory();
      notify(`已删除 ${r.revoked} 条`, 'success');
    } catch (e) {
      fail(e);
    } finally {
      setBusy(false);
      await load();
    }
  };

  if (error && items === null) return <p className="error" role="alert">{error}</p>;
  if (items === null) return <p className="muted">加载中…</p>;
  const expiry = items.find((m) => m.validUntil);
  return (
    <div>
      <p className="muted">这些是助手从你的对话里记下的内容，{expiry ? '会保留到到期日，' : ''}可随时删除。</p>
      {error && <p className="error" role="alert">{error}</p>}
      {items.length === 0 ? <p className="muted">还没有记忆。和助手聊几句，它会记住。</p> : (
        <>
          <div className="actions"><button className="secondary" disabled={busy} onClick={removeAll}>删除全部</button></div>
          <ul className="memory">
            {items.map((m) => {
              const long = m.content.length > CUT;
              const shown = long && !open[m.id] ? m.content.slice(0, CUT) + '…' : m.content;
              return (
                <li key={m.id}>
                  <p className="text">{shown}</p>
                  {long && <button className="secondary" onClick={() => setOpen((o) => ({ ...o, [m.id]: !o[m.id] }))}>{open[m.id] ? '收起' : '显示更多'}</button>}
                  <div className="meta">
                    <span>记于 {day(m.createdAt)}</span>
                    {m.validUntil && <span>到期 {day(m.validUntil)}</span>}
                    {m.ref && <span>来自运行 {m.ref}</span>}
                  </div>
                  <div className="actions"><button disabled={busy} onClick={() => removeOne(m)}>删除</button></div>
                </li>
              );
            })}
          </ul>
        </>
      )}
    </div>
  );
}
