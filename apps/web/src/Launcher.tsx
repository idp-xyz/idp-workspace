import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { ApiError } from './api.ts';
import type { Api } from './api.ts';
import type { ActionContract, InstalledApp, Workspace } from './types.ts';

// Starting an action that no inbox item carries (ADR-0001 D19): the shell renders a form from the
// action's input schema and forwards the call; the workspace checks membership, roles and the schema
// again. An eer.decision decides an open item, so it stays on that item.

export interface Field {
  name: string;
  label: string;
  hint?: string;
  kind: 'text' | 'textarea' | 'number' | 'integer' | 'boolean' | 'select' | 'json';
  required: boolean;
  options?: string[];
}

const LONG_TEXT = /message|prompt|question|text|content|code|body|note|description/i;

/** the actions of an app a member can start here; roles null = not known yet, the server decides */
export function launchable(app: InstalledApp, roles: string[] | null = null): ActionContract[] {
  return app.manifest.actions.filter((a) => a.target.kind !== 'eer.decision' && (roles === null || !a.roles?.length || a.roles.some((r) => roles.includes(r))));
}

/** the form of an object schema: one field per property; what a form cannot express is typed as JSON */
export function fieldsOf(schema: Record<string, unknown>): Field[] {
  const props = (schema.properties ?? {}) as Record<string, Record<string, unknown>>;
  const required = new Set(Array.isArray(schema.required) ? (schema.required as string[]) : []);
  return Object.entries(props).map(([name, p]): Field => {
    const base = { name, label: typeof p.title === 'string' ? p.title : name, hint: typeof p.description === 'string' ? p.description : undefined, required: required.has(name) };
    if (Array.isArray(p.enum) && p.enum.every((v) => typeof v === 'string')) return { ...base, kind: 'select', options: p.enum as string[] };
    switch (p.type) {
      case 'string': return { ...base, kind: LONG_TEXT.test(name) || Number(p.maxLength ?? 0) > 200 ? 'textarea' : 'text' };
      case 'number': return { ...base, kind: 'number' };
      case 'integer': return { ...base, kind: 'integer' };
      case 'boolean': return { ...base, kind: 'boolean' };
      default: return { ...base, kind: 'json' };
    }
  });
}

/** the action's input from the form's values; throws a message for the member when a value is wrong */
export function buildInput(fields: Field[], values: Record<string, string | boolean>): Record<string, unknown> {
  const input: Record<string, unknown> = {};
  for (const f of fields) {
    const v = values[f.name];
    if (f.kind === 'boolean') {
      if (v !== undefined || f.required) input[f.name] = Boolean(v);
      continue;
    }
    const text = typeof v === 'string' ? v : '';
    if (text.trim() === '') {
      if (f.required) throw new Error(`${f.label} 必填`);
      continue;
    }
    if (f.kind === 'number' || f.kind === 'integer') {
      const n = Number(text);
      if (!Number.isFinite(n) || (f.kind === 'integer' && !Number.isInteger(n))) throw new Error(`${f.label} 不是${f.kind === 'integer' ? '整数' : '数字'}`);
      input[f.name] = n;
    } else if (f.kind === 'json') {
      try { input[f.name] = JSON.parse(text); } catch { throw new Error(`${f.label} 不是合法的 JSON`); }
    } else {
      input[f.name] = text;
    }
  }
  return input;
}

export interface LauncherProps {
  api: Api;
  ws: Workspace;
  apps: InstalledApp[];
  confirm(question: string): Promise<boolean>;
  notify(message: string, level: string): void;
}

export function Launcher({ api, ws, apps, confirm, notify }: LauncherProps) {
  // the member's roles in this workspace; until they are known nothing is offered
  const [roles, setRoles] = useState<string[] | null | undefined>(undefined);
  useEffect(() => {
    let live = true;
    setRoles(undefined);
    api.workspace(ws.name).then((w) => { if (live) setRoles(w.roles); }).catch(() => { if (live) setRoles(null); });
    return () => { live = false; };
  }, [api, ws]);
  if (roles === undefined) return <p className="muted">正在读取你在本工作空间的角色…</p>;
  const offered = apps.flatMap((app) => launchable(app, roles).map((contract) => ({ app, contract })));
  if (offered.length === 0) return <p className="muted">已安装的应用里没有可以直接发起的动作。</p>;
  return (
    <div className="launcher">
      <p className="muted">发起后，结果会回到收件箱。</p>
      {offered.map(({ app, contract }) => (
        <ActionForm key={`${app.manifest.name}/${contract.name}`} api={api} ws={ws} app={app} contract={contract} confirm={confirm} notify={notify} />
      ))}
    </div>
  );
}

function ActionForm({ api, ws, app, contract, confirm, notify }: { api: Api; ws: Workspace; app: InstalledApp; contract: ActionContract; confirm(q: string): Promise<boolean>; notify(m: string, l: string): void }) {
  const fields = fieldsOf(contract.input);
  const [values, setValues] = useState<Record<string, string | boolean>>({});
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    let input: Record<string, unknown>;
    try {
      input = buildInput(fields, values);
    } catch (err) {
      notify(`${contract.title}：${(err as Error).message}`, 'error');
      return;
    }
    if (contract.confirm && !(await confirm(`${app.manifest.title}：${contract.title}？`))) return;
    setBusy(true);
    try {
      const inv = await api.invoke(ws.name, { app: app.manifest.name, action: contract.name, input });
      if (inv.status === 'failed') {
        notify(`${contract.title} 失败：${inv.error?.code ?? ''} ${inv.error?.message ?? ''}`, 'error');
      } else {
        notify(`${contract.title}：已发起，结果会出现在收件箱`, 'success');
        setValues({});
      }
    } catch (err) {
      notify(err instanceof ApiError ? `${contract.title} 失败：${err.code} ${err.message}` : String(err), 'error');
    } finally {
      setBusy(false);
    }
  };
  return (
    <form className="launch" aria-label={`${app.manifest.title}：${contract.title}`} onSubmit={submit}>
      <div className="item-head"><span className="kind">{app.manifest.title}</span><strong>{contract.title}</strong></div>
      {contract.description && <p className="muted">{contract.description}</p>}
      {fields.map((f) => <FieldInput key={f.name} field={f} value={values[f.name]} onChange={(v) => setValues((cur) => ({ ...cur, [f.name]: v }))} />)}
      <div className="actions"><button type="submit" disabled={busy}>发起</button></div>
    </form>
  );
}

function FieldInput({ field, value, onChange }: { field: Field; value: string | boolean | undefined; onChange(v: string | boolean): void }) {
  const text = typeof value === 'string' ? value : '';
  let control;
  switch (field.kind) {
    case 'textarea':
    case 'json':
      control = <textarea aria-label={field.label} rows={field.kind === 'json' ? 3 : 4} placeholder={field.kind === 'json' ? 'JSON' : undefined} value={text} onChange={(e) => onChange(e.target.value)} />;
      break;
    case 'boolean':
      control = <input type="checkbox" aria-label={field.label} checked={value === true} onChange={(e) => onChange(e.target.checked)} />;
      break;
    case 'select':
      control = (
        <select aria-label={field.label} value={text} onChange={(e) => onChange(e.target.value)}>
          <option value="">—</option>
          {field.options!.map((o) => <option key={o} value={o}>{o}</option>)}
        </select>
      );
      break;
    default:
      control = <input type={field.kind === 'text' ? 'text' : 'number'} step={field.kind === 'integer' ? 1 : 'any'} aria-label={field.label} value={text} onChange={(e) => onChange(e.target.value)} />;
  }
  return (
    <label className="field">
      <span>{field.label}{field.required ? ' *' : ''}</span>
      {control}
      {field.hint && <small className="muted">{field.hint}</small>}
    </label>
  );
}
