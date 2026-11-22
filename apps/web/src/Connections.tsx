import { useCallback, useEffect, useRef, useState } from 'react';
import type { Api } from './api.ts';
import { STATUS_WORDS, connectionError, connectionPath, isAuthorizationUrl, reasonWords } from './connections.ts';
import type { Connection, ConnectionAccount } from './types.ts';

export interface ConnectionsProps {
  api: Api;
  confirm(question: string): Promise<boolean>;
  notify(message: string, level: string): void;
  /** the connection the member was pointed at (from a conversation's prompt): shown first and marked */
  focus?: string;
  /** where the browser goes to authorize (the page's address bar in the app; a stand-in in tests) */
  navigate?(url: string): void;
}

const STATUS_CLASS: Record<string, string> = { connected: 'ok', reauthorization_required: 'warn', not_connected: 'idle', not_required: 'idle' };

// "Connected services" (ADR-0001 D30): the third-party services the member's agents can use as them. The member connects each
// once, at the third party's own page; the tokens stay in idp-mcp-bridge and are never shown here. Titles and ids come from the
// server and are shown as plain text.
export function ConnectionsView({ api, confirm, notify, focus, navigate = (u) => window.location.assign(u) }: ConnectionsProps) {
  const [items, setItems] = useState<Connection[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const focusRef = useRef<HTMLLIElement | null>(null);
  const load = useCallback(async () => {
    try {
      setItems(await api.connections());
      setError(null);
    } catch (e) {
      setError(connectionError(e, '读取已连接的服务失败'));
    }
  }, [api]);
  useEffect(() => { void load(); }, [load]);
  useEffect(() => { focusRef.current?.scrollIntoView?.({ block: 'center' }); }, [items, focus]);

  const connect = async (c: Connection, reviewer: boolean) => {
    const key = `${c.id}${reviewer ? ':reviewer' : ''}`;
    setBusy(key);
    try {
      // coming back lands on this connection's own page
      const url = await api.connect(c.id, { reviewer, returnPath: connectionPath(c.id) });
      if (!isAuthorizationUrl(url)) throw new Error('not an address');
      navigate(url);
    } catch (e) {
      notify(e instanceof Error && e.message === 'not an address' ? '连接失败：没有得到可用的授权地址' : connectionError(e, '连接失败'), 'error');
      setBusy(null);
    }
  };
  const disconnect = async (c: Connection, reviewer: boolean) => {
    const who = reviewer ? `评审账号（${c.title}）` : c.title;
    const q = reviewer
      ? `断开 ${who}？之后这个连接的工具不能再被发现和评审，直到重新连接。`
      : `断开 ${who}？你的智能体将不能再以你的身份使用它，对方那里的授权也会撤销。`;
    if (!(await confirm(q))) return;
    setBusy(`${c.id}${reviewer ? ':reviewer' : ''}`);
    try {
      await api.disconnect(c.id, reviewer);
      notify(`已断开 ${who}`, 'success');
    } catch (e) {
      notify(connectionError(e, '断开失败'), 'error');
    } finally {
      setBusy(null);
      await load(); // the server's list is the truth
    }
  };

  if (error && items === null) return <p className="error" role="alert">{error}</p>;
  if (items === null) return <p className="muted">加载中…</p>;
  const ordered = focus ? [...items.filter((c) => c.id === focus), ...items.filter((c) => c.id !== focus)] : items;
  return (
    <div className="connections">
      <p className="muted">你的智能体可以以你自己的身份使用这些第三方服务。每个服务只需要连接一次，在对方的页面上授权；令牌由连接桥保管，这里看不到，智能体也看不到。</p>
      {error && <p className="error" role="alert">{error}</p>}
      {focus && !items.some((c) => c.id === focus) && <p className="notice warn" role="status">没有找到你要连接的服务（{focus}）。它可能已被运维移除。</p>}
      {ordered.length === 0 ? <p className="muted">还没有可连接的服务。运维在连接桥上配置之后，会出现在这里。</p> : (
        <ul className="connection-list">
          {ordered.map((c) => (
            <li key={c.id} ref={c.id === focus ? focusRef : undefined} className={c.id === focus ? 'focus' : undefined} aria-current={c.id === focus ? 'true' : undefined}>
              <div className="head">
                <strong className="title">{c.title}</strong>
                <span className={`status ${STATUS_CLASS[c.status] ?? 'idle'}`}>{STATUS_WORDS[c.status]}</span>
              </div>
              {c.status === 'reauthorization_required' && reasonWords(c.reason) && <p className="reason">{reasonWords(c.reason)}</p>}
              {c.status === 'connected' && c.scope && <p className="muted scope">已授予：{c.scope}</p>}
              <div className="actions">
                {c.status === 'not_connected' && <button disabled={busy !== null} onClick={() => connect(c, false)}>连接</button>}
                {c.status === 'reauthorization_required' && <button disabled={busy !== null} onClick={() => connect(c, false)}>重新授权</button>}
                {(c.status === 'connected' || c.status === 'reauthorization_required') && <button className="secondary" disabled={busy !== null} onClick={() => disconnect(c, false)}>断开</button>}
              </div>
              {c.reviewer && <Reviewer account={c.reviewer} busy={busy !== null} onConnect={() => connect(c, true)} onDisconnect={() => disconnect(c, true)} />}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// the reviewer account (运维): the connection's own account at the third party, used to discover and review its tools, never for a member's calls
function Reviewer({ account, busy, onConnect, onDisconnect }: { account: ConnectionAccount; busy: boolean; onConnect(): void; onDisconnect(): void }) {
  return (
    <div className="reviewer">
      <div className="head">
        <span className="label">评审账号（运维）</span>
        <span className={`status ${STATUS_CLASS[account.status] ?? 'idle'}`}>{STATUS_WORDS[account.status]}</span>
      </div>
      {account.status === 'reauthorization_required' && reasonWords(account.reason) && <p className="reason">{reasonWords(account.reason)}</p>}
      <p className="muted">只用于发现、评审和检测这个连接的工具变化，不用于任何成员的调用。</p>
      <div className="actions">
        {account.status === 'not_connected' && <button disabled={busy} onClick={onConnect}>连接评审账号</button>}
        {account.status === 'reauthorization_required' && <button disabled={busy} onClick={onConnect}>重新授权评审账号</button>}
        {(account.status === 'connected' || account.status === 'reauthorization_required') && <button className="secondary" disabled={busy} onClick={onDisconnect}>断开评审账号</button>}
      </div>
    </div>
  );
}

