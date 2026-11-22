// Connected services (ADR-0001 D30), the shell's side: what a connection id looks like, which paths the shell goes back to, what
// the member reads for each state and each failure, and how the third party's redirect is kept across a sign-in. The words come
// from the fixed codes the server answers with; a message the server sent is never shown as it is.
import { ApiError } from './api.ts';
import { CONNECTION_ID } from './connectionId.ts';
import { friendly } from './errors.ts';
import type { ConnectionStatus } from './types.ts';

export { CONNECTION_ID };

export const CALLBACK_PATH = '/oauth/callback';
export const CONNECTIONS_PATH = '/connections';

/** the Workspace's own page for a connection: a path of this origin, built from a checked id */
export function connectionPath(id: string): string {
  return CONNECTION_ID.test(id) ? `${CONNECTIONS_PATH}/${id}` : CONNECTIONS_PATH;
}

/** the view a path names: the connected-services page (and the connection it points at), or null */
export function connectionsOfPath(pathname: string): { id?: string } | null {
  if (pathname === CONNECTIONS_PATH || pathname === CONNECTIONS_PATH + '/') return {};
  const m = /^\/connections\/([^/]+)\/?$/.exec(pathname);
  return m && CONNECTION_ID.test(m[1]) ? { id: m[1] } : null;
}

/** a relative path of this Workspace (the server's ValidReturnPath): one slash, no second slash or backslash, visible ASCII only, not the callback page */
export function isLocalPath(p: unknown): p is string {
  if (typeof p !== 'string' || p.length === 0 || p.length > 200 || p[0] !== '/') return false;
  if (p.length > 1 && (p[1] === '/' || p[1] === '\\')) return false;
  if (/[^\x21-\x7e]/.test(p) || p.includes('\\')) return false;
  try {
    const u = new URL(p, 'https://workspace.invalid');
    if (u.origin !== 'https://workspace.invalid') return false;
    const decoded = decodeURIComponent(u.pathname);
    if (decoded.startsWith('//') || decoded.includes('\\') || /[\x00-\x1f\x7f]/.test(decoded)) return false;
    return !u.pathname.startsWith(CALLBACK_PATH);
  } catch {
    return false;
  }
}

/** the authorization page the server returned: followed only if it is an address (the server has checked it is https) */
export function isAuthorizationUrl(u: unknown): u is string {
  if (typeof u !== 'string' || u.length > 4096 || /[^\x21-\x7e]/.test(u)) return false;
  try {
    const x = new URL(u);
    return (x.protocol === 'https:' || x.protocol === 'http:') && x.host !== '' && x.username === '' && x.password === '';
  } catch {
    return false;
  }
}

export const STATUS_WORDS: Record<ConnectionStatus, string> = {
  not_required: '无需连接',
  not_connected: '未连接',
  connected: '已连接',
  reauthorization_required: '需要重新授权',
};

const REASON_WORDS: Record<string, string> = {
  insufficient_scope: '对方要求更多权限，需要重新授权',
  issuer_changed: '对方的授权服务器已变更，需要重新授权',
  expired: '授权已过期，需要重新授权',
};

/** the reason a grant cannot be used, in words ("" for none or one the shell does not know) */
export function reasonWords(reason: string | undefined): string {
  return reason && Object.hasOwn(REASON_WORDS, reason) ? REASON_WORDS[reason] : '';
}

const FAILED: Record<string, string> = {
  iss_mismatch: '授权服务器的身份与配置的不一致，已拒绝',
  iss_missing: '授权服务器没有表明自己的身份，已拒绝',
  invalid_grant: '授权码无效或已经用过',
  token_response_invalid: '授权服务器交来的令牌无法使用',
  token_endpoint_error: '授权服务器没有给出令牌',
};
const DENIED: Record<string, string> = {
  access_denied: '你在对方的页面上拒绝了授权',
  invalid_scope: '对方不接受申请的权限范围',
  server_error: '对方的授权服务器出错了，请稍后再试',
  temporarily_unavailable: '对方的授权服务器暂时不可用，请稍后再试',
  invalid_request: '对方拒绝了这次授权请求',
  unauthorized_client: '对方不接受这个应用的授权请求',
  unsupported_response_type: '对方不接受这种授权方式',
};
const REFUSED: Record<string, string> = {
  issuer_mismatch: '对方的授权服务器与运维配置的不一致',
  issuer_not_listed: '对方的授权服务器不在运维配置的清单里',
  resource_mismatch: '对方服务的标识与运维配置的不一致',
  resource_metadata_origin: '对方服务的元数据来自不被信任的地址',
  pkce_unsupported: '对方的授权服务器不支持必需的安全措施（PKCE）',
  client_identity_unsupported: '对方的授权服务器不接受这个应用的身份',
  address_refused: '对方的地址不符合出口规则',
  redirect_refused: '对方不接受这个应用的回调地址',
  metadata_invalid: '对方的元数据无法使用',
};

/** what the member reads when a connected-services call failed; the server's message only picks among fixed sentences */
export function connectionError(e: unknown, what = ''): string {
  const prefix = what ? `${what}：` : '';
  if (!(e instanceof ApiError)) return friendly(e, what);
  const pick = (table: Record<string, string>, dflt: string) => (Object.hasOwn(table, e.message) ? table[e.message] : dflt);
  switch (e.code) {
    case 'CONNECTION_STATE_INVALID': return `${prefix}这次授权不是你发起的，或已经用过。请回到“已连接的服务”重新连接`;
    case 'CONNECTION_STATE_EXPIRED': return `${prefix}这次授权已经超时（10 分钟内要完成）。请重新连接`;
    case 'CONNECTION_REQUEST_INVALID': return `${prefix}对方的回应不完整。请重新连接`;
    case 'CONNECTION_AUTHORIZATION_FAILED': return `${prefix}${pick(FAILED, '授权没有完成')}。请重新连接`;
    case 'CONNECTION_AUTHORIZATION_DENIED': return `${prefix}${pick(DENIED, '对方拒绝了授权')}`;
    case 'CONNECTION_REFUSED': return `${prefix}${pick(REFUSED, '这个连接现在不能授权')}，需要运维处理`;
    case 'CONNECTION_CONFIG_CHANGED': return `${prefix}这个连接的配置刚刚变更。请重新连接`;
    case 'CONNECTION_SERVER_UNAVAILABLE': return `${prefix}对方的授权服务器没有应答，请稍后再试`;
    case 'CONNECTIONS_UNAVAILABLE': return `${prefix}连接服务暂时不可用，请稍后再试`;
    case 'NOT_FOUND': return `${prefix}没有这个连接`;
    case 'FORBIDDEN': return `${prefix}你没有权限做这件事（评审账号只给运维）`;
    case 'INVALID': return `${prefix}请求无效（返回的路径必须是本站的路径）`;
    case 'UNAUTHENTICATED': return `${prefix}登录已失效，请重新登录`;
    default: return `${prefix}出了点问题（${/^[A-Za-z0-9_]{1,64}$/.test(e.code) ? e.code : '未知'}）`;
  }
}

// ---- the third party's redirect, kept across a sign-in ----

export interface OAuthParams { code: string; state: string; iss: string; error: string }

const STASH = 'idp-workspace.oauth-callback';
const STASH_MS = 10 * 60_000;

interface Loc { pathname: string; search: string }
interface KV { getItem(k: string): string | null; setItem(k: string, v: string): void; removeItem(k: string): void }

/** The parameters of a redirect that landed on the callback page, or (after a sign-in, which loses the address) the ones kept
 * for it a moment ago. Exactly what the browser received: nothing is decoded again, trimmed or filtered. A redirect with none
 * of the three parameters that matter is none. */
export function readCallback(loc: Loc, store: KV, now = Date.now()): OAuthParams | null {
  try {
    if (loc.pathname === CALLBACK_PATH) {
      const q = new URLSearchParams(loc.search);
      const p = { code: q.get('code') ?? '', state: q.get('state') ?? '', iss: q.get('iss') ?? '', error: q.get('error') ?? '' };
      if (!p.code && !p.state && !p.error) return null;
      store.setItem(STASH, JSON.stringify({ at: now, p }));
      return p;
    }
    const raw = store.getItem(STASH);
    if (!raw) return null;
    const s = JSON.parse(raw) as { at?: number; p?: Partial<OAuthParams> };
    if (typeof s.at !== 'number' || now - s.at > STASH_MS || now < s.at || !s.p) { store.removeItem(STASH); return null; }
    return { code: String(s.p.code ?? ''), state: String(s.p.state ?? ''), iss: String(s.p.iss ?? ''), error: String(s.p.error ?? '') };
  } catch {
    return null;
  }
}

/** forgets the kept redirect: it is used once, whatever came of it */
export function clearCallback(store: KV): void {
  try { store.removeItem(STASH); } catch { /* storage unavailable */ }
}
