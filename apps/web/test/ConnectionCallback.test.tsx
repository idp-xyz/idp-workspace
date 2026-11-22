import { StrictMode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import { ConnectionCallback } from '../src/ConnectionCallback.tsx';
import { App } from '../src/App.tsx';
import { Api, ApiError } from '../src/api.ts';
import type { ConnectionResult } from '../src/types.ts';

const params = { code: 'a/b+c=d é', state: 'S T', iss: 'https://as.example/realms/x', error: '' };

function fake(result: ConnectionResult | ApiError) {
  const finishConnection = vi.fn(async (_p: unknown) => { if (result instanceof ApiError) throw result; return result; });
  return { api: { finishConnection } as unknown as Api, finishConnection };
}

describe('ConnectionCallback', () => {
  beforeEach(() => { vi.useFakeTimers({ shouldAdvanceTime: true }); sessionStorage.clear(); });
  afterEach(() => vi.useRealTimers());

  it('hands the redirect over exactly as received, once (also under StrictMode), says it is connected and goes back to the recorded path', async () => {
    const f = fake({ connection: 'github', status: 'connected', returnPath: '/connections/github' });
    const go = vi.fn();
    render(<StrictMode><ConnectionCallback api={f.api} params={params} titleOf={async () => 'GitHub'} go={go} /></StrictMode>);
    expect(await screen.findByText('已连接：GitHub')).toBeTruthy();
    expect(f.finishConnection).toHaveBeenCalledTimes(1);
    expect(f.finishConnection).toHaveBeenCalledWith({ code: 'a/b+c=d é', state: 'S T', iss: 'https://as.example/realms/x', error: '' });
    expect(go).not.toHaveBeenCalled();
    await act(async () => { vi.advanceTimersByTime(1600); });
    expect(go).toHaveBeenCalledWith('/connections/github');
  });

  it('passes an error answer on as it is, for the server to judge', async () => {
    const f = fake(new ApiError(400, 'CONNECTION_AUTHORIZATION_DENIED', 'access_denied'));
    render(<ConnectionCallback api={f.api} params={{ code: '', state: 'S', iss: '', error: 'access_denied' }} go={vi.fn()} />);
    expect((await screen.findByRole('alert')).textContent).toBe('连接没有完成：你在对方的页面上拒绝了授权');
    expect(f.finishConnection).toHaveBeenCalledWith({ code: '', state: 'S', iss: '', error: 'access_denied' });
  });

  it('shows a failure in fixed words only and offers the way back; the path is the page\'s own', async () => {
    const hostile = '<script>alert(1)</script> https://phish.example/';
    const f = fake(new ApiError(400, 'CONNECTION_AUTHORIZATION_FAILED', hostile));
    const go = vi.fn();
    const r = render(<ConnectionCallback api={f.api} params={params} go={go} />);
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).not.toMatch(/script|phish/);
    expect(r.container.querySelector('script')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '回到已连接的服务' }));
    expect(go).toHaveBeenCalledWith('/connections');
  });

  it('never goes to a return path that is not a path of this site', async () => {
    for (const bad of ['https://evil.example/', '//evil.example', '/\\evil.example', 'javascript:alert(1)']) {
      const f = fake({ connection: 'github', status: 'connected', returnPath: bad });
      const go = vi.fn();
      const r = render(<ConnectionCallback api={f.api} params={params} go={go} />);
      await screen.findByText('已连接：github');
      fireEvent.click(screen.getByRole('button', { name: '现在回去' }));
      await act(async () => { vi.advanceTimersByTime(2000); });
      expect(go.mock.calls.map((c) => c[0]), bad).toEqual(['/connections', '/connections']);
      r.unmount();
    }
  });
});

// the callback as the browser meets it: the address, the member's sign-in, the session's storage
describe('App at the OAuth callback', () => {
  const build = (token: string | null) => {
    const finish = vi.fn(async (_body: unknown) => ({ connection: 'github', status: 'connected', returnPath: '/connections' }));
    const fetcher = (async (url: string, init?: RequestInit) => {
      const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
      if (url === '/shell-config.json') return json({ devMode: true, version: 't' });
      if (url === '/v1/me') return json({ subject: 'bob', tenant: 't', roles: ['ws-member'], connections: true });
      if (url === '/v1/me/connections/callback') return json(await finish(JSON.parse(String(init?.body))));
      if (url === '/v1/me/connections') return json({ connections: [{ id: 'github', title: 'GitHub', status: 'connected' }] });
      if (url === '/v1/workspaces') return json({ workspaces: [{ workspaceId: 'w1', name: 'us-ops', displayName: 'US' }] });
      if (url.endsWith('/apps')) return json({ apps: [] });
      if (/\/inbox(\?|$)/.test(url)) return json({ items: [] });
      return new Promise<Response>(() => {});
    }) as unknown as typeof fetch;
    const store = new Map<string, string>(token ? [['idp-workspace.token', token]] : []);
    return { finish, api: new Api(fetcher, { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => void store.set(k, v), removeItem: (k) => void store.delete(k) }) };
  };
  beforeEach(() => { sessionStorage.clear(); });
  afterEach(() => { window.history.replaceState(null, '', '/'); sessionStorage.clear(); });

  it('signed in: finishes the connect once, with what the redirect carried, and takes the code out of the address', async () => {
    window.history.pushState(null, '', '/oauth/callback?code=CODE%2F1&state=STATE-1&iss=https%3A%2F%2Fas.example');
    const { api, finish } = build('tok');
    render(<StrictMode><App api={api} /></StrictMode>);
    expect(await screen.findByText('已连接：GitHub')).toBeTruthy();
    await new Promise((r) => setTimeout(r, 50));
    expect(finish).toHaveBeenCalledTimes(1);
    expect(finish).toHaveBeenCalledWith({ code: 'CODE/1', state: 'STATE-1', iss: 'https://as.example', error: '' });
    expect(window.location.search).toBe(''); // the address bar and the history do not keep the code
    expect(window.location.pathname).toBe('/oauth/callback');
    expect(sessionStorage.length).toBe(0);
  });

  it('not signed in: asks for the sign-in first, keeps the redirect for it and finishes once after it', async () => {
    window.history.pushState(null, '', '/oauth/callback?code=C2&state=STATE-2');
    const { api, finish } = build(null);
    render(<App api={api} />);
    await waitFor(() => expect(screen.getByLabelText(/令牌/)).toBeTruthy());
    expect(finish).not.toHaveBeenCalled(); // nobody is signed in: nothing is handed over
    fireEvent.change(screen.getByLabelText(/令牌/), { target: { value: 'tok' } });
    fireEvent.click(screen.getByText('登录'));
    expect(await screen.findByText('已连接：GitHub')).toBeTruthy();
    await new Promise((r) => setTimeout(r, 50));
    expect(finish).toHaveBeenCalledTimes(1);
    expect(finish).toHaveBeenCalledWith({ code: 'C2', state: 'STATE-2', iss: '', error: '' });
  });

  it('after the sign-in\'s own redirect (the address is gone) the kept redirect is used, and then it is gone', async () => {
    sessionStorage.setItem('idp-workspace.oauth-callback', JSON.stringify({ at: Date.now(), p: { code: 'C3', state: 'STATE-3', iss: '', error: '' } }));
    const { api, finish } = build('tok');
    const r = render(<App api={api} />);
    await screen.findByText('已连接：GitHub');
    expect(finish).toHaveBeenCalledWith({ code: 'C3', state: 'STATE-3', iss: '', error: '' });
    expect(sessionStorage.getItem('idp-workspace.oauth-callback')).toBeNull();
    r.unmount();
    render(<App api={api} />); // a reload does not do it again
    await new Promise((res) => setTimeout(res, 50));
    expect(finish).toHaveBeenCalledTimes(1);
  });
});
