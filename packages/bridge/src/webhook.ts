// Verifying the workspace's webhook signature in an app backend (idp-workspace ADR-0001 D4):
// Idp-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<raw body>">. Uses Web Crypto, so it
// runs in Node, Deno, browsers and edge workers.

const enc = new TextEncoder();

function hex(buf: ArrayBuffer): string {
  return [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, '0')).join('');
}

// bytes copies into a fresh ArrayBuffer-backed array (Web Crypto refuses shared buffers).
function bytes(v: string | Uint8Array): Uint8Array<ArrayBuffer> {
  return typeof v === 'string' ? enc.encode(v) : new Uint8Array(v);
}

export async function sign(secret: string | Uint8Array, timestamp: string, body: string | Uint8Array): Promise<string> {
  const key = await crypto.subtle.importKey('raw', bytes(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  const b = bytes(body);
  const msg = new Uint8Array(timestamp.length + 1 + b.length);
  msg.set(enc.encode(timestamp + '.'), 0);
  msg.set(b, timestamp.length + 1);
  return hex(await crypto.subtle.sign('HMAC', key, msg));
}

function equal(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let d = 0;
  for (let i = 0; i < a.length; i++) d |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return d === 0;
}

/** verify throws unless header signs body with secret within toleranceSeconds of now. */
export async function verify(secret: string | Uint8Array, header: string | null | undefined, body: string | Uint8Array, opts: { now?: Date; toleranceSeconds?: number } = {}): Promise<void> {
  let t = '';
  let v1 = '';
  for (const part of (header ?? '').split(',')) {
    const [k, v] = part.trim().split('=', 2);
    if (k === 't') t = v ?? '';
    if (k === 'v1') v1 = v ?? '';
  }
  if (!/^[0-9]{1,12}$/.test(t) || !/^[0-9a-f]{64}$/.test(v1)) throw new Error('malformed signature header');
  const now = Math.floor((opts.now ?? new Date()).getTime() / 1000);
  if (Math.abs(now - Number(t)) > (opts.toleranceSeconds ?? 300)) throw new Error('signature timestamp outside the tolerance');
  if (!equal(await sign(secret, t, body), v1)) throw new Error('signature mismatch');
}
