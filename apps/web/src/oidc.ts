// Signing in to the shell with the organisation's identity provider (ADR-0001 D14): authorization
// code + PKCE (S256) as a public client. The shell holds the access token for the tab only; the
// workspace verifies it against the provider's keys like any other API caller.

export interface OIDCConfig { issuer: string; clientId: string; scope: string }
interface Discovery { authorization_endpoint: string; token_endpoint: string; end_session_endpoint?: string }
export interface Tokens { accessToken: string; refreshToken?: string; idToken?: string; expiresAt: number }

export interface KV { getItem(k: string): string | null; setItem(k: string, v: string): void; removeItem(k: string): void }

const PENDING = 'idp-workspace.oidc.pending';

function b64url(bytes: Uint8Array): string {
  let s = '';
  for (const b of bytes) s += String.fromCharCode(b);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

export async function pkcePair(): Promise<{ verifier: string; challenge: string }> {
  const verifier = b64url(crypto.getRandomValues(new Uint8Array(32)));
  const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier)));
  return { verifier, challenge: b64url(digest) };
}

export class OIDC {
  private discovery: Promise<Discovery> | null = null;
  cfg: OIDCConfig;
  private store: KV;
  private fetcher: typeof fetch;
  redirectUri: string;
  get issuer(): string { return this.cfg.issuer; }
  constructor(cfg: OIDCConfig, origin: string, store: KV = sessionStorage, fetcher: typeof fetch = (...a) => fetch(...a)) {
    this.cfg = cfg;
    this.store = store;
    this.fetcher = fetcher;
    this.redirectUri = origin + '/callback';
  }

  private discover(): Promise<Discovery> {
    this.discovery ??= this.fetcher(this.cfg.issuer.replace(/\/$/, '') + '/.well-known/openid-configuration').then(async (r) => {
      if (!r.ok) throw new Error(`discovery failed: HTTP ${r.status}`);
      return (await r.json()) as Discovery;
    });
    // a failure is not remembered: the member may fix the cause (trust a certificate) and try again without a reload
    this.discovery.catch(() => { this.discovery = null; });
    return this.discovery;
  }

  /** authorizeUrl prepares PKCE and state, remembers them for the callback, and returns where to go. */
  async authorizeUrl(): Promise<string> {
    const d = await this.discover();
    const { verifier, challenge } = await pkcePair();
    const state = b64url(crypto.getRandomValues(new Uint8Array(16)));
    this.store.setItem(PENDING, JSON.stringify({ verifier, state }));
    const q = new URLSearchParams({ response_type: 'code', client_id: this.cfg.clientId, redirect_uri: this.redirectUri, scope: this.cfg.scope || 'openid',
      state, code_challenge: challenge, code_challenge_method: 'S256' });
    return `${d.authorization_endpoint}?${q}`;
  }

  /** callback completes a sign-in from the redirect's query; it refuses a state it did not issue. */
  async callback(query: URLSearchParams): Promise<Tokens> {
    const pending = this.store.getItem(PENDING);
    this.store.removeItem(PENDING);
    if (query.get('error')) throw new Error(`sign-in refused: ${query.get('error')}`);
    const code = query.get('code');
    if (!pending || !code) throw new Error('no sign-in in progress');
    const { verifier, state } = JSON.parse(pending) as { verifier: string; state: string };
    if (query.get('state') !== state) throw new Error('sign-in state mismatch');
    return this.token({ grant_type: 'authorization_code', code, redirect_uri: this.redirectUri, client_id: this.cfg.clientId, code_verifier: verifier });
  }

  refresh(refreshToken: string): Promise<Tokens> {
    return this.token({ grant_type: 'refresh_token', refresh_token: refreshToken, client_id: this.cfg.clientId });
  }

  async logoutUrl(idToken?: string): Promise<string | null> {
    const d = await this.discover();
    if (!d.end_session_endpoint) return null;
    const q = new URLSearchParams({ client_id: this.cfg.clientId, post_logout_redirect_uri: this.redirectUri.replace(/\/callback$/, '/') });
    if (idToken) q.set('id_token_hint', idToken);
    return `${d.end_session_endpoint}?${q}`;
  }

  private async token(form: Record<string, string>): Promise<Tokens> {
    const d = await this.discover();
    const r = await this.fetcher(d.token_endpoint, { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body: new URLSearchParams(form).toString() });
    const body = await r.json().catch(() => ({}));
    if (!r.ok || !body.access_token) throw new Error(`token endpoint refused: ${body.error ?? r.status}`);
    return { accessToken: body.access_token, refreshToken: body.refresh_token, idToken: body.id_token, expiresAt: Date.now() + (body.expires_in ?? 300) * 1000 };
  }
}
