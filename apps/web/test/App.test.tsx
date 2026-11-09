import { describe, expect, it } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { App } from '../src/App.tsx';
import { Api } from '../src/api.ts';

describe('App', () => {
  it('loads each resource once (no request loop) and lists the inbox', async () => {
    const urls: string[] = [];
    const fetcher = (async (url: string) => {
      urls.push(url);
      const json = (v: unknown) => new Response(JSON.stringify(v), { status: 200 });
      if (url === '/shell-config.json') return json({ devMode: true, version: 't' });
      if (url === '/v1/workspaces') return json({ workspaces: [{ workspaceId: 'w1', name: 'us-ops', displayName: 'US operations' }] });
      if (url.endsWith('/apps')) return json({ apps: [] });
      if (/\/inbox(\?|$)/.test(url)) return json({ items: [{ itemId: 'i1', workspaceId: 'w1', producer: 'p', externalRef: 'r', kind: 'task', title: 'Check the Ohio lane',
        ref: { system: 'external', kind: 't', id: '1' }, priority: 'normal', actions: [], entities: [], status: 'open', version: 1, createdAt: '', updatedAt: '' }] });
      return new Promise<Response>(() => {}); // the stream stays open
    }) as unknown as typeof fetch;
    const store = new Map<string, string>([['idp-workspace.token', 'tok']]);
    const api = new Api(fetcher, { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => void store.set(k, v), removeItem: (k) => void store.delete(k) });
    render(<App api={api} />);
    await waitFor(() => expect(screen.getByText('Check the Ohio lane')).toBeTruthy());
    await new Promise((r) => setTimeout(r, 100));
    const count = (u: string) => urls.filter((x) => x === u).length;
    expect(count('/v1/workspaces')).toBeLessThanOrEqual(2); // StrictMode may run an effect twice
    expect(count('/v1/workspaces/us-ops/inbox?status=all&archived=true&limit=300')).toBeLessThanOrEqual(2);
  });
});

describe('App sign-in', () => {
  it('drops the error of an earlier token once a valid one is used', async () => {
    let good = false;
    const fetcher = (async (url: string, init?: RequestInit) => {
      const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
      if (url === '/shell-config.json') return json({ devMode: true, version: 't' });
      const bearer = (init?.headers as Record<string, string> | undefined)?.Authorization;
      if (bearer !== 'Bearer good') return json({ error: { code: 'UNAUTHENTICATED', message: 'invalid token' } }, 401);
      good = true;
      if (url === '/v1/workspaces') return json({ workspaces: [{ workspaceId: 'w1', name: 'us-ops', displayName: 'US operations' }] });
      if (url.endsWith('/apps')) return json({ apps: [] });
      if (/\/inbox(\?|$)/.test(url)) return json({ items: [] });
      return new Promise<Response>(() => {});
    }) as unknown as typeof fetch;
    const store = new Map<string, string>([['idp-workspace.token', 'bad']]);
    const api = new Api(fetcher, { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => void store.set(k, v), removeItem: (k) => void store.delete(k) });
    render(<App api={api} />);
    await waitFor(() => expect(screen.getByLabelText(/令牌/)).toBeTruthy()); // the bad token was rejected: back to sign-in
    fireEvent.change(screen.getByLabelText(/令牌/), { target: { value: 'good' } });
    fireEvent.click(screen.getByText('登录'));
    await waitFor(() => expect(good).toBe(true));
    await waitFor(() => expect(screen.getByText('收件箱是空的。')).toBeTruthy());
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

describe('App navigation by role', () => {
  const build = (roles: string[]) => {
    const fetcher = (async (url: string) => {
      const json = (v: unknown) => new Response(JSON.stringify(v), { status: 200 });
      if (url === '/shell-config.json') return json({ devMode: true, version: 't' });
      if (url === '/v1/me') return json({ subject: 's', tenant: 't', roles });
      if (url === '/v1/workspaces') return json({ workspaces: [{ workspaceId: 'w1', name: 'us-ops', displayName: 'US operations' }] });
      if (url.endsWith('/apps')) return json({ apps: [] });
      if (/\/inbox(\?|$)/.test(url)) return json({ items: [] });
      return new Promise<Response>(() => {});
    }) as unknown as typeof fetch;
    const store = new Map<string, string>([['idp-workspace.token', 'tok']]);
    return new Api(fetcher, { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => void store.set(k, v), removeItem: (k) => void store.delete(k) });
  };
  it('offers the admin screen to administrators only', async () => {
    const admin = render(<App api={build(['ws-admin'])} />);
    expect(await admin.findByText('管理')).toBeTruthy();
    admin.unmount();
    const member = render(<App api={build(['ws-member'])} />);
    await member.findByText('收件箱是空的。');
    expect(member.queryByText('管理')).toBeNull();
  });
});
