// @vitest-environment node
import { describe, expect, it } from 'vitest';
import { OIDC, pkcePair } from '../src/oidc.ts';

const mem = () => { const m = new Map<string, string>(); return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v), removeItem: (k: string) => void m.delete(k) }; };

describe('OIDC', () => {
  it('builds a PKCE authorization request and completes it only with the state it issued', async () => {
    const posts: URLSearchParams[] = [];
    const fetcher = (async (url: string, init?: RequestInit) => {
      if (url.endsWith('/.well-known/openid-configuration')) return new Response(JSON.stringify({ authorization_endpoint: 'https://idp/auth', token_endpoint: 'https://idp/token', end_session_endpoint: 'https://idp/logout' }));
      posts.push(new URLSearchParams(String(init?.body)));
      return new Response(JSON.stringify({ access_token: 'at', refresh_token: 'rt', id_token: 'it', expires_in: 300 }));
    }) as unknown as typeof fetch;
    const store = mem();
    const o = new OIDC({ issuer: 'https://idp/realms/idp/', clientId: 'workspace-shell', scope: 'openid' }, 'http://127.0.0.1:8560', store, fetcher);
    const url = new URL(await o.authorizeUrl());
    const q = url.searchParams;
    expect(url.origin + url.pathname).toBe('https://idp/auth');
    expect(q.get('code_challenge_method')).toBe('S256');
    expect(q.get('redirect_uri')).toBe('http://127.0.0.1:8560/callback');
    expect(q.get('client_id')).toBe('workspace-shell');
    // a forged state is refused and the pending sign-in is consumed
    await expect(o.callback(new URLSearchParams({ code: 'c', state: 'forged' }))).rejects.toThrow(/state mismatch/);
    await expect(o.callback(new URLSearchParams({ code: 'c', state: q.get('state')! }))).rejects.toThrow(/no sign-in in progress/);
    // a real round trip sends the verifier that matches the challenge
    const q2 = new URL(await o.authorizeUrl()).searchParams;
    const t = await o.callback(new URLSearchParams({ code: 'c2', state: q2.get('state')! }));
    expect(t).toMatchObject({ accessToken: 'at', refreshToken: 'rt', idToken: 'it' });
    const sent = posts.at(-1)!;
    expect(sent.get('grant_type')).toBe('authorization_code');
    const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(sent.get('code_verifier')!)));
    expect(Buffer.from(digest).toString('base64url')).toBe(q2.get('code_challenge'));
    await o.refresh('rt');
    expect(posts.at(-1)!.get('grant_type')).toBe('refresh_token');
    expect(await o.logoutUrl('it')).toContain('id_token_hint=it');
    await expect(o.callback(new URLSearchParams({ error: 'access_denied' }))).rejects.toThrow(/access_denied/);
  });

  it('pkce pairs are fresh and URL-safe', async () => {
    const a = await pkcePair(), b = await pkcePair();
    expect(a.verifier).not.toBe(b.verifier);
    expect(a.verifier).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });
});

describe('OIDC discovery failures', () => {
  it('does not remember a failed discovery: the next attempt asks again', async () => {
    let calls = 0;
    const fetcher = (async () => {
      calls += 1;
      if (calls === 1) throw new TypeError('Failed to fetch'); // an untrusted certificate, say
      return new Response(JSON.stringify({ authorization_endpoint: 'https://idp.example/auth', token_endpoint: 'https://idp.example/token' }), { status: 200 });
    }) as unknown as typeof fetch;
    const store = new Map<string, string>();
    const o = new OIDC({ issuer: 'https://idp.example/realms/idp', clientId: 'shell', scope: 'openid' }, 'http://localhost:8560', { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => void store.set(k, v), removeItem: (k) => void store.delete(k) }, fetcher);
    await expect(o.authorizeUrl()).rejects.toThrow('Failed to fetch');
    expect(await o.authorizeUrl()).toContain('https://idp.example/auth');
    expect(calls).toBe(2);
    expect(o.issuer).toBe('https://idp.example/realms/idp');
  });
});
