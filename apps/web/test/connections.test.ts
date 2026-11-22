import { describe, expect, it } from 'vitest';
import { ApiError } from '../src/api.ts';
import { CONNECTION_ID, CALLBACK_PATH, clearCallback, connectionError, connectionPath, connectionsOfPath, isAuthorizationUrl, isLocalPath, readCallback, reasonWords } from '../src/connections.ts';

const kv = () => {
  const m = new Map<string, string>();
  return { m, getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v), removeItem: (k: string) => void m.delete(k) };
};

describe('isLocalPath', () => {
  it('accepts the Workspace\'s own relative paths', () => {
    for (const p of ['/', '/connections', '/connections/github', '/connections/github?from=chat', '/?view=connections']) expect(isLocalPath(p), p).toBe(true);
  });
  it('refuses everything that could leave the site', () => {
    for (const p of ['https://evil.example/', '//evil.example', '/\\evil.example', '\\\\evil.example', 'evil.example', 'connections', 'javascript:alert(1)', 'data:text/html,x', '/ /x', '/\tx', '/a\\b', '/é',
      '/%2F/evil.example', '/%5Cevil.example', '/%0d%0ax', '', '/' + 'a'.repeat(200), '/oauth/callback', '/oauth/callback?x=1', '///evil.example', null, undefined, 5, {}]) {
      expect(isLocalPath(p), String(p)).toBe(false);
    }
  });
});

describe('isAuthorizationUrl', () => {
  it('follows an address with a host and nothing else', () => {
    expect(isAuthorizationUrl('https://github.example/login/oauth/authorize?state=abc')).toBe(true);
    for (const u of ['javascript:alert(1)', 'data:text/html,x', '/relative', '//evil.example/x', 'https://user:pw@evil.example/', 'ftp://x.example/', '', 'https://x.example/a b', undefined, 3]) expect(isAuthorizationUrl(u), String(u)).toBe(false);
  });
});

describe('the connection id and the paths of the connected-services page', () => {
  it('is the bridge\'s pattern', () => {
    for (const id of ['github', 'a_b-C9', 'a'.repeat(64)]) expect(CONNECTION_ID.test(id)).toBe(true);
    for (const id of ['', 'a'.repeat(65), 'a/b', 'a b', 'https://x', 'a.b', 'é', '<b>']) expect(CONNECTION_ID.test(id), id).toBe(false);
  });
  it('builds the page\'s own path from a checked id only', () => {
    expect(connectionPath('github')).toBe('/connections/github');
    expect(connectionPath('https://evil.example')).toBe('/connections');
    expect(connectionsOfPath('/connections')).toEqual({});
    expect(connectionsOfPath('/connections/github')).toEqual({ id: 'github' });
    expect(connectionsOfPath('/connections/a%2Fb')).toBeNull();
    expect(connectionsOfPath('/connections/a/b')).toBeNull();
    expect(connectionsOfPath('/inbox')).toBeNull();
  });
});

describe('what the member reads', () => {
  it('names the reasons it knows, and nothing it does not', () => {
    expect(reasonWords('insufficient_scope')).toMatch(/更多权限/);
    expect(reasonWords('expired')).toMatch(/过期/);
    expect(reasonWords('constructor')).toBe('');
    expect(reasonWords('<script>')).toBe('');
    expect(reasonWords(undefined)).toBe('');
  });
  it('turns each fixed code into a sentence, and the reason picks among fixed ones', () => {
    expect(connectionError(new ApiError(400, 'CONNECTION_STATE_INVALID', 'x'))).toMatch(/不是你发起的/);
    expect(connectionError(new ApiError(400, 'CONNECTION_STATE_EXPIRED', 'x'))).toMatch(/超时/);
    expect(connectionError(new ApiError(400, 'CONNECTION_AUTHORIZATION_DENIED', 'access_denied'), '连接没有完成')).toBe('连接没有完成：你在对方的页面上拒绝了授权');
    expect(connectionError(new ApiError(400, 'CONNECTION_AUTHORIZATION_FAILED', 'iss_mismatch'))).toMatch(/身份与配置的不一致/);
    expect(connectionError(new ApiError(409, 'CONNECTION_REFUSED', 'pkce_unsupported'))).toMatch(/PKCE/);
    expect(connectionError(new ApiError(502, 'CONNECTIONS_UNAVAILABLE', 'x'))).toMatch(/暂时不可用/);
    expect(connectionError(new ApiError(403, 'FORBIDDEN', 'x'))).toMatch(/评审账号只给运维/);
  });
  it('never shows a message the server sent that is not one of the fixed reasons', () => {
    const hostile = '<script>alert(1)</script> https://phish.example/login';
    for (const code of ['CONNECTION_AUTHORIZATION_DENIED', 'CONNECTION_AUTHORIZATION_FAILED', 'CONNECTION_REFUSED', 'CONNECTION_STATE_INVALID', 'CONNECTIONS_UNAVAILABLE', 'NOT_FOUND', 'SOMETHING_NEW', hostile]) {
      const shown = connectionError(new ApiError(400, code, hostile));
      expect(shown, code).not.toContain('script');
      expect(shown, code).not.toContain('phish');
    }
    expect(connectionError(new ApiError(400, 'constructor', 'constructor'))).not.toMatch(/function|native/);
    expect(connectionError(new ApiError(400, 'CONNECTION_REFUSED', 'constructor'))).not.toMatch(/function|native/);
  });
});

describe('the third party\'s redirect', () => {
  it('is read exactly as the browser received it, and kept for a sign-in', () => {
    const s = kv();
    const p = readCallback({ pathname: CALLBACK_PATH, search: '?code=a%2Fb%2Bc%3D&state=S%20T&iss=https%3A%2F%2Fas.example%2F&extra=1' }, s, 1000);
    expect(p).toEqual({ code: 'a/b+c=', state: 'S T', iss: 'https://as.example/', error: '' });
    // after a sign-in the address is gone; the kept redirect is used
    expect(readCallback({ pathname: '/', search: '' }, s, 1000 + 5 * 60_000)).toEqual(p);
    clearCallback(s);
    expect(readCallback({ pathname: '/', search: '' }, s, 1000 + 5 * 60_000)).toBeNull();
  });
  it('carries an error answer, and is none without any of the parameters', () => {
    const s = kv();
    expect(readCallback({ pathname: CALLBACK_PATH, search: '?error=access_denied&state=S&error_description=Nope' }, s)).toEqual({ code: '', state: 'S', iss: '', error: 'access_denied' });
    const t = kv();
    expect(readCallback({ pathname: CALLBACK_PATH, search: '' }, t)).toBeNull();
    expect(readCallback({ pathname: CALLBACK_PATH, search: '?foo=bar' }, t)).toBeNull();
    expect(t.m.size).toBe(0);
  });
  it('is forgotten after ten minutes', () => {
    const s = kv();
    readCallback({ pathname: CALLBACK_PATH, search: '?code=c&state=s' }, s, 1000);
    expect(readCallback({ pathname: '/', search: '' }, s, 1000 + 10 * 60_000 + 1)).toBeNull();
    expect(s.m.size).toBe(0);
  });
  it('survives storage that is broken', () => {
    const broken = { getItem: () => { throw new Error('no'); }, setItem: () => { throw new Error('no'); }, removeItem: () => { throw new Error('no'); } };
    expect(readCallback({ pathname: CALLBACK_PATH, search: '?code=c&state=s' }, broken)).toBeNull();
    expect(() => clearCallback(broken)).not.toThrow();
  });
});
