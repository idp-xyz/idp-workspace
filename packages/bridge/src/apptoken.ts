// Verifying a workspace app token in the app's backend (idp-workspace ADR-0001 D15): an ES256 JWT
// signed by the workspace, audience idp-workspace-app:<app>, keys from <workspace>/.well-known/jwks.json.
// Web Crypto only, so it runs in Node, Deno, browsers and edge workers.

export interface AppTokenClaims {
  iss: string;
  aud: string;
  sub: string;
  tenant_id: string;
  workspace: { id: string; name: string };
  app: string;
  appVersion: string;
  roles: string[];
  iat: number;
  exp: number;
  jti: string;
}

export interface JWK { kty: string; crv?: string; kid?: string; x?: string; y?: string; alg?: string; use?: string }

const dec = new TextDecoder();

function b64url(s: string): Uint8Array<ArrayBuffer> {
  const b = atob(s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (s.length % 4)) % 4));
  const out = new Uint8Array(b.length);
  for (let i = 0; i < b.length; i++) out[i] = b.charCodeAt(i);
  return out;
}

export interface VerifyOptions {
  /** the workspace's public URL (the token's iss) */
  issuer: string;
  /** this app's name; the audience must be idp-workspace-app:<app> */
  app: string;
  /** the workspace's keys: { keys: [...] } as served at /.well-known/jwks.json */
  jwks: { keys: JWK[] };
  now?: Date;
  leewaySeconds?: number;
}

/** verifyAppToken returns the claims of a valid token and throws otherwise. */
export async function verifyAppToken(token: string, opts: VerifyOptions): Promise<AppTokenClaims> {
  const parts = token.split('.');
  if (parts.length !== 3) throw new Error('malformed token');
  let header: { alg?: string; kid?: string };
  let claims: AppTokenClaims;
  try {
    header = JSON.parse(dec.decode(b64url(parts[0])));
    claims = JSON.parse(dec.decode(b64url(parts[1])));
  } catch {
    throw new Error('malformed token');
  }
  if (header.alg !== 'ES256') throw new Error('unexpected algorithm');
  const jwk = opts.jwks.keys.find((k) => k.kid === header.kid && k.kty === 'EC' && k.crv === 'P-256');
  if (!jwk) throw new Error('unknown signing key');
  const key = await crypto.subtle.importKey('jwk', { kty: 'EC', crv: 'P-256', x: jwk.x, y: jwk.y, ext: true }, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['verify']);
  const ok = await crypto.subtle.verify({ name: 'ECDSA', hash: 'SHA-256' }, key, b64url(parts[2]), new TextEncoder().encode(parts[0] + '.' + parts[1]));
  if (!ok) throw new Error('signature mismatch');
  const now = Math.floor((opts.now ?? new Date()).getTime() / 1000);
  const leeway = opts.leewaySeconds ?? 30;
  if (claims.iss !== opts.issuer) throw new Error('unexpected issuer');
  if (claims.aud !== 'idp-workspace-app:' + opts.app) throw new Error('token is for another app');
  if (typeof claims.exp !== 'number' || now > claims.exp + leeway) throw new Error('token expired');
  if (typeof claims.iat === 'number' && claims.iat > now + leeway) throw new Error('token issued in the future');
  return claims;
}
