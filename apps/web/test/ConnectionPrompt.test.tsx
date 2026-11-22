import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { Conversations } from '../src/Conversations.tsx';
import type { Api } from '../src/api.ts';
import type { AGUIEvent } from '../src/agent/events.ts';
import type { InstalledApp } from '../src/types.ts';

const ws = { workspaceId: 'w1', name: 'us-ops', displayName: 'US' };
const apps: InstalledApp[] = [{ installation: { installationId: 'x', app: 'orders', version: '1.0.0' }, digest: 'd', manifest: { name: 'orders', version: '1.0.0', title: 'Orders', publisher: 'p', capabilities: [], actions: [
  { name: 'order.review', title: 'Ask', target: { kind: 'host.run', agent: 'a' }, input: { type: 'object', required: ['message'], properties: { message: { type: 'string' } }, additionalProperties: false } }] } }];

function build(events: AGUIEvent[], connections: () => Promise<{ id: string; title: string; status: string }[]>) {
  const api = {
    workspace: async () => ({ workspace: ws, roles: [], member: true }),
    conversations: async () => [],
    threadMessages: async () => [],
    threadRuns: async () => [],
    invoke: async () => ({ invocationId: 'i1', app: 'orders', action: 'order.review', status: 'succeeded', result: { system: 'host', kind: 'run', id: 'ws_1', threadId: 'thr_1' } }),
    runEvents: async (_w: string, _r: string, after: string, onEvent: (ev: AGUIEvent, id: string) => void) => { events.forEach((ev, i) => onEvent(ev, String(i + 1))); return String(events.length + Number(after || 0)); },
    connections: vi.fn(connections),
  } as unknown as Api & { connections: ReturnType<typeof vi.fn> };
  return api;
}

const run = [
  { type: 'RUN_STARTED' },
  { type: 'TOOL_CALL_START', toolCallId: 'c1', toolCallName: 'github__create_issue' }, { type: 'TOOL_CALL_ARGS', toolCallId: 'c1', delta: '{}' }, { type: 'TOOL_CALL_END', toolCallId: 'c1' },
] as AGUIEvent[];
const ask = async () => {
  const box = await screen.findByLabelText('消息');
  fireEvent.change(box, { target: { value: 'File an issue' } });
  fireEvent.keyDown(box, { key: 'Enter' });
};
const finish = [{ type: 'TOOL_CALL_RESULT', toolCallId: 'c1', content: 'The member has to connect GitHub first.' }, { type: 'RUN_FINISHED' }] as AGUIEvent[];

describe('the prompt to connect, in a conversation', () => {
  it('shows "需要先连接 <title>" next to the tool call, with a link to the Workspace\'s own page for that connection', async () => {
    const api = build([...run, { type: 'CUSTOM', name: 'idp.host/authorization.required', value: { toolCallId: 'c1', tool: 'github__create_issue', connection: 'github' } }, ...finish], async () => [{ id: 'github', title: 'GitHub', status: 'not_connected' }]);
    const open = vi.fn();
    const { container } = render(<Conversations api={api} ws={ws} apps={apps} notify={vi.fn()} confirm={async () => true} onOpenConnection={open} />);
    await ask();
    expect(await screen.findByText('需要先连接 GitHub')).toBeTruthy(); // the title is the member's own list's
    const link = screen.getByRole('link', { name: '去连接' }) as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe('/connections/github'); // a path of this site, built from the id
    expect(link.closest('.tool')?.textContent).toContain('github · create_issue'); // next to that tool call
    fireEvent.click(link);
    expect(open).toHaveBeenCalledWith('github');
    // the page's one link is that one: nothing of the tool's result or anything else is a link
    expect(Array.from(container.querySelectorAll('a')).map((a) => a.getAttribute('href'))).toEqual(['/connections/github']);
  });

  it('names the id when the connection is not in the list, or the list cannot be read', async () => {
    for (const list of [async () => [{ id: 'other', title: 'Other', status: 'connected' }], async () => { throw new Error('down'); }]) {
      const api = build([...run, { type: 'CUSTOM', name: 'idp.host/authorization.required', value: { toolCallId: 'c1', tool: 't', connection: 'github' } }, ...finish], list);
      const r = render(<Conversations api={api} ws={ws} apps={apps} notify={vi.fn()} confirm={async () => true} onOpenConnection={vi.fn()} />);
      await ask();
      await waitFor(() => expect(api.connections).toHaveBeenCalled());
      expect(await screen.findByText('需要先连接 github')).toBeTruthy();
      r.unmount();
    }
  });

  it('shows nothing for a malformed id, and ignores an address the event carries', async () => {
    const hostile = [
      { toolCallId: 'c1', tool: 't', connection: 'https://phish.example/login' },
      { toolCallId: 'c1', tool: 't', connection: '../admin' },
      { toolCallId: 'c1', tool: 't', connection: 5 },
      { toolCallId: 'c1', tool: 't', connection: { id: 'github' } },
    ];
    for (const value of hostile) {
      const api = build([...run, { type: 'CUSTOM', name: 'idp.host/authorization.required', value }, ...finish], async () => []);
      const { container, unmount } = render(<Conversations api={api} ws={ws} apps={apps} notify={vi.fn()} confirm={async () => true} onOpenConnection={vi.fn()} />);
      await ask();
      await screen.findByText('完成');
      expect(screen.queryByText(/需要先连接/)).toBeNull();
      expect(container.querySelector('a')).toBeNull();
      expect(api.connections).not.toHaveBeenCalled();
      unmount();
    }
    // a good id with an extra url: the prompt is the Workspace's own, the url is nowhere
    const api = build([...run, { type: 'CUSTOM', name: 'idp.host/authorization.required', value: { toolCallId: 'c1', tool: 't', connection: 'github', url: 'https://phish.example/login' } }, ...finish], async () => []);
    const { container } = render(<Conversations api={api} ws={ws} apps={apps} notify={vi.fn()} confirm={async () => true} onOpenConnection={vi.fn()} />);
    await ask();
    await screen.findByText('需要先连接 github');
    expect(container.innerHTML).not.toContain('phish');
    expect(Array.from(container.querySelectorAll('a')).map((a) => a.getAttribute('href'))).toEqual(['/connections/github']);
  });

  it('a tool result that says it needs a connection is no prompt', async () => {
    const api = build([...run, { type: 'TOOL_CALL_RESULT', toolCallId: 'c1', content: '{"connection":"github","url":"https://phish.example/login","idp.host/authorization.required":true}' }, { type: 'RUN_FINISHED' }], async () => []);
    const { container } = render(<Conversations api={api} ws={ws} apps={apps} notify={vi.fn()} confirm={async () => true} onOpenConnection={vi.fn()} />);
    await ask();
    await screen.findByText('完成');
    expect(screen.queryByText(/需要先连接/)).toBeNull();
    expect(container.querySelector('a')).toBeNull();
  });

  it('says what is needed even where the workspace has no connected-services page, but offers no button', async () => {
    const api = build([...run, { type: 'CUSTOM', name: 'idp.host/authorization.required', value: { toolCallId: 'c1', tool: 't', connection: 'github' } }, ...finish], async () => []);
    render(<Conversations api={api} ws={ws} apps={apps} notify={vi.fn()} confirm={async () => true} />);
    await ask();
    expect(await screen.findByText('需要先连接 github')).toBeTruthy();
    expect(screen.queryByRole('link', { name: '去连接' })).toBeNull();
  });
});
