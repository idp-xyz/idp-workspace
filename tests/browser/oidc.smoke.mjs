// Browser smoke test of sign-in through the identity provider (ADR-0001 D14-D16), run by
// deploy/e2e.sh with OIDC=1 BROWSER=1: Chromium signs bob in on Keycloak's login page (authorization
// code + PKCE), bob starts an agent run from an inbox item (the workspace exchanges his token for the
// HOST's audience), an app gets an app token that a backend verifies with the workspace's JWKS, and
// sign-out ends the Keycloak session.
import { createRequire } from 'node:module';
import { verifyAppToken } from '../../packages/bridge/src/apptoken.ts';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_CORE || 'playwright-core');
const WS = process.env.WS_URL, PRODUCER = process.env.PRODUCER_TOKEN, USER = process.env.KC_USER, PASSWORD = process.env.KC_PASSWORD;
const fail = (msg) => { console.error('FAIL: ' + msg); process.exit(1); };

const browser = await chromium.launch({ ...(process.env.CHROMIUM ? { executablePath: process.env.CHROMIUM } : {}), args: ['--no-sandbox'] });
try {
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  await page.goto(WS + '/');
  if (await page.locator('#token').count() !== 0) fail('the development token form is offered although tokens come from the identity provider');
  await page.click('text=使用企业账号登录');
  await page.waitForURL(/127\.0\.0\.1:7080\/realms\/idp\/protocol\/openid-connect\/auth/, { timeout: 15000 });
  if (!new URL(page.url()).searchParams.get('code_challenge')) fail('no PKCE challenge in the authorization request');
  await page.fill('#username', USER);
  await page.fill('#password', PASSWORD);
  await page.click('#kc-login');
  await page.waitForURL(WS + '/', { timeout: 15000 });
  await page.waitForSelector('.who', { timeout: 15000 });
  if ((await page.textContent('.who')) !== USER) fail('signed in as ' + (await page.textContent('.who')));
  // an item whose action starts an agent run on the HOST with bob's exchanged token
  const res = await fetch(WS + '/v1/workspaces/us-ops/inbox/items', { method: 'POST', headers: { Authorization: 'Bearer ' + PRODUCER, 'Content-Type': 'application/json' },
    body: JSON.stringify({ externalRef: 'oidc:1', kind: 'task', title: 'Browser question', ref: { system: 'external', kind: 't', id: 'o1' }, assignees: { subjects: [USER] },
      actions: [{ app: 'orders', action: 'order.review', label: 'Ask from the browser', input: { message: 'asked in the browser' } }] }) });
  if (res.status !== 201) fail('producer put ' + res.status + ' ' + (await res.text()));
  await page.click('button:has-text("Ask from the browser")', { timeout: 15000 });
  await page.waitForSelector('.toast >> text=已提交', { timeout: 15000 });
  await page.waitForSelector('text=Echo: asked in the browser', { timeout: 30000 });
  // the app's token, verified as an app backend would: the workspace's JWKS only
  await page.click('nav >> text=Desk');
  const frame = await (await page.waitForSelector('iframe.app-frame')).contentFrame();
  await frame.waitForFunction(() => document.getElementById('token')?.textContent.length > 0, null, { timeout: 15000 });
  const token = await frame.textContent('#token');
  if (token.startsWith('ERROR')) fail('identity.token: ' + token);
  const jwks = await (await fetch(WS + '/.well-known/jwks.json')).json();
  const claims = await verifyAppToken(token, { issuer: 'http://127.0.0.1:8560', app: 'desk', jwks });
  if (claims.sub !== USER || claims.workspace.name !== 'us-ops') fail('app token claims ' + JSON.stringify(claims));
  await verifyAppToken(token, { issuer: 'http://127.0.0.1:8560', app: 'orders', jwks }).then(() => fail('the desk token verified for orders'), () => {});
  // sign out ends the identity provider's session too: the next sign-in asks for the password again
  await page.click('header button:has-text("退出")');
  await page.waitForSelector('text=使用企业账号登录', { timeout: 15000 });
  await page.click('text=使用企业账号登录');
  await page.waitForSelector('#username', { timeout: 15000 });
  if (errors.length) fail('page errors: ' + errors.join(' | '));
  console.log('PASS: OIDC browser: Keycloak sign-in with PKCE, run with an exchanged token, app token verified by JWKS, sign-out');
} finally {
  await browser.close();
}
