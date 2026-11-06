import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { Launcher, buildInput, fieldsOf, launchable } from '../src/Launcher.tsx';
import type { Api } from '../src/api.ts';
import type { InstalledApp } from '../src/types.ts';

const ws = { workspaceId: 'w1', name: 'us-ops', displayName: 'US' };
const orders: InstalledApp = { installation: { installationId: 'x', app: 'orders', version: '1.0.0' }, digest: 'd',
  manifest: { name: 'orders', version: '1.0.0', title: 'Orders', publisher: 'p', capabilities: [], actions: [
    { name: 'order.review', title: 'Ask the order assistant', target: { kind: 'host.run', agent: 'order-assistant' },
      input: { type: 'object', required: ['message'], properties: { message: { type: 'string' } }, additionalProperties: false } },
    { name: 'approval.approve', title: 'Approve', target: { kind: 'eer.decision', decision: 'approve' }, input: { type: 'object' } },
    { name: 'order.refund', title: 'Refund', target: { kind: 'webhook', url: 'https://orders.example/r' }, roles: ['finance'], input: { type: 'object' } },
  ] } };

describe('fieldsOf / buildInput', () => {
  it('turns an input schema into fields, and the form values back into input', () => {
    const fields = fieldsOf({ type: 'object', required: ['message', 'count'], properties: {
      message: { type: 'string' }, orderId: { type: 'string', title: 'Order' }, count: { type: 'integer' },
      urgent: { type: 'boolean' }, tier: { type: 'string', enum: ['T1', 'T2'] }, extra: { type: 'object' } } });
    expect(fields.map((f) => [f.name, f.kind, f.required])).toEqual([
      ['message', 'textarea', true], ['orderId', 'text', false], ['count', 'integer', true],
      ['urgent', 'boolean', false], ['tier', 'select', false], ['extra', 'json', false]]);
    expect(fields[1].label).toBe('Order');
    expect(buildInput(fields, { message: 'hi', count: '3', urgent: true, tier: 'T2', extra: '{"a":1}' })).toEqual({ message: 'hi', count: 3, urgent: true, tier: 'T2', extra: { a: 1 } });
    expect(buildInput(fields, { message: 'hi', count: '0', orderId: '  ' })).toEqual({ message: 'hi', count: 0 }); // an empty optional field is left out
    expect(() => buildInput(fields, { count: '3' })).toThrow(/message 必填/);
    expect(() => buildInput(fields, { message: 'x', count: '1.5' })).toThrow(/整数/);
    expect(() => buildInput(fields, { message: 'x', count: '1', extra: '{' })).toThrow(/JSON/);
  });

  it('leaves item-bound decisions on their items and filters by the contract roles', () => {
    expect(launchable(orders).map((a) => a.name)).toEqual(['order.review', 'order.refund']); // roles not known: the server decides
    expect(launchable(orders, ['approver']).map((a) => a.name)).toEqual(['order.review']);
    expect(launchable(orders, ['finance']).map((a) => a.name)).toEqual(['order.review', 'order.refund']);
  });
});

describe('Launcher', () => {
  it('offers what the member may start and forwards the form as the input', async () => {
    const invoke = vi.fn(async () => ({ invocationId: 'inv-1', app: 'orders', action: 'order.review', status: 'succeeded' as const, result: { id: 'run-1' } }));
    const workspace = vi.fn(async () => ({ workspace: ws, roles: ['approver'], member: true }));
    const notify = vi.fn();
    render(<Launcher api={{ invoke, workspace } as unknown as Api} ws={ws} apps={[orders]} confirm={async () => true} notify={notify} />);
    expect(await screen.findByText('Ask the order assistant')).toBeTruthy();
    expect(screen.queryByText('Approve')).toBeNull();
    expect(screen.queryByText('Refund')).toBeNull();
    fireEvent.click(screen.getByText('发起'));
    expect(notify).toHaveBeenCalledWith(expect.stringContaining('必填'), 'error');
    expect(invoke).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText('message'), { target: { value: 'What is the pickup policy?' } });
    fireEvent.click(screen.getByText('发起'));
    await waitFor(() => expect(invoke).toHaveBeenCalledWith('us-ops', { app: 'orders', action: 'order.review', input: { message: 'What is the pickup policy?' } }));
    await waitFor(() => expect(notify).toHaveBeenCalledWith(expect.stringContaining('收件箱'), 'success'));
    expect((screen.getByLabelText('message') as HTMLTextAreaElement).value).toBe(''); // ready for the next one
  });

  it('says so when no installed app has an action to start', async () => {
    const workspace = vi.fn(async () => ({ workspace: ws, roles: [], member: true }));
    const onlyDecisions = { ...orders, manifest: { ...orders.manifest, actions: [orders.manifest.actions[1]] } };
    render(<Launcher api={{ workspace } as unknown as Api} ws={ws} apps={[onlyDecisions]} confirm={async () => true} notify={() => {}} />);
    expect(await screen.findByText('已安装的应用里没有可以直接发起的动作。')).toBeTruthy();
  });
});
