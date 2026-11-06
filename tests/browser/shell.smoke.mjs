// Browser smoke test of the web shell (ADR-0001 D5, D9, D10), run by deploy/e2e.sh with BROWSER=1:
// a real Chromium signs in with a development token, sees the inbox, receives a live item from the
// stream, opens a framed third-party app and checks what the app could and could not do through the
// bridge. Uses the repository's playwright-core (a root devDependency) and its Chromium
// (npx playwright-core install chromium), or PLAYWRIGHT_CORE / CHROMIUM when given.
import { createRequire } from 'node:module';
const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_CORE || 'playwright-core');
const WS = process.env.WS_URL, TOKEN = process.env.MEMBER_TOKEN, PRODUCER = process.env.PRODUCER_TOKEN, ADMIN = process.env.ADMIN_TOKEN;
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
  // an action no item carries, started from the shell's own form (D19); the demo's webhook does not
  // listen, so what comes back is the workspace's refusal
  await page.click('nav >> text=发起');
  await page.click('form[aria-label="Demo：Ping"] >> button:has-text("发起")');
  await page.waitForSelector('.toast >> text=Ping 失败：TARGET_UNAVAILABLE', { timeout: 10000 });
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
  console.log('browser: sign-in, inbox, live stream item, an action started from the shell, framed app through the bridge ok');

  // ---- the administrator does everything through the screens, no API calls ----
  const admin = await browser.newPage();
  const adminErrors = [];
  admin.on('pageerror', (e) => adminErrors.push(String(e)));
  admin.on('dialog', (d) => d.accept('browser test'));
  await admin.goto(WS + '/');
  await admin.fill('#token', ADMIN);
  await admin.click('button[type=submit]');
  await admin.click('nav >> text=管理');
  await admin.fill('input[aria-label=名称]', 'eu-ops');
  await admin.fill('input[aria-label=显示名]', 'EU operations');
  await admin.click('button:has-text("创建")');
  await admin.waitForSelector('.toast >> text=工作空间已创建', { timeout: 10000 });
  await admin.selectOption('header select', { label: 'EU operations' });
  await admin.fill('input[aria-label=成员]', 'bob');
  await admin.fill('input[aria-label=角色]', 'approver');
  await admin.click('button:has-text("添加或更新")');
  await admin.waitForSelector('td:has-text("approver")', { timeout: 10000 });
  await admin.click('role=tab[name="应用"]');
  await admin.fill('textarea[aria-label=应用清单]', JSON.stringify({ schemaVersion: 'idp.workspace.app/v1', name: 'eu-desk', version: '1.0.0', title: 'EU desk', publisher: 'IDP',
    entry: { url: 'http://127.0.0.1:9700/demo-app.html' }, capabilities: ['context.read'], actions: [] }));
  await admin.click('button:has-text("发布")');
  await admin.waitForSelector('.toast >> text=已发布', { timeout: 10000 });
  await admin.selectOption('select[aria-label="安装哪个版本"]', 'eu-desk@1.0.0');
  await admin.click('button:has-text("安装 / 升级")');
  await admin.waitForSelector('.toast >> text=已安装', { timeout: 10000 });
  await admin.click('role=tab[name="审计"]');
  for (const a of ['workspace.create', 'member.put', 'app.publish', 'app.install']) await admin.waitForSelector(`td:text-is("${a}")`, { timeout: 10000 });
  // a bad manifest is refused by the server and the reason is shown
  await admin.click('role=tab[name="应用"]');
  await admin.fill('textarea[aria-label=应用清单]', JSON.stringify({ schemaVersion: 'idp.workspace.app/v1', name: 'Bad Name', version: '1', title: 't', publisher: 'p', capabilities: [], actions: [] }));
  await admin.click('button:has-text("发布")');
  await admin.waitForSelector('.toast >> text=INVALID_MANIFEST', { timeout: 10000 });
  // bob, now a member of eu-ops through the screen, sees the new workspace and its app; he has no admin screen
  const bob = await browser.newPage();
  await bob.goto(WS + '/');
  await bob.fill('#token', TOKEN);
  await bob.click('button[type=submit]');
  await bob.waitForSelector('header select');
  if (await bob.locator('nav >> text=管理').count() !== 0) fail('a member sees the admin screen');
  await bob.selectOption('header select', { label: 'EU operations' });
  await bob.waitForSelector('nav >> text=EU desk', { timeout: 10000 });
  if (adminErrors.length) fail('admin page errors: ' + adminErrors.join(' | '));
  console.log('PASS: browser shell: sign-in, inbox, live stream, framed app, and the administrator\'s screens (workspace, member, publish, install, audit, refusal)');
} finally {
  await browser.close();
}
