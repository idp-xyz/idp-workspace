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
});
