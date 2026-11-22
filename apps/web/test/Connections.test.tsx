import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { ConnectionsView } from '../src/Connections.tsx';
import { App } from '../src/App.tsx';
import { Api, ApiError } from '../src/api.ts';
import type { Connection } from '../src/types.ts';

const github: Connection = { id: 'github', title: 'GitHub', status: 'not_connected' };
const notion: Connection = { id: 'notion', title: 'Notion', status: 'connected', scope: 'read_content update_content' };
const linear: Connection = { id: 'linear', title: 'Linear', status: 'reauthorization_required', reason: 'insufficient_scope' };
const docs: Connection = { id: 'docs', title: 'Public docs', status: 'not_required' };

function fake(lists: Connection[][]) {
  const connections = vi.fn(async () => lists.shift() ?? []);
  const connect = vi.fn(async (_id: string, _o?: { reviewer?: boolean; returnPath?: string }) => 'https://github.example/authorize?state=abc');
  const disconnect = vi.fn(async (_id: string, _reviewer?: boolean) => null);
  return { api: { connections, connect, disconnect } as unknown as Api, connections, connect, disconnect };
}
const common = () => ({ confirm: async () => true, notify: vi.fn() });

describe('ConnectionsView', () => {
  it('shows every state in words, with the action that fits it', async () => {
    const f = fake([[github, notion, linear, docs]]);
    render(<ConnectionsView api={f.api} {...common()} />);
    await screen.findByText('GitHub');
    const row = (title: string) => within(screen.getByText(title).closest('li') as HTMLElement);
    expect(row('GitHub').getByText('未连接')).toBeTruthy();
    expect(row('GitHub').getByRole('button', { name: '连接' })).toBeTruthy();
    expect(row('GitHub').queryByRole('button', { name: '断开' })).toBeNull();
    expect(row('Notion').getByText('已连接')).toBeTruthy();
    expect(row('Notion').getByText(/read_content update_content/)).toBeTruthy();
    expect(row('Notion').getByRole('button', { name: '断开' })).toBeTruthy();
    expect(row('Notion').queryByRole('button', { name: '连接' })).toBeNull();
    expect(row('Linear').getByText('需要重新授权')).toBeTruthy();
    expect(row('Linear').getByText('对方要求更多权限，需要重新授权')).toBeTruthy(); // the reason, in plain words
    expect(row('Linear').getByRole('button', { name: '重新授权' })).toBeTruthy();
    expect(row('Linear').getByRole('button', { name: '断开' })).toBeTruthy();
    expect(row('Public docs').getByText('无需连接')).toBeTruthy();
    expect(row('Public docs').queryAllByRole('button')).toHaveLength(0);
    expect(screen.queryByText(/评审账号/)).toBeNull(); // the bridge reported no reviewer account
  });

  it('says so when there is nothing to connect, and when the list cannot be read', async () => {
    const empty = render(<ConnectionsView api={fake([[]]).api} {...common()} />);
    await screen.findByText(/还没有可连接的服务/);
    empty.unmount();
    const api = { connections: vi.fn(async () => { throw new ApiError(502, 'CONNECTIONS_UNAVAILABLE', 'connected services are not available'); }) } as unknown as Api;
    render(<ConnectionsView api={api} {...common()} />);
    expect((await screen.findByRole('alert')).textContent).toBe('读取已连接的服务失败：连接服务暂时不可用，请稍后再试');
  });

  it('connects: the server records the start, the browser goes to the address it returns, and the way back is this connection\'s own page', async () => {
    const f = fake([[github]]);
    const navigate = vi.fn();
    render(<ConnectionsView api={f.api} {...common()} navigate={navigate} />);
    fireEvent.click(await screen.findByRole('button', { name: '连接' }));
    await waitFor(() => expect(navigate).toHaveBeenCalledWith('https://github.example/authorize?state=abc'));
    expect(f.connect).toHaveBeenCalledWith('github', { reviewer: false, returnPath: '/connections/github' });
  });

  it('does not follow an address that is not one, and tells why in fixed words', async () => {
    const f = fake([[github]]);
    f.connect.mockResolvedValueOnce('javascript:alert(1)');
    const c = common();
    const navigate = vi.fn();
    render(<ConnectionsView api={f.api} {...c} navigate={navigate} />);
    fireEvent.click(await screen.findByRole('button', { name: '连接' }));
    await waitFor(() => expect(c.notify).toHaveBeenCalledWith('连接失败：没有得到可用的授权地址', 'error'));
    expect(navigate).not.toHaveBeenCalled();
    // the server refused: the reason picks a fixed sentence, the message is not shown
    f.connect.mockRejectedValueOnce(new ApiError(409, 'CONNECTION_REFUSED', 'issuer_mismatch'));
    fireEvent.click(screen.getByRole('button', { name: '连接' }));
    await waitFor(() => expect(c.notify).toHaveBeenCalledWith('连接失败：对方的授权服务器与运维配置的不一致，需要运维处理', 'error'));
    expect(screen.getByRole('button', { name: '连接' }).hasAttribute('disabled')).toBe(false); // can try again
  });

  it('disconnects only after the member confirms, and then reads the list again', async () => {
    const f = fake([[notion], [{ ...notion, status: 'not_connected', scope: undefined }]]);
    const c = { confirm: vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true), notify: vi.fn() };
    render(<ConnectionsView api={f.api} {...c} />);
    fireEvent.click(await screen.findByRole('button', { name: '断开' }));
    await waitFor(() => expect(c.confirm).toHaveBeenCalledTimes(1));
    expect(c.confirm.mock.calls[0][0]).toContain('Notion');
    expect(f.disconnect).not.toHaveBeenCalled(); // "no" does nothing
    fireEvent.click(screen.getByRole('button', { name: '断开' }));
    await waitFor(() => expect(f.disconnect).toHaveBeenCalledWith('notion', false));
    await waitFor(() => expect(screen.getByText('未连接')).toBeTruthy());
    expect(f.connections).toHaveBeenCalledTimes(2);
    expect(c.notify).toHaveBeenCalledWith('已断开 Notion', 'success');
  });

  it('shows the reviewer account only when the bridge reports it, and treats it as its own account', async () => {
    const withReviewer: Connection = { ...github, reviewer: { status: 'not_connected' } };
    const connected: Connection = { ...notion, reviewer: { status: 'reauthorization_required', reason: 'expired' } };
    const f = fake([[withReviewer, connected, linear]]);
    const c = { confirm: vi.fn().mockResolvedValue(true), notify: vi.fn() };
    const navigate = vi.fn();
    render(<ConnectionsView api={f.api} {...c} navigate={navigate} />);
    await screen.findByText('GitHub');
    expect(screen.getAllByText(/评审账号（运维）/)).toHaveLength(2); // GitHub and Notion; Linear has none
    const row = (title: string) => within(screen.getByText(title).closest('li') as HTMLElement);
    expect(row('Linear').queryByText(/评审账号/)).toBeNull();
    fireEvent.click(row('GitHub').getByRole('button', { name: '连接评审账号' }));
    await waitFor(() => expect(f.connect).toHaveBeenCalledWith('github', { reviewer: true, returnPath: '/connections/github' }));
    expect(navigate).toHaveBeenCalled();
    expect(row('Notion').getByText('授权已过期，需要重新授权')).toBeTruthy();
  });

  it('disconnects the reviewer account, not the member\'s own, after a confirmation that names it', async () => {
    const connected: Connection = { ...notion, reviewer: { status: 'reauthorization_required', reason: 'expired' } };
    const f = fake([[connected], [{ ...connected, reviewer: { status: 'not_connected' } }]]);
    const c = { confirm: vi.fn().mockResolvedValue(true), notify: vi.fn() };
    render(<ConnectionsView api={f.api} {...c} />);
    await screen.findByText('Notion');
    const row = (title: string) => within(screen.getByText(title).closest('li') as HTMLElement);
    fireEvent.click(row('Notion').getByRole('button', { name: '断开评审账号' }));
    await waitFor(() => expect(c.confirm).toHaveBeenCalled());
    expect(c.confirm.mock.calls[0][0]).toMatch(/评审账号/);
    await waitFor(() => expect(f.disconnect).toHaveBeenCalledWith('notion', true)); // the reviewer's grant, not the member's
    await waitFor(() => expect(row('Notion').getByRole('button', { name: '连接评审账号' })).toBeTruthy()); // the list was read again
    expect(row('Notion').getByText('已连接')).toBeTruthy(); // the member's own account is untouched
  });

  it('puts the connection the member was pointed at first, marked, and says when it is not there', async () => {
    const f = fake([[github, notion, linear]]);
    const r = render(<ConnectionsView api={f.api} {...common()} focus="linear" />);
    await screen.findByText('GitHub');
    const items = r.container.querySelectorAll('.connection-list > li');
    expect(items[0].textContent).toContain('Linear');
    expect(items[0].getAttribute('aria-current')).toBe('true');
    expect(items[1].getAttribute('aria-current')).toBeNull();
    r.unmount();
    render(<ConnectionsView api={fake([[github]]).api} {...common()} focus="gone" />);
    expect((await screen.findByRole('status')).textContent).toContain('gone');
  });

  it('shows a title and an id as text, never as markup', async () => {
    const hostile: Connection = { id: 'x', title: '<img src=x onerror=alert(1)><b>bold</b>', status: 'not_connected' };
    const r = render(<ConnectionsView api={fake([[hostile]]).api} {...common()} />);
    await screen.findByText(/onerror/);
    expect(r.container.querySelector('img')).toBeNull();
    expect(r.container.querySelector('b')).toBeNull();
  });
});

describe('App: the connected-services page', () => {
  const shell = (connections: boolean) => {
    const fetcher = (async (url: string) => {
      const json = (v: unknown) => new Response(JSON.stringify(v), { status: 200 });
      if (url === '/shell-config.json') return json({ devMode: true, version: 't' });
      if (url === '/v1/me') return json({ subject: 'bob', tenant: 't', roles: ['ws-member'], connections });
      if (url === '/v1/me/connections') return json({ connections: [{ id: 'github', title: 'GitHub', status: 'not_connected' }, { id: 'notion', title: 'Notion', status: 'connected' }] });
      if (url === '/v1/workspaces') return json({ workspaces: [{ workspaceId: 'w1', name: 'us-ops', displayName: 'US' }] });
      if (url.endsWith('/apps')) return json({ apps: [] });
      if (/\/inbox(\?|$)/.test(url)) return json({ items: [] });
      return new Promise<Response>(() => {});
    }) as unknown as typeof fetch;
    const store = new Map<string, string>([['idp-workspace.token', 'tok']]);
    return new Api(fetcher, { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => void store.set(k, v), removeItem: (k) => void store.delete(k) });
  };
  it('has its page in the navigation only where the feature is on', async () => {
    const off = render(<App api={shell(false)} />);
    await off.findByText('收件箱是空的。');
    expect(off.queryByText('已连接的服务')).toBeNull();
    off.unmount();
    const on = render(<App api={shell(true)} />);
    fireEvent.click(await on.findByRole('button', { name: '已连接的服务' }));
    expect(await on.findByText('GitHub')).toBeTruthy();
    expect(window.location.pathname).toBe('/connections');
    fireEvent.click(on.getByRole('button', { name: '收件箱' }));
    expect(window.location.pathname).toBe('/');
  });
  it('opens at the connection a path points at, first and marked', async () => {
    window.history.pushState(null, '', '/connections/notion');
    try {
      const r = render(<App api={shell(true)} />);
      await r.findByText('GitHub');
      const items = r.container.querySelectorAll('.connection-list > li');
      expect(items[0].textContent).toContain('Notion');
      expect(items[0].getAttribute('aria-current')).toBe('true');
    } finally {
      window.history.replaceState(null, '', '/');
    }
  });
  it('shows the inbox for a connections path where the workspace has no such page', async () => {
    window.history.pushState(null, '', '/connections');
    try {
      const r = render(<App api={shell(false)} />);
      await r.findByText('收件箱是空的。');
      expect(r.queryByText('已连接的服务')).toBeNull();
      expect(window.location.pathname).toBe('/');
    } finally {
      window.history.replaceState(null, '', '/');
    }
  });
});
