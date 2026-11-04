import { useCallback, useEffect, useMemo, useState } from 'react';
import { Api, ApiError } from './api.ts';
import { Admin } from './Admin.tsx';
import { AppFrame } from './AppFrame.tsx';
import { Inbox } from './Inbox.tsx';
import type { ShellUI } from './bridgeHandlers.ts';
import type { InboxItem, InstalledApp, Me, Workspace } from './types.ts';

interface Toast { id: number; message: string; level: string }

export function App(props: { api?: Api }) {
  // one client for the life of the shell: a new one per render would re-run every effect below
  const [api] = useState(() => props.api ?? new Api());
  const [token, setToken] = useState(api.token);
  const [devMode, setDevMode] = useState(false);
  const [me, setMe] = useState<Me | null>(null);
  const [workspaces, setWorkspaces] = useState<Workspace[]>([]);
  const [ws, setWs] = useState<Workspace | null>(null);
  const [apps, setApps] = useState<InstalledApp[]>([]);
  const [items, setItems] = useState<InboxItem[]>([]);
  const [view, setView] = useState<string>('inbox');
  const [toasts, setToasts] = useState<Toast[]>([]);
  const [error, setError] = useState<string | null>(null);

  const notify = useCallback((message: string, level: string) => {
    const id = Date.now() + Math.random();
    setToasts((t) => [...t, { id, message, level }]);
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 5000);
  }, []);
  const ui: ShellUI = useMemo(() => ({ notify, confirm: async (q: string) => window.confirm(q), locale: navigator.language, theme: 'light' }), [notify]);

  useEffect(() => { api.shellConfig().then((c) => setDevMode(c.devMode)).catch(() => {}); }, [api]);
  useEffect(() => {
    if (!token) { setMe(null); return; }
    api.me().then(setMe).catch(() => {});
    setError(null); // a new sign-in starts clean: errors of an earlier token are not this session's
    api.workspaces().then((list) => {
      setWorkspaces(list);
      setWs((cur) => cur ?? list[0] ?? null);
    }).catch((e) => {
      if (e instanceof ApiError && e.status === 401) { api.signOut(); setToken(null); }
      setError(String(e));
    });
  }, [api, token]);
  useEffect(() => {
    if (!ws) return;
    const abort = new AbortController();
    setItems([]);
    setError(null);
    api.apps(ws.name).then(setApps).catch((e) => setError(String(e)));
    // the stream replays what is open, then pushes changes (ADR-0001 D10)
    api.inbox(ws.name).then(setItems).catch((e) => setError(String(e)));
    api.stream(ws.name, (it) => setItems((cur) => [it, ...cur.filter((x) => x.itemId !== it.itemId)]), abort.signal).catch((e) => setError(String(e)));
    return () => abort.abort();
  }, [api, ws]);

  if (!token) return <SignIn devMode={devMode} onToken={(t) => { api.signIn(t); setToken(api.token); }} />;
  const open = items.filter((i) => i.status === 'open' && !i.archivedAt).length;
  const current = apps.find((a) => a.manifest.name === view);
  return (
    <div className="shell">
      <header>
        <span className="brand">IDP Workspace</span>
        <select aria-label="workspace" value={ws?.name ?? ''} onChange={(e) => setWs(workspaces.find((w) => w.name === e.target.value) ?? null)}>
          {workspaces.map((w) => <option key={w.workspaceId} value={w.name}>{w.displayName}</option>)}
        </select>
        <button className="secondary" onClick={() => { api.signOut(); setToken(null); }}>退出</button>
      </header>
      <nav>
        <button className={view === 'inbox' ? 'active' : ''} onClick={() => setView('inbox')}>收件箱{open > 0 && <span className="badge">{open}</span>}</button>
        {me?.roles.includes('ws-admin') && <button className={view === '@admin' ? 'active' : ''} onClick={() => setView('@admin')}>管理</button>}
        {apps.filter((a) => a.manifest.entry).map((a) => (
          <button key={a.manifest.name} className={view === a.manifest.name ? 'active' : ''} onClick={() => setView(a.manifest.name)}>{a.manifest.title}</button>
        ))}
      </nav>
      <main>
        {error && <p className="error" role="alert">{error}</p>}
        {!ws && <p className="muted">你还不属于任何工作空间。</p>}
        {ws && view === 'inbox' && <Inbox api={api} ws={ws} items={items} apps={apps} confirm={ui.confirm} notify={notify} onChanged={(it) => setItems((cur) => cur.map((x) => x.itemId === it.itemId ? it : x))} />}
        {view === '@admin' && me?.roles.includes('ws-admin') && <Admin api={api} workspaces={workspaces} current={ws} notify={notify} onWorkspaces={() => api.workspaces().then(setWorkspaces).catch(() => {})} />}
        {ws && current && <AppFrame api={api} ws={ws} app={current} ui={ui} contextVersion={0} />}
      </main>
      <div className="toasts" aria-live="polite">{toasts.map((t) => <div key={t.id} className={`toast ${t.level}`}>{t.message}</div>)}</div>
    </div>
  );
}

function SignIn({ devMode, onToken }: { devMode: boolean; onToken(token: string): void }) {
  const [value, setValue] = useState('');
  return (
    <div className="signin">
      <h1>IDP Workspace</h1>
      {devMode ? (
        <form onSubmit={(e) => { e.preventDefault(); if (value.trim()) onToken(value); }}>
          <label htmlFor="token">开发令牌（idp-workspace dev-token TENANT SUBJECT ws-member）</label>
          <textarea id="token" rows={4} value={value} onChange={(e) => setValue(e.target.value)} />
          <button type="submit">登录</button>
        </form>
      ) : (
        <p className="muted">单点登录尚未配置（ADR-0001 M3：OIDC 授权码 + PKCE）。</p>
      )}
    </div>
  );
}
