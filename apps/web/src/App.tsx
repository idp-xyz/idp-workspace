import { useCallback, useEffect, useMemo, useState } from 'react';
import { Api, ApiError } from './api.ts';
import { OIDC } from './oidc.ts';
import type { Tokens } from './oidc.ts';
import { Admin } from './Admin.tsx';
import { AppFrame } from './AppFrame.tsx';
import { Inbox } from './Inbox.tsx';
import { Launcher, launchable } from './Launcher.tsx';
import type { ShellUI } from './bridgeHandlers.ts';
import type { InboxItem, InstalledApp, Me, Workspace } from './types.ts';

interface Toast { id: number; message: string; level: string }

export function App(props: { api?: Api }) {
  // one client for the life of the shell: a new one per render would re-run every effect below
  const [api] = useState(() => props.api ?? new Api());
  const [token, setToken] = useState(api.token);
  const [devMode, setDevMode] = useState(false);
  const [oidc, setOidc] = useState<OIDC | null>(null);
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

  // sign-in with the identity provider (ADR-0001 D14): finish a redirect on /callback, then keep the
  // access token fresh with the refresh token until it no longer works
  const keep = useCallback((t: Tokens) => {
    api.signIn(t.accessToken);
    sessionStorage.setItem('idp-workspace.session', JSON.stringify({ refreshToken: t.refreshToken, idToken: t.idToken, expiresAt: t.expiresAt }));
    setToken(api.token);
  }, [api]);
  useEffect(() => {
    api.shellConfig().then(async (c) => {
      setDevMode(c.devMode);
      if (!c.oidc) return;
      const o = new OIDC(c.oidc, window.location.origin);
      setOidc(o);
      if (window.location.pathname === '/callback') {
        try { keep(await o.callback(new URLSearchParams(window.location.search))); } catch (e) { setError(String(e)); }
        window.history.replaceState(null, '', '/');
      }
    }).catch(() => {});
  }, [api, keep]);
  useEffect(() => {
    if (!oidc || !token) return;
    const s = JSON.parse(sessionStorage.getItem('idp-workspace.session') ?? '{}') as { refreshToken?: string; expiresAt?: number };
    if (!s.refreshToken || !s.expiresAt) return;
    const timer = setTimeout(() => {
      oidc.refresh(s.refreshToken!).then(keep).catch(() => { api.signOut(); setToken(null); });
    }, Math.max(5_000, s.expiresAt - Date.now() - 60_000));
    return () => clearTimeout(timer);
  }, [oidc, token, api, keep]);
  const signOut = async () => {
    const s = JSON.parse(sessionStorage.getItem('idp-workspace.session') ?? '{}') as { idToken?: string };
    api.signOut();
    sessionStorage.removeItem('idp-workspace.session');
    setToken(null);
    const to = oidc && s.idToken ? await oidc.logoutUrl(s.idToken).catch(() => null) : null;
    if (to) window.location.assign(to);
  };
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

  if (!token) return <SignIn devMode={devMode} oidc={oidc} error={error} onToken={(t) => { api.signIn(t); setToken(api.token); }} />;
  const open = items.filter((i) => i.status === 'open' && !i.archivedAt).length;
  const current = apps.find((a) => a.manifest.name === view);
  return (
    <div className="shell">
      <header>
        <span className="brand">IDP Workspace</span>
        <select aria-label="workspace" value={ws?.name ?? ''} onChange={(e) => setWs(workspaces.find((w) => w.name === e.target.value) ?? null)}>
          {workspaces.map((w) => <option key={w.workspaceId} value={w.name}>{w.displayName}</option>)}
        </select>
        {me && <span className="who">{me.subject}</span>}
        <button className="secondary" onClick={signOut}>退出</button>
      </header>
      <nav>
        <button className={view === 'inbox' ? 'active' : ''} onClick={() => setView('inbox')}>收件箱{open > 0 && <span className="badge">{open}</span>}</button>
        {apps.some((a) => launchable(a).length > 0) && <button className={view === '@launch' ? 'active' : ''} onClick={() => setView('@launch')}>发起</button>}
        {me?.roles.includes('ws-admin') && <button className={view === '@admin' ? 'active' : ''} onClick={() => setView('@admin')}>管理</button>}
        {apps.filter((a) => a.manifest.entry).map((a) => (
          <button key={a.manifest.name} className={view === a.manifest.name ? 'active' : ''} onClick={() => setView(a.manifest.name)}>{a.manifest.title}</button>
        ))}
      </nav>
      <main>
        {error && <p className="error" role="alert">{error}</p>}
        {!ws && <p className="muted">你还不属于任何工作空间。</p>}
        {ws && view === 'inbox' && <Inbox api={api} ws={ws} items={items} apps={apps} confirm={ui.confirm} notify={notify} onChanged={(it) => setItems((cur) => cur.map((x) => x.itemId === it.itemId ? it : x))} />}
        {ws && view === '@launch' && <Launcher api={api} ws={ws} apps={apps} confirm={ui.confirm} notify={notify} />}
        {view === '@admin' && me?.roles.includes('ws-admin') && <Admin api={api} workspaces={workspaces} current={ws} notify={notify} onWorkspaces={() => api.workspaces().then(setWorkspaces).catch(() => {})} />}
        {ws && current && <AppFrame api={api} ws={ws} app={current} ui={ui} contextVersion={0} />}
      </main>
      <div className="toasts" aria-live="polite">{toasts.map((t) => <div key={t.id} className={`toast ${t.level}`}>{t.message}</div>)}</div>
    </div>
  );
}

function SignIn({ devMode, oidc, error, onToken }: { devMode: boolean; oidc: OIDC | null; error: string | null; onToken(token: string): void }) {
  const [value, setValue] = useState('');
  return (
    <div className="signin">
      <h1>IDP Workspace</h1>
      {error && <p className="error" role="alert">{error}</p>}
      {oidc && <button className="primary-wide" onClick={async () => window.location.assign(await oidc.authorizeUrl())}>使用企业账号登录</button>}
      {devMode ? (
        <form onSubmit={(e) => { e.preventDefault(); if (value.trim()) onToken(value); }}>
          <label htmlFor="token">开发令牌（idp-workspace dev-token TENANT SUBJECT ws-member）</label>
          <textarea id="token" rows={4} value={value} onChange={(e) => setValue(e.target.value)} />
          <button type="submit">登录</button>
        </form>
      ) : !oidc && (
        <p className="muted">没有配置登录方式（IDP_WORKSPACE_OIDC_CLIENT_ID）。</p>
      )}
    </div>
  );
}
