import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { MemoryView } from '../src/Memory.tsx';
import { App } from '../src/App.tsx';
import { Api, ApiError } from '../src/api.ts';
import type { MemoryItem } from '../src/types.ts';

const m1: MemoryItem = { id: 'a', content: 'Bob prefers email', createdAt: '2026-11-05T10:00:00Z', validUntil: '2026-12-05T10:00:00Z', source: 'idp-host', ref: 'run:ws_1', tags: [] };
const m2: MemoryItem = { id: 'b', content: 'x'.repeat(400), createdAt: '2026-11-04T10:00:00Z', tags: [] };

function fake(list: MemoryItem[][]) {
  const memory = vi.fn(async () => list.shift() ?? []);
  const deleteMemory = vi.fn(async () => ({ revoked: 1 }));
  const deleteAllMemory = vi.fn(async () => ({ revoked: 2 }));
  return { api: { memory, deleteMemory, deleteAllMemory } as unknown as Api, memory, deleteMemory, deleteAllMemory };
}

describe('MemoryView', () => {
  it('lists memories, cuts long content with show more, and states the privacy line without a number', async () => {
    const f = fake([[m1, m2]]);
    render(<MemoryView api={f.api} confirm={async () => true} notify={() => {}} />);
    await waitFor(() => expect(screen.getByText('Bob prefers email')).toBeTruthy());
    expect(screen.getByText(/这些是助手从你的对话里记下的内容/).textContent).not.toMatch(/\d/);
    expect(screen.getByText('x'.repeat(300) + '…')).toBeTruthy();
    expect(screen.getByText(/来自运行 run:ws_1/)).toBeTruthy();
    fireEvent.click(screen.getByText('显示更多'));
    expect(screen.getByText('x'.repeat(400))).toBeTruthy();
  });

  it('deletes one and refreshes the list', async () => {
    const f = fake([[m1, m2], [m2]]);
    const notify = vi.fn();
    render(<MemoryView api={f.api} confirm={async () => true} notify={notify} />);
    await waitFor(() => screen.getByText('Bob prefers email'));
    fireEvent.click(screen.getAllByText('删除')[0]);
    await waitFor(() => expect(f.deleteMemory).toHaveBeenCalledWith('a'));
    await waitFor(() => expect(screen.queryByText('Bob prefers email')).toBeNull());
    expect(f.memory).toHaveBeenCalledTimes(2);
    expect(notify).toHaveBeenCalledWith('已删除', 'success');
  });

  it('deletes all only after the confirm dialog says yes', async () => {
    const f = fake([[m1], []]);
    const confirm = vi.fn().mockResolvedValueOnce(false).mockResolvedValueOnce(true);
    render(<MemoryView api={f.api} confirm={confirm} notify={() => {}} />);
    await waitFor(() => screen.getByText('Bob prefers email'));
    fireEvent.click(screen.getByText('删除全部'));
    await waitFor(() => expect(confirm).toHaveBeenCalledTimes(1));
    expect(f.deleteAllMemory).not.toHaveBeenCalled();
    fireEvent.click(screen.getByText('删除全部'));
    await waitFor(() => expect(f.deleteAllMemory).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText('还没有记忆。和助手聊几句，它会记住。')).toBeTruthy());
  });

  it('shows the empty state', async () => {
    render(<MemoryView api={fake([[]]).api} confirm={async () => true} notify={() => {}} />);
    await waitFor(() => expect(screen.getByText('还没有记忆。和助手聊几句，它会记住。')).toBeTruthy());
    expect(screen.queryByText('删除全部')).toBeNull();
  });

  it('keeps a failed delete visible as an error and reloads', async () => {
    const f = fake([[m1], [m1]]);
    f.deleteMemory.mockRejectedValueOnce(new ApiError(404, 'NOT_FOUND', 'no such memory'));
    const notify = vi.fn();
    render(<MemoryView api={f.api} confirm={async () => true} notify={notify} />);
    await waitFor(() => screen.getByText('Bob prefers email'));
    fireEvent.click(screen.getByText('删除'));
    await waitFor(() => expect(notify).toHaveBeenCalledWith(expect.stringContaining('NOT_FOUND'), 'error'));
  });
});

describe('App memory tab', () => {
  const shell = (memory: Response) => {
    const fetcher = (async (url: string) => {
      const json = (v: unknown, status = 200) => new Response(JSON.stringify(v), { status });
      if (url === '/shell-config.json') return json({ devMode: true, version: 't' });
      if (url === '/v1/me') return json({ subject: 'bob', tenant: 't', roles: ['ws-member'] });
      if (url === '/v1/me/memory') return memory.clone();
      if (url === '/v1/workspaces') return json({ workspaces: [{ workspaceId: 'w1', name: 'us-ops', displayName: 'US' }] });
      if (url.endsWith('/apps')) return json({ apps: [] });
      if (url.endsWith('/inbox')) return json({ items: [] });
      return new Promise<Response>(() => {});
    }) as unknown as typeof fetch;
    const store = new Map<string, string>([['idp-workspace.token', 'tok']]);
    return new Api(fetcher, { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => void store.set(k, v), removeItem: (k) => void store.delete(k) });
  };

  it('shows the 记忆 tab when the endpoint answers', async () => {
    render(<App api={shell(new Response(JSON.stringify({ memories: [] }), { status: 200 }))} />);
    await waitFor(() => expect(screen.getByText('记忆')).toBeTruthy());
    fireEvent.click(screen.getByText('记忆'));
    await waitFor(() => expect(screen.getByText('还没有记忆。和助手聊几句，它会记住。')).toBeTruthy());
  });

  it('hides the tab when memory is not configured', async () => {
    render(<App api={shell(new Response(JSON.stringify({ error: { code: 'NOT_CONFIGURED', message: 'x' } }), { status: 404 }))} />);
    await waitFor(() => expect(screen.getByText('收件箱是空的。')).toBeTruthy());
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByText('记忆')).toBeNull();
  });
});
