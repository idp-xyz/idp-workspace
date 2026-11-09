import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { Inbox } from '../src/Inbox.tsx';
import type { Api } from '../src/api.ts';
import type { InboxItem, InstalledApp } from '../src/types.ts';

const item: InboxItem = {
  itemId: 'i1', workspaceId: 'w1', producer: 'svc-eer', externalRef: 'execution:7f3a/approval', kind: 'approval', title: 'Approve the refund for order 1001',
  summary: 'Refund of 420.00 USD', ref: { system: 'eer', kind: 'execution', id: 'execution:7f3a' }, priority: 'high',
  actions: [{ app: 'orders', action: 'approval.approve', label: 'Approve' }, { app: 'gone', action: 'x', label: 'Gone' }], entities: [{ type: 'order', id: '1001' }],
  status: 'open', version: 1, createdAt: '2026-11-04T09:00:00Z', updatedAt: '2026-11-04T09:00:00Z',
};
const apps: InstalledApp[] = [{ installation: { installationId: 'x', app: 'orders', version: '1.2.0' }, digest: 'd',
  manifest: { name: 'orders', version: '1.2.0', title: 'Orders', publisher: 'p', capabilities: [], actions: [{ name: 'approval.approve', title: 'Approve', input: { type: 'object' }, target: { kind: 'eer.decision', decision: 'approve' }, confirm: true }] } }];

describe('Inbox', () => {
  it('forwards an item action after confirmation and leaves the item to its producer', async () => {
    const invoke = vi.fn(async () => ({ invocationId: 'inv-12345678', app: 'orders', action: 'approval.approve', status: 'succeeded' as const }));
    const mark = vi.fn(async () => ({ ...item, readAt: '2026-11-04T10:00:00Z' }));
    const notify = vi.fn();
    const confirm = vi.fn(async () => true);
    const onChanged = vi.fn();
    render(<Inbox api={{ invoke, mark } as unknown as Api} ws={{ workspaceId: 'w1', name: 'us-ops', displayName: 'US' }} items={[item]} apps={apps} confirm={confirm} notify={notify} onChanged={onChanged} />);
    expect(screen.getByText('Approve the refund for order 1001')).toBeTruthy();
    expect(screen.getByText('order:1001')).toBeTruthy();
    fireEvent.click(screen.getByText('Approve'));
    await waitFor(() => expect(invoke).toHaveBeenCalledWith('us-ops', { app: 'orders', action: 'approval.approve', itemId: 'i1', input: {} }));
    expect(confirm).toHaveBeenCalled();
    expect(notify).toHaveBeenCalledWith(expect.stringContaining('已提交'), 'success');
    expect(mark).not.toHaveBeenCalled(); // a button click is not a read mark
    fireEvent.click(screen.getByText('Gone'));
    expect(notify).toHaveBeenCalledWith(expect.stringContaining('不可用'), 'error');
    fireEvent.click(screen.getByText('Refund of 420.00 USD'));
    await waitFor(() => expect(mark).toHaveBeenCalledWith('us-ops', 'i1', { read: true }));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it('hides closed and archived items', () => {
    render(<Inbox api={{} as Api} ws={{ workspaceId: 'w1', name: 'us-ops', displayName: 'US' }} items={[{ ...item, status: 'resolved' }, { ...item, itemId: 'i2', archivedAt: 'x' }]} apps={apps} confirm={async () => true} notify={() => {}} onChanged={() => {}} />);
    expect(screen.getByText('收件箱是空的。')).toBeTruthy();
  });

  it('keeps the closed and the archived under their tabs, filters by kind and words, and opens a run\'s conversation', () => {
    const run: InboxItem = { ...item, itemId: 'r1', kind: 'run', title: 'Ask the order assistant — finished', summary: 'Parcels **above 10000 USD** need approval.', actions: [],
      ref: { system: 'host', kind: 'run', id: 'ws_42' }, producer: 'idp-workspace', externalRef: 'run:ws_42' };
    const onOpenRun = vi.fn();
    render(<Inbox api={{} as Api} ws={{ workspaceId: 'w1', name: 'us-ops', displayName: 'US' }} items={[item, run, { ...item, itemId: 'i3', status: 'resolved', resolution: { outcome: 'approved', by: 'bob', at: 'x' } }, { ...item, itemId: 'i4', archivedAt: 'x', title: 'Old one' }]}
      apps={apps} confirm={async () => true} notify={() => {}} onChanged={() => {}} onOpenRun={onOpenRun} />);
    // the run's title carries its state as a badge, its summary is Markdown
    expect(screen.getByText('Ask the order assistant')).toBeTruthy();
    expect(screen.getByText('已完成')).toBeTruthy();
    expect(screen.getByText('above 10000 USD').tagName).toBe('STRONG');
    fireEvent.click(screen.getByText('在对话中打开'));
    expect(onOpenRun).toHaveBeenCalledWith('ws_42');
    expect(screen.queryByText('Old one')).toBeNull();
    fireEvent.click(screen.getByRole('tab', { name: /已归档/ }));
    expect(screen.getByText('Old one')).toBeTruthy();
    expect(screen.getByText('取消归档')).toBeTruthy();
    fireEvent.click(screen.getByRole('tab', { name: /全部/ }));
    expect(screen.getByText(/已处理：approved/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText('种类'), { target: { value: 'run' } });
    expect(screen.queryByText('Approve the refund for order 1001')).toBeNull();
    expect(screen.getByText('Ask the order assistant')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('搜索'), { target: { value: 'nothing like this' } });
    expect(screen.getByText('没有符合条件的事项。')).toBeTruthy();
  });
});
