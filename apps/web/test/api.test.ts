// @vitest-environment node
import { describe, expect, it } from 'vitest';
import { Api, ApiError } from '../src/api.ts';

function memoryStore() {
  const m = new Map<string, string>();
  return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v), removeItem: (k: string) => void m.delete(k) };
}

describe('Api', () => {
  it('sends the bearer token and an idempotency key, and maps problems', async () => {
    const calls: Array<{ url: string; init: RequestInit }> = [];
    const fetcher = (async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      if (url.endsWith('/actions/invoke')) return new Response(JSON.stringify({ error: { code: 'ITEM_CLOSED', message: 'the item is resolved' } }), { status: 409 });
      return new Response(JSON.stringify({ workspaces: [{ workspaceId: 'w1', name: 'us-ops', displayName: 'US' }] }), { status: 200 });
    }) as unknown as typeof fetch;
    const api = new Api(fetcher, memoryStore());
    api.signIn('  tok-1 \n');
    expect(await api.workspaces()).toHaveLength(1);
    expect((calls[0].init.headers as Record<string, string>).Authorization).toBe('Bearer tok-1');
    const err = await api.invoke('us-ops', { app: 'orders', action: 'approval.approve', itemId: 'i1' }, 'key-1').catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 409, code: 'ITEM_CLOSED' });
    expect((calls[1].init.headers as Record<string, string>)['Idempotency-Key']).toBe('key-1');
    expect(JSON.parse(String(calls[1].init.body))).toEqual({ app: 'orders', action: 'approval.approve', itemId: 'i1' });
    api.signOut();
    expect(api.token).toBeNull();
  });

  it('streams items and resumes after the last id', async () => {
    const urls: string[] = [];
    let n = 0;
    const abort = new AbortController();
    const fetcher = (async (url: string) => {
      urls.push(url);
      n++;
      const body = n === 1 ? 'id: 10:a\nevent: item\ndata: {"itemId":"a","version":1}\n\n' : 'id: 11:b\nevent: item\ndata: {"itemId":"b","version":1}\n\n';
      return new Response(new Blob([body]).stream(), { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
    }) as unknown as typeof fetch;
    const api = new Api(fetcher, memoryStore(), 1);
    const got: string[] = [];
    await api.stream('us-ops', (it) => { got.push(it.itemId); if (got.length === 2) abort.abort(); }, abort.signal);
    expect(got).toEqual(['a', 'b']);
    expect(urls[0]).toBe('/v1/workspaces/us-ops/inbox/stream');
    expect(urls[1]).toBe('/v1/workspaces/us-ops/inbox/stream?after=10%3Aa');
  });
});
