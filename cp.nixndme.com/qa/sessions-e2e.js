// Browser checks for login sessions: at most 5 at once, ending one from Settings, the
// auto-logout setting, and (IDLE_WAIT=1, takes ~6 minutes) a real idle logout.
// It logs in and out many times, so run it against a throwaway instance, never production:
//
//   CP_URL=http://localhost:18091 SESS_USER=tester SESS_PASS=... [IDLE_WAIT=1] node sessions-e2e.js
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE = (process.env.CP_URL || '').replace(/\/$/, '');
const USER = process.env.SESS_USER;
const PASS = process.env.SESS_PASS;
if (!BASE || !USER || !PASS) {
  console.error('set CP_URL, SESS_USER and SESS_PASS (a throwaway instance)');
  process.exit(2);
}
const MAX = 5;
const SHOTS = path.join(__dirname, 'shots');

let failed = 0;
const check = (ok, what) => { console.log(`  ${ok ? 'OK  ' : 'FAIL'} ${what}`); if (!ok) failed++; };
const step = (name) => console.log(`\n== ${name}`);

async function login(browser) {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto(BASE + '/login');
  await page.fill('input[name=username]', USER);
  await page.fill('input[name=password]', PASS);
  await Promise.all([page.waitForURL(BASE + '/'), page.click('button[type=submit]')]);
  return page;
}

async function loggedIn(page) {
  await page.goto(BASE + '/');
  return !page.url().includes('/login');
}

async function setIdle(page, seconds) {
  await page.evaluate(async (idle) => {
    const s = (await CP.api('GET', '/api/settings')).settings;
    await CP.api('PUT', '/api/settings', { ...s, session_idle: idle });
  }, seconds);
}

async function main() {
  fs.mkdirSync(SHOTS, { recursive: true });
  const browser = await chromium.launch();
  const errors = [];
  try {
    step('A 6th login ends the least recently used one');
    const pages = [];
    for (let i = 0; i < MAX + 1; i++) {
      pages.push(await login(browser));
      await new Promise((r) => setTimeout(r, 1100)); // distinct login times
    }
    pages.forEach((p) => p.on('pageerror', (e) => errors.push(e.message)));
    check(!(await loggedIn(pages[0])), 'first (oldest) browser was logged out');
    for (let i = 1; i <= MAX; i++) check(await loggedIn(pages[i]), `browser ${i + 1} still logged in`);

    step('Settings lists the logins');
    const me = pages[MAX];
    await me.goto(BASE + '/settings');
    await me.waitForSelector('#session-list li');
    check((await me.locator('#session-list li').count()) === MAX, `${MAX} rows listed`);
    check((await me.locator('#sessions').textContent()).includes(`${MAX} of at most ${MAX}`), 'count text');
    check((await me.locator('#session-list .badge', { hasText: 'This browser' }).count()) === 1, 'one row marked "This browser"');
    check(/Chrome|Browser/.test(await me.locator('#session-list li strong').first().textContent()), 'device name shown');
    await me.screenshot({ path: path.join(SHOTS, 'sessions-settings.png'), fullPage: true });

    step('Log out one other browser');
    const other = me.locator('#session-list li', { hasNot: me.locator('.badge') }).first();
    await other.locator('button', { hasText: 'Log out' }).click();
    await me.waitForFunction((n) => document.querySelectorAll('#session-list li').length === n, MAX - 1);
    check(true, 'row removed');
    let ended = 0;
    for (let i = 1; i < MAX; i++) if (!(await loggedIn(pages[i]))) ended++;
    check(ended === 1, `exactly one other browser lost its login (got ${ended})`);

    step('Auto-logout setting');
    await me.goto(BASE + '/settings');
    await me.waitForSelector('#session-list li');
    check((await me.inputValue('#s-session-idle')) === '900', 'default is 15 minutes');
    await me.selectOption('#s-session-idle', '600');
    await me.click('#idle-form button[type=submit]');
    await me.waitForFunction(() => document.getElementById('idle-msg').textContent === 'Saved');
    await me.reload();
    await me.waitForSelector('#session-list li');
    check((await me.inputValue('#s-session-idle')) === '600', '10 minutes saved');
    await setIdle(me, 900);

    step('Log out this browser from the list (needs a second click)');
    await me.reload();
    await me.waitForSelector('#session-list li');
    const mine = me.locator('#session-list li', { has: me.locator('.badge') });
    await mine.locator('button').click();
    check(me.url().endsWith('/settings'), 'first click only arms the button');
    await Promise.all([me.waitForURL(/\/login/), mine.locator('button').click()]);
    check(!(await loggedIn(me)), 'this browser is logged out');

    if (process.env.IDLE_WAIT) {
      step('Real idle logout (5 minute setting, ~6 minute wait)');
      const idle = await login(browser);
      const busy = await login(browser);
      const transfer = await login(browser);
      [idle, busy, transfer].forEach((p) => p.on('pageerror', (e) => errors.push(e.message)));
      await setIdle(idle, 300);
      await idle.goto(BASE + '/'); // dashboard: polls the share list every 30 s in the background
      await busy.goto(BASE + '/settings');
      await transfer.goto(BASE + '/live');
      await transfer.evaluate(() => CP.keepAliveWhile(() => true)); // as during a Live transfer: no input
      const until = Date.now() + 340 * 1000;
      while (Date.now() < until && !idle.url().includes('/login')) {
        await busy.keyboard.press('Shift'); // real input on the busy tab only
        await busy.waitForTimeout(20 * 1000);
      }
      check(idle.url().includes('/login?idle=1'), `unattended dashboard sent to login (${idle.url()})`);
      check(await idle.getByText('logged out', { exact: false }).isVisible(), 'login page explains why');
      await idle.screenshot({ path: path.join(SHOTS, 'sessions-idle-login.png') });
      check(busy.url().endsWith('/settings'), 'busy tab was not redirected');
      check(transfer.url().endsWith('/live'), 'page with a running transfer was not redirected');
      check(await loggedIn(transfer), 'page with a running transfer kept its login');
      check(await loggedIn(busy), 'tab with activity stayed logged in');
      await setIdle(busy, 900);
    }
    check(errors.length === 0, `no page errors ${errors.join(' | ')}`);
  } finally {
    await browser.close();
  }
  console.log(failed ? `\n${failed} FAILED` : '\nall OK');
  process.exit(failed);
}

main().catch((e) => { console.error(e); process.exit(1); });
