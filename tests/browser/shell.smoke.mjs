// Browser smoke test of the web shell (ADR-0001 D5, D9, D10), run by deploy/e2e.sh with BROWSER=1:
// a real Chromium signs in with a development token, sees the inbox, receives a live item from the
// stream, opens a framed third-party app and checks what the app could and could not do through the
// bridge. Uses the repository's playwright-core (a root devDependency) and its Chromium
// (npx playwright-core install chromium), or PLAYWRIGHT_CORE / CHROMIUM when given.
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_CORE || 'playwright-core');
const WS = process.env.WS_URL, TOKEN = process.env.MEMBER_TOKEN, PRODUCER = process.env.PRODUCER_TOKEN;
const fail = (msg) => { console.error('FAIL: ' + msg); process.exit(1); };

const browser = await chromium.launch({ ...(process.env.CHROMIUM ? { executablePath: process.env.CHROMIUM } : {}), args: ['--no-sandbox'] });
try {
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e)));
  page.on('console', (m) => { if (m.type() === 'error') errors.push(m.text()); });
  await page.goto(WS + '/');
  await page.fill('#token', TOKEN);
  await page.click('button[type=submit]');
  try {
    await page.waitForSelector('text=Approve the refund', { timeout: 15000 });
  } catch (e) {
    fail('inbox not shown: ' + errors.join(' | ') + '\n' + (await page.content()).slice(0, 2000));
  }
  // a new item arrives over the stream without reloading
  const res = await fetch(WS + '/v1/workspaces/us-ops/inbox/items', { method: 'POST', headers: { Authorization: 'Bearer ' + PRODUCER, 'Content-Type': 'application/json' },
    body: JSON.stringify({ externalRef: 'browser:1', kind: 'notification', title: 'Live from the stream', ref: { system: 'external', kind: 'n', id: 'b1' } }) });
  if (res.status !== 201) fail('producer put ' + res.status);
  await page.waitForSelector('text=Live from the stream', { timeout: 15000 });
  // the framed app
  await page.click('nav >> text=Demo');
  const frame = await (await page.waitForSelector('iframe.app-frame')).contentFrame();
  await frame.waitForFunction(() => document.getElementById('log')?.textContent.includes('done'), null, { timeout: 15000 });
  const log = await frame.textContent('#log');
  const want = ['hello us-ops context.read,actions.invoke,ui.notify', 'context order:1001', 'inbox CAPABILITY_DENIED', 'invoke TARGET_UNAVAILABLE', 'notify ok', 'token-visible false'];
  for (const w of want) if (!log.includes(w)) fail(`app log lacks "${w}":\n${log}`);
  await page.waitForSelector('.toast >> text=Demo: hello from the app', { timeout: 5000 });
  const sandbox = await page.getAttribute('iframe.app-frame', 'sandbox');
  if (sandbox !== 'allow-scripts allow-forms allow-same-origin') fail('sandbox ' + sandbox);
  const unexpected = errors.filter((e) => !/TARGET_UNAVAILABLE|502|CAPABILITY_DENIED/.test(e));
  if (unexpected.length) fail('page errors: ' + unexpected.join(' | '));
  console.log('PASS: browser shell: sign-in, inbox, live stream item, framed app through the bridge');
} finally {
  await browser.close();
}
