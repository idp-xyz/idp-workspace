import { test } from 'node:test';
import assert from 'node:assert/strict';
import { verifyAppToken } from '../src/apptoken.ts';

const enc = new TextEncoder();
const b64 = (b: Uint8Array | string) => Buffer.from(typeof b === 'string' ? enc.encode(b) : b).toString('base64url');

async function setup() {
  const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign', 'verify']);
  const jwk = await crypto.subtle.exportKey('jwk', pair.publicKey);
  const sign = async (claims: object, header: object = { alg: 'ES256', kid: 'k1', typ: 'JWT' }) => {
    const input = b64(JSON.stringify(header)) + '.' + b64(JSON.stringify(claims));
    const sig = new Uint8Array(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, pair.privateKey, enc.encode(input)));
    return input + '.' + b64(sig);
  };
  return { sign, jwks: { keys: [{ kty: 'EC', crv: 'P-256', kid: 'k1', x: jwk.x, y: jwk.y, alg: 'ES256' }] } };
}

const now = new Date(1790000000 * 1000);
const good = { iss: 'https://workspace.example', aud: 'idp-workspace-app:desk', sub: 'bob', tenant_id: 't1', workspace: { id: 'w1', name: 'us-ops' },
  app: 'desk', appVersion: '1.0.0', roles: ['approver'], iat: 1790000000, exp: 1790000300, jti: 'j' };
const opts = (jwks: { keys: object[] }) => ({ issuer: 'https://workspace.example', app: 'desk', jwks: jwks as never, now });

test('a valid app token verifies and yields the member, tenant and workspace', async () => {
  const { sign, jwks } = await setup();
  const claims = await verifyAppToken(await sign(good), opts(jwks));
  assert.equal(claims.sub, 'bob');
  assert.equal(claims.workspace.name, 'us-ops');
  assert.deepEqual(claims.roles, ['approver']);
});

test('everything else is refused', async () => {
  const { sign, jwks } = await setup();
  const other = await setup();
  const cases: Array<[string, Promise<string>, RegExp]> = [
    ['another app', sign({ ...good, aud: 'idp-workspace-app:orders' }), /another app/],
    ['another issuer', sign({ ...good, iss: 'https://evil.example' }), /issuer/],
    ['expired', sign({ ...good, exp: 1789999000 }), /expired/],
    ['from the future', sign({ ...good, iat: 1790001000 }), /future/],
    ['another key', other.sign(good), /signature/],
    ['unknown kid', sign(good, { alg: 'ES256', kid: 'k2' }), /unknown signing key/],
    ['alg none', Promise.resolve(b64(JSON.stringify({ alg: 'none', kid: 'k1' })) + '.' + b64(JSON.stringify(good)) + '.'), /algorithm/],
    ['HS256', sign(good, { alg: 'HS256', kid: 'k1' }), /algorithm/],
    ['garbage', Promise.resolve('a.b'), /malformed/],
  ];
  for (const [name, tok, re] of cases) await assert.rejects(verifyAppToken(await tok, opts(jwks)), re, name);
});
