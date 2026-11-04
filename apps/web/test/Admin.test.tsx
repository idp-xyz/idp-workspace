import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { Admin } from '../src/Admin.tsx';
import { ApiError } from '../src/api.ts';
import type { Api } from '../src/api.ts';
import type { Release, Workspace } from '../src/types.ts';

const ws: Workspace = { workspaceId: 'w1', name: 'us-ops', displayName: 'US operations' };
const release: Release = { name: 'orders', version: '1.0.0', digest: 'sha256:x', status: 'published', publishedBy: 'alice', publishedAt: '2026-11-04T09:00:00Z',
  manifest: { name: 'orders', version: '1.0.0', title: '订单台', publisher: 'IDP', capabilities: [], actions: [{ name: 'order.review', title: 'Ask', input: { type: 'object' }, target: { kind: 'host.run', agent: 'a' } }] } };

function fakeApi(over: Partial<Record<keyof Api, unknown>> = {}) {
  return {
    members: vi.fn(async () => [{ subject: 'alice', roles: ['owner'], addedBy: 'alice', addedAt: '2026-11-04T09:00:00Z' }]),
    releases: vi.fn(async () => [release]),
    installations: vi.fn(async () => []),
    audit: vi.fn(async () => [{ id: 1, at: '2026-11-04T09:00:00Z', subject: 'alice', action: 'workspace.create', resourceId: 'w1', detail: null }]),
    createWorkspace: vi.fn(async () => ws),
    putMember: vi.fn(async () => ({})),
    removeMember: vi.fn(async () => null),
    publish: vi.fn(async () => release),
    install: vi.fn(async () => ({})),
    uninstall: vi.fn(async () => null),
    revoke: vi.fn(async () => release),
    workspaces: vi.fn(async () => [ws]),
    ...over,
  } as unknown as Api;
}

const props = (api: Api, notify = vi.fn(), onWorkspaces = vi.fn()) => ({ api, workspaces: [ws], current: ws, notify, onWorkspaces });

describe('Admin', () => {
  it('creates a workspace and adds a member with workspace roles', async () => {
    const api = fakeApi();
    const notify = vi.fn();
    const onWorkspaces = vi.fn();
    render(<Admin {...props(api, notify, onWorkspaces)} />);
    await screen.findByText('alice');
    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'eu-ops' } });
    fireEvent.change(screen.getByLabelText('显示名'), { target: { value: 'EU operations' } });
    fireEvent.click(screen.getByText('创建'));
    await waitFor(() => expect(api.createWorkspace).toHaveBeenCalledWith('eu-ops', 'EU operations'));
    await waitFor(() => expect(onWorkspaces).toHaveBeenCalled());
    fireEvent.change(screen.getByLabelText('成员'), { target: { value: 'bob' } });
    fireEvent.change(screen.getByLabelText('角色'), { target: { value: 'approver, reviewer' } });
    fireEvent.click(screen.getByText('添加或更新'));
    await waitFor(() => expect(api.putMember).toHaveBeenCalledWith('us-ops', 'bob', ['approver', 'reviewer']));
  });

  it('shows the server\'s reason when a call is refused', async () => {
    const api = fakeApi({ createWorkspace: vi.fn(async () => { throw new ApiError(409, 'CONFLICT', 'a workspace named us-ops exists'); }) });
    const notify = vi.fn();
    render(<Admin {...props(api, notify)} />);
    await screen.findByText('alice');
    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'us-ops' } });
    fireEvent.change(screen.getByLabelText('显示名'), { target: { value: 'x' } });
    fireEvent.click(screen.getByText('创建'));
    await waitFor(() => expect(notify).toHaveBeenCalledWith('CONFLICT: a workspace named us-ops exists', 'error'));
  });

  it('publishes a manifest, installs a version with config, and refuses invalid JSON before calling the API', async () => {
    const api = fakeApi();
    const notify = vi.fn();
    render(<Admin {...props(api, notify)} />);
    fireEvent.click(screen.getByRole('tab', { name: '应用' }));
    await screen.findByText('已发布的版本');
    fireEvent.change(screen.getByLabelText('应用清单'), { target: { value: '{not json' } });
    fireEvent.click(screen.getByText('发布'));
    expect(notify).toHaveBeenCalledWith('清单不是合法的 JSON', 'error');
    expect(api.publish).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText('应用清单'), { target: { value: '{"name":"orders"}' } });
    fireEvent.click(screen.getByText('发布'));
    await waitFor(() => expect(api.publish).toHaveBeenCalledWith({ name: 'orders' }));
    fireEvent.change(screen.getByLabelText('安装哪个版本'), { target: { value: 'orders@1.0.0' } });
    fireEvent.change(screen.getByLabelText('配置'), { target: { value: '{"region":"us"}' } });
    fireEvent.click(screen.getByText('安装 / 升级'));
    await waitFor(() => expect(api.install).toHaveBeenCalledWith('us-ops', 'orders', '1.0.0', { region: 'us' }, undefined));
  });

  it('upgrading an installed app sends its revision as the precondition', async () => {
    const api = fakeApi({ installations: vi.fn(async () => [{ installationId: 'i1', app: 'orders', version: '0.9.0', config: {}, revision: 4, releaseStatus: 'published' }]) });
    render(<Admin {...props(api)} />);
    fireEvent.click(screen.getByRole('tab', { name: '应用' }));
    await screen.findByText('（第 4 版配置）', { exact: false });
    fireEvent.change(screen.getByLabelText('安装哪个版本'), { target: { value: 'orders@1.0.0' } });
    fireEvent.click(screen.getByText('安装 / 升级'));
    await waitFor(() => expect(api.install).toHaveBeenCalledWith('us-ops', 'orders', '1.0.0', {}, 4));
  });

  it('lists the audit', async () => {
    render(<Admin {...props(fakeApi())} />);
    fireEvent.click(screen.getByRole('tab', { name: '审计' }));
    expect(await screen.findByText('workspace.create')).toBeTruthy();
  });
});
