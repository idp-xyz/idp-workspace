import { describe, expect, it } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
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
      if (url.endsWith('/inbox')) return json({ items: [{ itemId: 'i1', workspaceId: 'w1', producer: 'p', externalRef: 'r', kind: 'task', title: 'Check the Ohio lane',
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
    expect(count('/v1/workspaces/us-ops/inbox')).toBeLessThanOrEqual(2);
  });
});
