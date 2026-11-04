// End-to-end checks for the hidden sign-in: start page for strangers, the secret word in its
// search box, /login looking like a missing page. Needs cp with CP_HOME_UPSTREAM pointing at
// a running start page (Glance). It sets and clears the secret word, so throwaway only:
//
//   CP_URL=http://localhost:18092 APP_USER=tester APP_PASS=... node home-e2e.js
const { chromium, devices } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE = (process.env.CP_URL || '').replace(/\/$/, '');
const USER = process.env.APP_USER;
const PASS = process.env.APP_PASS;
if (!BASE || !USER || !PASS) {
  console.error('set CP_URL, APP_USER and APP_PASS (a throwaway instance)');
  process.exit(2);
}
const WORD = 'Purple Kettle Monsoon';
const SHOTS = path.join(__dirname, 'shots');

let failed = 0;
const check = (ok, what) => { console.log(`  ${ok ? 'OK  ' : 'FAIL'} ${what}`); if (!ok) failed++; };
const step = (name) => console.log(`\n== ${name}`);
const isCSP = (p) => /Content Security Policy|Refused to/i.test(p);

async function fillLogin(page) {
  await page.fill('input[name=username]', USER);
  await page.fill('input[name=password]', PASS);
  await Promise.all([page.waitForURL(BASE + '/'), page.click('button[type=submit]')]);
}

// Collects CSP violations and script errors from a page.
function watch(page, list) {
  page.on('console', (m) => { if (m.type() === 'error') list.push(m.text()); });
  page.on('pageerror', (e) => list.push(e.message));
}

async function startPageReady(page) {
  await page.waitForSelector('.widget-type-hacker-news li', { timeout: 30000 });
}

async function search(page, q) {
  await page.fill('.search-input', q);
  await page.press('.search-input', 'Enter');
}

async function main() {
  fs.mkdirSync(SHOTS, { recursive: true });
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
  const page = await ctx.newPage();
  const problems = [];
  watch(page, problems);
  try {
    step('Before a secret word, sign-in works as before');
    await page.goto(BASE + '/');
    check(page.url() === BASE + '/login', '/ goes to the sign-in page');
    check(!(await page.content()).toLowerCase().includes('clipboard'), 'no "clipboard" on the sign-in page');
    await fillLogin(page);

    step('Set the secret word in Settings');
    await page.goto(BASE + '/settings');
    await page.waitForFunction(() => document.getElementById('word-status').textContent.startsWith('Off'));
    await page.fill('#word-new', WORD);
    await page.fill('#word-repeat', WORD);
    await page.fill('#word-password', PASS);
    await page.click('#word-form button[type=submit]');
    await page.waitForFunction(() => document.getElementById('word-status').textContent.startsWith('On'));
    check(true, 'secret word saved, status says On');

    step('Log out: the start page, not a sign-in');
    await Promise.all([page.waitForURL(/\/home\/$/), page.click('form[action="/logout"] button')]);
    await startPageReady(page);
    const text = (await page.textContent('body')).toLowerCase();
    // Whole words: headlines can contain e.g. "log ingestion".
    check(!/\b(clipboard|sign in|log in|password)\b/.test(text), 'nothing hints at a sign-in or a clipboard');
    for (const w of ['hacker-news', 'reddit', 'rss', 'weather', 'clock', 'calendar', 'releases', 'markets']) {
      check(await page.isVisible(`.widget-type-${w}`), `${w} widget shown`);
    }
    await page.waitForTimeout(1500);
    await page.screenshot({ path: path.join(SHOTS, 'home-02-start-desktop.png') });
    const csp = problems.filter(isCSP);
    check(csp.length === 0, 'no CSP violations on the start page' + (csp.length ? ': ' + csp.slice(0, 3).join(' | ') : ''));

    step('Theme picker: switch and keep the choice');
    const bg = () => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
    const bgBefore = await bg();
    await page.locator('.theme-picker:visible').first().click();
    // The picker button shows the current theme's swatch, and a hidden copy exists for phones.
    const presets = page.locator('.theme-preset[data-key]');
    await page.locator('.theme-preset[data-key="dracula"]:visible').first().click();
    await page.waitForFunction((b) => getComputedStyle(document.body).backgroundColor !== b, bgBefore);
    const bgPicked = await bg();
    await page.reload();
    await startPageReady(page);
    check(bgPicked !== bgBefore && (await bg()) === bgPicked, `theme changes to Dracula and survives a reload (${await presets.count()} swatches)`);

    step('Strangers poking around');
    const stranger = await browser.newContext();
    const sp = await stranger.newPage();
    await sp.goto(BASE + '/no-such-page');
    const missingBody = await sp.textContent('body');
    const login = await sp.goto(BASE + '/login');
    check(login.status() === 404 && (await sp.textContent('body')) === missingBody, '/login looks exactly like a missing page');
    check((await sp.goto(BASE + '/cpup')).status() === 404, '/cpup is a missing page too');
    await sp.goto(BASE + '/settings');
    check(sp.url().startsWith(BASE + '/home/'), '/settings sends a stranger to the start page');
    await stranger.close();

    step('An ordinary search goes to the web');
    await search(page, 'kubernetes gateway api');
    await page.waitForURL(/example\.com/, { timeout: 15000 });
    check(/q=kubernetes(\+|%20)gateway(\+|%20)api/.test(page.url()), `searched: ${page.url()}`);

    step('The secret word opens the sign-in');
    await page.goto(BASE + '/');
    await startPageReady(page);
    await search(page, '  purple KETTLE monsoon ');
    await page.waitForURL(BASE + '/login');
    check(await page.isVisible('text=Sign in'), 'sign-in page shown after the word');
    await fillLogin(page);
    check(await page.isVisible('#new-share'), 'signed in: the dashboard');
    await page.goto(BASE + '/home/');
    await startPageReady(page);
    check(page.url().endsWith('/home/'), 'the start page still opens while signed in (/home/)');

    step('Phone layout');
    const phone = await browser.newContext({ ...devices['Pixel 7'] });
    const pp = await phone.newPage();
    const phoneProblems = [];
    watch(pp, phoneProblems);
    await pp.goto(BASE + '/');
    await startPageReady(pp);
    await pp.waitForTimeout(1500);
    await pp.screenshot({ path: path.join(SHOTS, 'home-03-start-phone.png') });
    const overflow = await pp.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    check(overflow <= 1, `no sideways scrolling on a phone (${overflow}px)`);
    check(phoneProblems.filter(isCSP).length === 0, 'no CSP violations on the phone');
    await phone.close();

    step('Turn the word off again (leaves the instance as found)');
    await page.goto(BASE + '/settings');
    await page.waitForFunction(() => document.getElementById('word-status').textContent.startsWith('On'));
    await page.fill('#word-password', PASS);
    await page.click('#word-clear');
    await page.click('#word-clear');
    await page.waitForFunction(() => document.getElementById('word-status').textContent.startsWith('Off'));
    const open = await browser.newContext();
    const op = await open.newPage();
    check((await op.goto(BASE + '/login')).status() === 200, '/login reachable again');
    await open.close();

    const other = problems.filter((p) => !isCSP(p) && !/Failed to load resource/i.test(p));
    check(other.length === 0, 'no script errors' + (other.length ? ': ' + other.slice(0, 3).join(' | ') : ''));
  } finally {
    await browser.close();
  }
  console.log(failed ? `\n${failed} check(s) FAILED` : '\nAll checks passed');
  process.exit(failed ? 1 : 0);
}

main().catch((e) => { console.error(e); process.exit(1); });
