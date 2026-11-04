// End-to-end checks for the phone app (share sheet) and API tokens + the cpup command.
// Creates and revokes tokens and logs in and out, so run it against a throwaway instance:
//
//   CP_URL=http://localhost:18092 APP_USER=tester APP_PASS=... node app-e2e.js
//
// Needs bash and curl on this machine (cpup is run for real, downloaded from the server).
const { chromium } = require('playwright');
const { execFileSync, spawnSync } = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');

const BASE = (process.env.CP_URL || '').replace(/\/$/, '');
const USER = process.env.APP_USER;
const PASS = process.env.APP_PASS;
if (!BASE || !USER || !PASS) {
  console.error('set CP_URL, APP_USER and APP_PASS (a throwaway instance)');
  process.exit(2);
}
const SHOTS = path.join(__dirname, 'shots');
const TMP = fs.mkdtempSync(path.join(os.tmpdir(), 'cp-app-e2e-'));

let failed = 0;
const check = (ok, what) => { console.log(`  ${ok ? 'OK  ' : 'FAIL'} ${what}`); if (!ok) failed++; };
const step = (name) => console.log(`\n== ${name}`);
const sha = (buf) => crypto.createHash('sha256').update(buf).digest('hex');

async function login(page) {
  await page.goto(BASE + '/login');
  await page.fill('input[name=username]', USER);
  await page.fill('input[name=password]', PASS);
  await Promise.all([page.waitForURL(BASE + '/'), page.click('button[type=submit]')]);
}

// Runs the downloaded cpup. Returns { code, out, err }.
function cpup(args, { input, token } = {}) {
  const r = spawnSync('bash', [path.join(TMP, 'cpup'), ...args], {
    input: input === undefined ? '' : input,
    env: { PATH: process.env.PATH, HOME: TMP, CP_URL: BASE, CP_TOKEN: token },
    encoding: 'utf8', timeout: 120000,
  });
  return { code: r.status, out: (r.stdout || '').trim(), err: (r.stderr || '').trim() };
}

const slugOf = (url) => (url || '').split('/s/')[1];

async function shareBySlug(page, slug) {
  return page.evaluate(async (s) => (await CP.api('GET', '/api/shares?status=')).shares.find((x) => x.slug === s), slug);
}

// Posts a share-sheet item the way Android does: a real multipart form navigation to the
// share_target action, which the service worker answers.
async function shareSheet(page, { title = '', text = '', url = '', files = [] }) {
  await page.evaluate(({ title, text, url }) => {
    const f = document.createElement('form');
    f.method = 'POST';
    f.action = '/share-target';
    f.enctype = 'multipart/form-data';
    f.id = 'fake-share';
    for (const [k, v] of Object.entries({ title, text, url })) {
      const i = document.createElement('input');
      i.name = k;
      i.value = v;
      f.append(i);
    }
    const fi = document.createElement('input');
    fi.type = 'file';
    fi.name = 'files';
    fi.multiple = true;
    f.append(fi);
    document.body.append(f);
  }, { title, text, url });
  if (files.length) await page.setInputFiles('#fake-share input[type=file]', files);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/share-target')), page.evaluate(() => document.getElementById('fake-share').submit())]);
}

async function main() {
  fs.mkdirSync(SHOTS, { recursive: true });
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    step('App manifest and service worker');
    await login(page);
    const swState = await page.evaluate(async () => {
      const reg = await navigator.serviceWorker.ready;
      return { scope: reg.scope, state: reg.active && reg.active.state };
    });
    check(swState.scope === BASE + '/' && swState.state === 'activated', `service worker active for the whole site (${swState.scope})`);
    const cdp = await ctx.newCDPSession(page);
    const inst = await cdp.send('Page.getInstallabilityErrors');
    check(inst.installabilityErrors.length === 0, 'Chrome sees cp as installable' +
      (inst.installabilityErrors.length ? ': ' + JSON.stringify(inst.installabilityErrors) : ''));
    const man = await cdp.send('Page.getAppManifest');
    check(man.errors.length === 0 && /share_target/.test(man.data), 'manifest parses and declares a share target');

    const setDefaultPublic = (on) => page.evaluate(async (v) => {
      const s = (await CP.api('GET', '/api/settings')).settings;
      await CP.api('PUT', '/api/settings', { ...s, default_public: v });
    }, on);
    await setDefaultPublic(true);
    await page.reload();

    step('Share sheet: text + file + title');
    const notes = path.join(TMP, 'notes from phone.txt');
    fs.writeFileSync(notes, 'shared file body\n');
    const photo = path.join(TMP, 'photo.bin');
    const photoBytes = crypto.randomBytes(300 * 1024);
    fs.writeFileSync(photo, photoBytes);
    await shareSheet(page, { title: 'From my phone', text: 'Some shared words', files: [notes, photo] });
    await page.waitForSelector('#new-dialog[open]', { timeout: 10000 });
    check(new URL(page.url()).search === '', 'address bar cleaned up after the share');
    check((await page.inputValue('#title')) === 'From my phone', 'title pre-filled');
    check((await page.inputValue('#text-input')) === 'Some shared words', 'text pre-filled');
    const picked = await page.$$eval('#picked .file-name', (els) => els.map((e) => e.textContent));
    check(picked.length === 2 && picked.includes('notes from phone.txt') && picked.includes('photo.bin'), `both files picked (${picked})`);
    check(!(await page.isChecked('#public')) && (await page.isVisible('#shared-note')),
      'starts private with a "check it" note, though new shares default to public');
    await page.screenshot({ path: path.join(SHOTS, 'app-01-share-sheet-dialog.png') });
    await page.click('#create-btn');
    await page.waitForFunction(() => !document.getElementById('new-dialog').open, null, { timeout: 30000 });
    const made = await page.evaluate(async () => (await CP.api('GET', '/api/shares')).shares[0]);
    check(made.title === 'From my phone' && !!made.text && made.files.length === 2, 'share created from the share sheet');
    const pf = made.files.find((f) => f.name === 'photo.bin');
    const dl = await page.evaluate(async (u) => {
      const b = new Uint8Array(await (await fetch(u)).arrayBuffer());
      return Array.from(b.slice(0, 16)).join(',') + ':' + b.length;
    }, `/d/${made.slug}/${pf.id}`);
    check(dl === Array.from(photoBytes.slice(0, 16)).join(',') + ':' + photoBytes.length, 'shared file stored byte for byte');

    check(!made.public, 'created private');
    await page.goto(BASE + '/');
    await page.click('#new-share');
    check((await page.isChecked('#public')) && !(await page.isVisible('#shared-note')), 'a normal New share keeps the defaults');
    await page.keyboard.press('Escape');
    await setDefaultPublic(false);

    step('A form on another site cannot pre-fill the dialog');
    await page.route('http://evil.test/**', (r) => r.fulfill({ contentType: 'text/html', body: '<!doctype html><title>evil</title><p>hi</p>' }));
    await page.goto('http://evil.test/');
    await page.evaluate((base) => {
      const f = document.createElement('form');
      f.method = 'POST';
      f.action = base + '/share-target';
      f.enctype = 'multipart/form-data';
      const i = document.createElement('input');
      i.name = 'text';
      i.value = 'planted by another site';
      f.append(i);
      document.body.append(f);
      f.submit();
    }, BASE);
    await page.waitForURL(BASE + '/');
    await page.waitForTimeout(800);
    check(!(await page.isVisible('#new-dialog[open]')), 'cross-site post ignored, no dialog');
    const leftover = await page.evaluate(() => new Promise((res) => {
      const rq = indexedDB.open('cp-share-target', 1);
      rq.onupgradeneeded = () => rq.result.createObjectStore('pending');
      rq.onsuccess = () => { const g = rq.result.transaction('pending').objectStore('pending').get('latest'); g.onsuccess = () => res(!!g.result); };
    }));
    check(!leftover, 'nothing stored from the other site');
    await page.unroute('http://evil.test/**');

    step('Logging out drops a share that was not picked up');
    await page.evaluate(() => new Promise((res) => {
      const rq = indexedDB.open('cp-share-target', 1);
      rq.onupgradeneeded = () => rq.result.createObjectStore('pending');
      rq.onsuccess = () => {
        const tx = rq.result.transaction('pending', 'readwrite');
        tx.objectStore('pending').put({ at: Date.now(), title: '', text: 'left behind', url: '', files: [] }, 'latest');
        tx.oncomplete = () => { rq.result.close(); res(); };
      };
    }));
    await Promise.all([page.waitForURL(/\/login/), page.click('form[action="/logout"] button')]);
    await login(page);
    await page.waitForTimeout(800);
    check(!(await page.isVisible('#new-dialog[open]')), 'nothing pops up after the next login');

    step('Share sheet: a web page (title + URL in text) becomes a link');
    await page.goto(BASE + '/');
    await shareSheet(page, { title: 'Interesting article', text: 'https://example.org/article?id=7' });
    await page.waitForSelector('#new-dialog[open]');
    check((await page.inputValue('#link-input')) === 'https://example.org/article?id=7', 'URL went to the links part');
    check((await page.inputValue('#text-input')) === '', 'text part left empty');
    await page.keyboard.press('Escape');

    step('Share sheet: url field plus "Title https://…" text is not duplicated');
    await page.goto(BASE + '/');
    await shareSheet(page, { text: 'Read this https://example.org/a', url: 'https://example.org/a' });
    await page.waitForSelector('#new-dialog[open]');
    check((await page.inputValue('#link-input')) === 'https://example.org/a' && (await page.inputValue('#text-input')) === 'Read this',
      'link once, the words kept as text');
    await page.keyboard.press('Escape');

    step('Share sheet while logged out: kept through the login');
    await page.evaluate(() => fetch('/logout', { method: 'POST' }));
    await page.goto(BASE + '/login');
    check(page.url().includes('/login'), 'logged out');
    await shareSheet(page, { text: 'shared before login' });
    check(page.url().includes('/login'), 'sent to the login page');
    await login(page);
    await page.waitForSelector('#new-dialog[open]', { timeout: 10000 });
    check((await page.inputValue('#text-input')) === 'shared before login', 'dialog opens with it after logging in');
    await page.keyboard.press('Escape');
    await page.reload();
    await page.waitForTimeout(800);
    check(!(await page.isVisible('#new-dialog[open]')), 'a pending share opens only once');

    step('Visitors of a public share get no service worker');
    const visitor = await browser.newContext();
    const vpage = await visitor.newPage();
    const pub = await page.evaluate(async () => {
      const t = new Blob(['public text']);
      const up = await CP.api('POST', '/api/uploads', { name: 'paste.txt', size: t.size, mime: 'text/plain' });
      await fetch(`/api/uploads/${up.id}/0`, { method: 'PUT', headers: { 'X-CSRF-Token': CP.csrf }, body: t });
      return CP.api('POST', '/api/shares', { text_upload: up.id, public: true, expires_in: 3600 });
    });
    await vpage.goto(pub.url.replace(/^https?:\/\/[^/]+/, BASE));
    await vpage.waitForTimeout(1000);
    const vRegs = await vpage.evaluate(async () => (await navigator.serviceWorker.getRegistrations()).length);
    check(vRegs === 0 && (await vpage.isVisible('text=public text')), 'share page works, no worker registered');
    await visitor.close();

    step('API token: create in Settings');
    await page.goto(BASE + '/settings');
    await page.fill('#tok-name', 'e2e laptop');
    await page.fill('#tok-password', PASS);
    await page.click('#token-form button[type=submit]');
    await page.waitForSelector('#tok-new:not([hidden])');
    const token = (await page.textContent('#tok-secret')).trim();
    check(/^cpt_[A-Za-z0-9]{43}$/.test(token), 'token shown once in the expected format');
    const install = await page.textContent('#tok-install');
    check(install.includes(BASE + '/cpup') && install.includes('cpup --setup'), 'install command shown');
    const listed = await page.textContent('#token-list');
    check(listed.includes('e2e laptop') && listed.includes('never used'), 'token listed, never used');
    await page.screenshot({ path: path.join(SHOTS, 'app-02-token-created.png'), fullPage: true });
    await page.reload();
    check(!(await page.content()).includes(token), 'secret not shown again after reload');

    step('cpup: download and run it for real');
    execFileSync('curl', ['-fsSL', BASE + '/cpup', '-o', path.join(TMP, 'cpup')]);
    const help = cpup(['--help'], { token });
    check(help.code === 0 && help.out.includes('Usage: cpup'), 'cpup --help');

    const t1 = cpup(['-t', 'deploy log', '-e', '1h', '-p'], { input: 'line one\nline two\n', token });
    check(t1.code === 0 && /^http:\/\/localhost:\d+\/s\/[A-Za-z0-9]{22}$/.test(t1.out), `text from stdin -> ${t1.out || t1.err}`);
    const raw = execFileSync('curl', ['-fsS', `${BASE}/r/${slugOf(t1.out)}`]).toString();
    check(raw === 'line one\nline two\n', 'public raw text matches stdin exactly');
    const s1 = await shareBySlug(page, slugOf(t1.out));
    check(s1.title === 'deploy log' && s1.public && s1.expires_at - s1.created_at === 3600, 'title, public and 1h expiry applied');

    const big = path.join(TMP, 'disk image.bin');
    const bigBytes = crypto.randomBytes(20 * 1024 * 1024 + 12345); // 3 chunks of 8 MB
    fs.writeFileSync(big, bigBytes);
    const t2 = cpup([big, notes, '-P'], { token });
    check(t2.code === 0, `20 MB file + small file -> ${t2.out || t2.err}`);
    const s2 = await shareBySlug(page, slugOf(t2.out));
    check(s2 && s2.files.length === 2 && !s2.public, 'two files, private');
    const bf = s2.files.find((f) => f.name === 'disk image.bin');
    const cookie = (await ctx.cookies()).find((c) => c.name === 'cps').value;
    const got = execFileSync('curl', ['-fsS', '-b', `cps=${cookie}`, `${BASE}/d/${s2.slug}/${bf.id}`], { maxBuffer: 64 << 20 });
    check(sha(got) === sha(bigBytes), 'big file downloads byte for byte (sha256)');

    const t3 = cpup(['https://example.org/one', '-l', 'https://example.org/two'], { token });
    const s3 = await shareBySlug(page, slugOf(t3.out));
    check(t3.code === 0 && s3.links.length === 2 && !s3.text && !s3.files.length, 'bare URL and -l make a link share');

    const tricky = 'Quote " backslash \\ tab\t ünïcødé ✓';
    const t4 = cpup(['-t', tricky, '-b', '-m', '3', '-L', 'yaml', '-', notes], { input: 'a: 1\n', token });
    const s4 = await shareBySlug(page, slugOf(t4.out));
    check(t4.code === 0 && s4 && s4.title === tricky, `odd characters in the title survive (${s4 && JSON.stringify(s4.title)})`);
    check(s4 && s4.burn && s4.max_downloads === 3 && s4.language === 'yaml' && !!s4.text && s4.files.length === 1,
      'stdin "-" with a file, burn, max downloads, language');

    const plain = spawnSync('bash', [path.join(TMP, 'cpup')], {
      input: 'x', env: { PATH: process.env.PATH, HOME: TMP, CP_URL: 'http://cp.example.com', CP_TOKEN: token }, encoding: 'utf8',
    });
    check(plain.status !== 0 && plain.stderr.includes('must start with https://'), 'refuses to send the token over plain http');
    const bad = cpup(['-e', '5x'], { input: 'x', token });
    check(bad.code !== 0 && bad.err.includes('bad expiry'), 'bad expiry refused with a clear message');
    const missing = cpup([path.join(TMP, 'nope.txt')], { token });
    check(missing.code !== 0 && missing.err.includes('not a readable file'), 'missing file refused');
    const tooBig = path.join(TMP, 'too-big.bin');
    fs.writeFileSync(tooBig, Buffer.alloc(101 * 1024 * 1024));
    const tb = cpup([tooBig], { token });
    check(tb.code !== 0 && /between 1 byte and/.test(tb.err), `over the size limit refused (${tb.err})`);
    fs.rmSync(tooBig);

    step('cpup --setup saves the config privately');
    const setup = spawnSync('bash', [path.join(TMP, 'cpup'), '--setup'], {
      input: `${BASE}\n${token}\n`, env: { PATH: process.env.PATH, HOME: TMP }, encoding: 'utf8',
    });
    const cfg = path.join(TMP, '.config', 'cpup', 'config');
    check(setup.status === 0 && fs.existsSync(cfg) && (fs.statSync(cfg).mode & 0o077) === 0, `config written with mode 600 ${setup.stderr}`);
    const viaCfg = spawnSync('bash', [path.join(TMP, 'cpup')], { input: 'from config', env: { PATH: process.env.PATH, HOME: TMP }, encoding: 'utf8' });
    check(viaCfg.status === 0 && viaCfg.stdout.includes('/s/'), `cpup works from the saved config ${viaCfg.stderr}`);

    step('Token use shows in Settings; revoke stops cpup');
    await page.goto(BASE + '/settings');
    await page.waitForSelector('#token-list li');
    check((await page.textContent('#token-list')).includes('last used'), 'last use shown');
    const revoke = page.locator('#token-list li', { hasText: 'e2e laptop' }).locator('button');
    await revoke.click();
    await revoke.click();
    await page.waitForFunction(() => !document.getElementById('token-list').textContent.includes('e2e laptop'));
    const after = cpup([], { input: 'should fail', token });
    check(after.code !== 0 && after.err.includes('wrong or was revoked'), `revoked token refused (${after.err})`);

    step('Dashboard lists what cpup made');
    await page.goto(BASE + '/');
    await page.waitForSelector('.share-row');
    check((await page.textContent('#shares')).includes('deploy log'), 'cpup share visible on the dashboard');

    check(errors.length === 0, 'no JavaScript errors' + (errors.length ? ': ' + errors.join(' | ') : ''));
  } finally {
    await browser.close();
    fs.rmSync(TMP, { recursive: true, force: true });
  }
  console.log(failed ? `\n${failed} check(s) FAILED` : '\nAll checks passed');
  process.exit(failed ? 1 : 0);
}

main().catch((e) => { console.error(e); process.exit(1); });
