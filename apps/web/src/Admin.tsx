import { useCallback, useEffect, useState } from 'react';
import { ApiError } from './api.ts';
import type { Api } from './api.ts';
import type { AuditEvent, InstallationRow, Member, Release, Workspace } from './types.ts';

// The administrator's screens (ADR-0001 D13): workspaces and members, the app registry and installations,
// the audit. Everything here is a call to the /api/v1 administration API, which enforces ws-admin; hiding
// this screen from other roles is only a convenience.

const problem = (e: unknown) => (e instanceof ApiError ? `${e.code}: ${e.message}` : String(e));

export interface AdminProps {
  api: Api;
  workspaces: Workspace[];
  current: Workspace | null;
  notify(message: string, level: string): void;
  onWorkspaces(): void; // the list of workspaces changed
}

export function Admin(props: AdminProps) {
  const [tab, setTab] = useState<'members' | 'apps' | 'audit'>('members');
  return (
    <section className="admin">
      <div className="tabs" role="tablist">
        {([['members', '工作空间与成员'], ['apps', '应用'], ['audit', '审计']] as const).map(([id, label]) => (
          <button key={id} role="tab" aria-selected={tab === id} className={tab === id ? 'active' : 'secondary'} onClick={() => setTab(id)}>{label}</button>
        ))}
      </div>
      {tab === 'members' && <Members {...props} />}
      {tab === 'apps' && <Apps {...props} />}
      {tab === 'audit' && <Audit {...props} />}
    </section>
  );
}

function Members({ api, workspaces, current, notify, onWorkspaces }: AdminProps) {
  const [name, setName] = useState('');
  const [display, setDisplay] = useState('');
  const [members, setMembers] = useState<Member[]>([]);
  const [subject, setSubject] = useState('');
  const [roles, setRoles] = useState('');
  const load = useCallback(() => {
    if (current) api.members(current.name).then(setMembers).catch((e) => notify(problem(e), 'error'));
  }, [api, current, notify]);
  useEffect(load, [load]);
  const run = async (what: () => Promise<unknown>, done: string) => {
    try { await what(); notify(done, 'success'); } catch (e) { notify(problem(e), 'error'); }
  };
  return (
    <>
      <h2>新建工作空间</h2>
      <form className="row" onSubmit={(e) => { e.preventDefault(); run(async () => { await api.createWorkspace(name.trim(), display.trim()); setName(''); setDisplay(''); onWorkspaces(); }, '工作空间已创建'); }}>
        <input aria-label="名称" placeholder="名称（小写字母、数字、连字符）" value={name} onChange={(e) => setName(e.target.value)} />
        <input aria-label="显示名" placeholder="显示名" value={display} onChange={(e) => setDisplay(e.target.value)} />
        <button type="submit" disabled={!name.trim() || !display.trim()}>创建</button>
      </form>
      <p className="muted">现有 {workspaces.length} 个：{workspaces.map((w) => w.name).join('、') || '无'}</p>
      {current && (
        <>
          <h2>{current.displayName} 的成员</h2>
          <table>
            <thead><tr><th>成员</th><th>工作空间角色</th><th /></tr></thead>
            <tbody>
              {members.map((m) => (
                <tr key={m.subject}>
                  <td>{m.subject}</td>
                  <td>{m.roles.join('、') || '—'}</td>
                  <td><button className="secondary" onClick={() => run(async () => { await api.removeMember(current.name, m.subject); load(); }, `已移除 ${m.subject}`)}>移除</button></td>
                </tr>
              ))}
            </tbody>
          </table>
          <form className="row" onSubmit={(e) => { e.preventDefault(); run(async () => {
            await api.putMember(current.name, subject.trim(), roles.split(/[,，\s]+/).filter(Boolean)); setSubject(''); setRoles(''); load();
          }, '成员已保存'); }}>
            <input aria-label="成员" placeholder="成员（令牌里的 sub）" value={subject} onChange={(e) => setSubject(e.target.value)} />
            <input aria-label="角色" placeholder="角色，逗号分隔，如 approver" value={roles} onChange={(e) => setRoles(e.target.value)} />
            <button type="submit" disabled={!subject.trim()}>添加或更新</button>
          </form>
          <p className="muted">工作空间角色决定谁能看到指派给角色的事项、谁能调用限定角色的动作；它与令牌里的 ws-* 角色是两回事。</p>
        </>
      )}
    </>
  );
}

const SAMPLE = JSON.stringify({
  schemaVersion: 'idp.workspace.app/v1', name: 'orders', version: '1.0.0', title: '订单台', publisher: 'IDP',
  capabilities: [],
  actions: [{ name: 'order.review', title: '询问订单助手', target: { kind: 'host.run', agent: 'order-assistant' },
    input: { type: 'object', required: ['message'], properties: { message: { type: 'string' } }, additionalProperties: false } }],
}, null, 2);

function Apps({ api, current, notify }: AdminProps) {
  const [releases, setReleases] = useState<Release[]>([]);
  const [installed, setInstalled] = useState<InstallationRow[]>([]);
  const [text, setText] = useState(SAMPLE);
  const [pick, setPick] = useState('');
  const [config, setConfig] = useState('{}');
  const load = useCallback(() => {
    api.releases().then(setReleases).catch((e) => notify(problem(e), 'error'));
    if (current) api.installations(current.name).then(setInstalled).catch((e) => notify(problem(e), 'error'));
  }, [api, current, notify]);
  useEffect(load, [load]);
  const run = async (what: () => Promise<unknown>, done: string) => {
    try { await what(); notify(done, 'success'); load(); } catch (e) { notify(problem(e), 'error'); }
  };
  const published = releases.filter((r) => r.status === 'published');
  return (
    <>
      <h2>发布应用</h2>
      <p className="muted">粘贴应用清单（idp.workspace.app/v1）。发布后不可修改；同名同版本只能原样重发。</p>
      <textarea aria-label="应用清单" rows={12} value={text} onChange={(e) => setText(e.target.value)} className="mono" />
      <div className="row">
        <button onClick={() => {
          let manifest: unknown;
          try { manifest = JSON.parse(text); } catch { notify('清单不是合法的 JSON', 'error'); return; }
          run(() => api.publish(manifest), '已发布');
        }}>发布</button>
      </div>
      <h2>已发布的版本</h2>
      <table>
        <thead><tr><th>应用</th><th>版本</th><th>状态</th><th>动作</th><th /></tr></thead>
        <tbody>
          {releases.map((r) => (
            <tr key={`${r.name}@${r.version}`}>
              <td>{r.manifest.title} <span className="muted">({r.name})</span></td>
              <td>{r.version}</td>
              <td>{r.status === 'published' ? '已发布' : `已撤销：${r.revocation?.reason ?? ''}`}</td>
              <td>{r.manifest.actions.map((a) => a.name).join('、') || '—'}</td>
              <td>{r.status === 'published' && <button className="secondary" onClick={() => {
                const reason = window.prompt(`撤销 ${r.name} ${r.version} 的理由`);
                if (reason) run(() => api.revoke(r.name, r.version, reason), '已撤销');
              }}>撤销</button>}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {current && (
        <>
          <h2>{current.displayName} 中的安装</h2>
          <table>
            <thead><tr><th>应用</th><th>版本</th><th>状态</th><th /></tr></thead>
            <tbody>
              {installed.map((i) => (
                <tr key={i.installationId}>
                  <td>{i.app}</td><td>{i.version}（第 {i.revision} 版配置）</td>
                  <td>{i.releaseStatus === 'published' ? '可用' : '版本已撤销，不可用'}</td>
                  <td><button className="secondary" onClick={() => run(() => api.uninstall(current.name, i.app), '已卸载')}>卸载</button></td>
                </tr>
              ))}
              {installed.length === 0 && <tr><td colSpan={4} className="muted">还没有安装任何应用。</td></tr>}
            </tbody>
          </table>
          <div className="row">
            <select aria-label="安装哪个版本" value={pick} onChange={(e) => setPick(e.target.value)}>
              <option value="">选择要安装的版本…</option>
              {published.map((r) => <option key={`${r.name}@${r.version}`} value={`${r.name}@${r.version}`}>{r.name} {r.version}</option>)}
            </select>
            <input aria-label="配置" className="mono" value={config} onChange={(e) => setConfig(e.target.value)} placeholder='配置 JSON，如 {"region":"us"}' />
            <button disabled={!pick} onClick={() => {
              const [app, version] = pick.split('@');
              let cfg: Record<string, unknown>;
              try { cfg = JSON.parse(config || '{}'); } catch { notify('配置不是合法的 JSON', 'error'); return; }
              const existing = installed.find((i) => i.app === app);
              run(() => api.install(current.name, app, version, cfg, existing?.revision), existing ? '已更新安装' : '已安装');
            }}>安装 / 升级</button>
          </div>
        </>
      )}
    </>
  );
}

function Audit({ api, notify }: AdminProps) {
  const [events, setEvents] = useState<AuditEvent[]>([]);
  useEffect(() => { api.audit(100).then(setEvents).catch((e) => notify(problem(e), 'error')); }, [api, notify]);
  return (
    <>
      <h2>管理审计</h2>
      <table>
        <thead><tr><th>时间</th><th>操作者</th><th>动作</th><th>对象</th></tr></thead>
        <tbody>
          {events.map((ev) => (
            <tr key={ev.id}><td>{new Date(ev.at).toLocaleString()}</td><td>{ev.subject}</td><td>{ev.action}</td><td className="mono">{ev.resourceId ?? ''}</td></tr>
          ))}
        </tbody>
      </table>
    </>
  );
}
