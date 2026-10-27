// Headless end-to-end checks for cp (Playwright), in the same style as the lab's other QA
// scripts: every step prints OK/FAIL, screenshots land in qa/shots/, exit code = failures.
// Credentials come from ../.env (git-ignored) and are never printed.
//
//   CP_URL=https://cp.example.com node e2e.js     (default: CP_BASE_URL from .env)
//   BIG=1 node e2e.js                             also push a full 100 MB file through
const { chromium } = require('playwright');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');

const ROOT = path.join(__dirname, '..');
const env = {};
for (const l of fs.readFileSync(path.join(ROOT, '.env'), 'utf8').split('\n')) {
  const m = l.match(/^([A-Z0-9_]+)=(.*)$/);
  if (m) env[m[1]] = m[2];
}
const BASE = (process.env.CP_URL || env.CP_BASE_URL).replace(/\/$/, '');
const USER = env.CP_ADMIN_USER;
const PASS = env.CP_ADMIN_PASSWORD;
const SHOTS = path.join(__dirname, 'shots');
const FIX = path.join(__dirname, 'fixtures');
const MB = 1 << 20;

let failed = 0;
const check = (ok, what) => { console.log(`  ${ok ? 'OK  ' : 'FAIL'} ${what}`); if (!ok) failed++; };
const step = (name) => console.log(`\n== ${name}`);
const sha = (buf) => crypto.createHash('sha256').update(buf).digest('hex');

async function shot(page, name) {
  fs.mkdirSync(SHOTS, { recursive: true });
  await page.screenshot({ path: path.join(SHOTS, `${name}.png`) });
}

function fixture(name, bytes) {
  fs.mkdirSync(FIX, { recursive: true });
  const p = path.join(FIX, name);
  if (!fs.existsSync(p) || fs.statSync(p).size !== bytes) {
    const fd = fs.openSync(p, 'w');
    for (let left = bytes; left > 0; left -= Math.min(left, 4 * MB)) fs.writeSync(fd, crypto.randomBytes(Math.min(left, 4 * MB)));
    fs.closeSync(fd);
  }
  return p;
}

const created = []; // slugs to delete at the end
let ownerPage = null;
let tempPassword = null; // set while the password-change check has a temporary password active

async function emergencyCleanup() {
  if (!ownerPage) return;
  try {
    if (tempPassword) {
      await ownerPage.evaluate(([cur, pw]) => CP.api('POST', '/api/account/password', { current: cur, new: pw }), [tempPassword, PASS]);
      console.log('  restored the .env password');
    }
    for (const slug of created) await ownerPage.evaluate((x) => CP.api('DELETE', '/api/shares/' + x).catch(() => null), slug);
    console.log(`  deleted ${created.length} test share(s)`);
  } catch (e) {
    console.log('  cleanup failed: ' + e.message + ' (run `cp reset-password` if login fails)');
  }
}
for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, async () => { console.log(`\n${sig}: cleaning up before exit`); await emergencyCleanup(); process.exit(130); });
}

async function login(page, user = USER, pass = PASS) {
  await page.goto(`${BASE}/login`);
  await page.fill('input[name=username]', user);
  await page.fill('input[name=password]', pass);
  await Promise.all([page.waitForURL((u) => !u.pathname.startsWith('/login'), { timeout: 15000 }).catch(() => {}), page.click('button[type=submit]')]);
  return !page.url().includes('/login');
}

// Drives the New share dialog like a person would and returns the created share (from the API).
async function newShare(page, o) {
  await page.click('#new-share');
  await page.waitForSelector('#new-dialog[open]');
  // Text, files and links are optional parts of one form; fill whichever the test wants.
  if (o.text) {
    await page.$eval('#text-input', (el, t) => { el.value = t; el.dispatchEvent(new Event('input')); }, o.text);
    if (o.language) await page.selectOption('#language', o.language);
  }
  if (o.files) await page.setInputFiles('#file-input', o.files);
  if (o.link) await page.fill('#link-input', o.link);
  if (o.title) await page.fill('#title', o.title);
  if (!o.keepPublicDefault) await page.setChecked('#public', !!o.public);
  await page.setChecked('#burn', !!o.burn);
  if (o.password) await page.fill('#share-password', o.password);
  if (o.maxDownloads) await page.fill('#max-downloads', String(o.maxDownloads));
  if (o.expiry) await page.selectOption('#expiry', String(o.expiry));
  if (o.shot) await shot(page, o.shot);
  await page.click('#create-btn');
  await page.waitForFunction(() => !document.getElementById('new-dialog').open || !document.getElementById('new-error').hidden,
    null, { timeout: o.timeout || 120000, polling: 250 });
  const err = await page.$eval('#new-error', (e) => (e.hidden ? '' : e.textContent));
  if (err) throw new Error('create failed: ' + err);
  const list = await page.evaluate(() => CP.api('GET', '/api/shares?status=active'));
  const share = list.shares[0];
  created.push(share.slug);
  return share;
}

async function visitorGet(ctx, url) {
  const res = await ctx.request.get(url, { maxRedirects: 0 });
  return { status: res.status(), body: await res.body(), headers: res.headers() };
}

async function run() {
  console.log(`cp e2e against ${BASE}`);
  const browser = await chromium.launch();
  const ownerCtx = await browser.newContext({ viewport: { width: 1360, height: 900 }, acceptDownloads: true });
  await ownerCtx.grantPermissions(['clipboard-read', 'clipboard-write'], { origin: BASE });
  const page = await ownerCtx.newPage();
  ownerPage = page;
  const visitor = await browser.newContext(); // a different browser: no cookies, never logged in
  const vpage = await visitor.newPage();

  step('access control');
  await vpage.goto(`${BASE}/`);
  check(vpage.url().endsWith('/login'), 'anonymous visitor is sent to the login page');
  await shot(vpage, '01-login');
  check((await visitorGet(visitor, `${BASE}/api/shares`)).status === 401, 'API refuses anonymous calls');
  check((await visitorGet(visitor, `${BASE}/settings`)).status === 303, 'settings page needs login');
  check(!(await login(vpage, USER, 'definitely-wrong')), 'wrong password is refused');
  check(await vpage.isVisible('.error'), 'login error is shown');
  check(await login(page), 'owner logs in');
  await page.waitForSelector('#usage-text:not(:text("…"))');
  await shot(page, '02-dashboard-empty');

  step('private by default');
  await page.click('#new-share');
  await page.waitForSelector('#new-dialog[open]');
  check(!(await page.isChecked('#public')), 'the Public link box starts unticked');
  await page.click('#new-dialog footer [data-close]');
  const def = await newShare(page, { kind: 'text', text: 'default visibility', keepPublicDefault: true });
  check(!def.public, 'a share made without touching the box is private');
  check((await visitorGet(visitor, `${BASE}/s/${def.slug}`)).status === 404, 'visitors cannot open it');

  step('text share: 1,000,000 lines through chunked upload');
  const lines = Array.from({ length: 1_000_000 }, (_, i) => `line ${i}: key_${i % 97}: value`).join('\n');
  const t0 = Date.now();
  const text = await newShare(page, { kind: 'text', text: lines, title: 'million lines', public: true, shot: '03-new-text' });
  check(text.size === Buffer.byteLength(lines), `stored ${(text.size / MB).toFixed(1)} MB of text in ${((Date.now() - t0) / 1000).toFixed(1)}s`);
  check(text.slug.length === 22 && /^[A-Za-z0-9]+$/.test(text.slug), 'link id is 22 random alphanumerics');
  const clip = await page.evaluate(() => navigator.clipboard.readText());
  check(clip.endsWith(`/s/${text.slug}`), 'new share link was copied to the clipboard');
  check(text.url === `${env.CP_BASE_URL}/s/${text.slug}`, 'share links use the public base URL');
  await vpage.goto(`${BASE}/s/${text.slug}`);
  check(await vpage.isVisible('text=million lines'), 'visitor sees the shared text page');
  check(await vpage.isVisible('text=preview truncated'), 'huge text shows a truncated preview');
  check(!(await vpage.isVisible('a[href="/settings"]')) && !(await vpage.isVisible('#shares')), 'visitor sees only this share, no dashboard');
  await shot(vpage, '04-visitor-text');
  const raw = await visitorGet(visitor, `${BASE}/r/${text.slug}`);
  check(raw.body.toString() === lines, 'raw download returns all 1,000,000 lines intact');

  await page.goto(`${BASE}/`);
  await page.waitForSelector(`.share-row[data-slug="${def.slug}"]`);
  check((await page.textContent(`.share-row[data-slug="${def.slug}"] .share-title`)).trim() === `Paste #${def.number}`, 'an untitled paste is listed as "Paste #N"');

  step('YAML with highlighting');
  const yaml = 'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: demo\nspec:\n  replicas: 2\n';
  const y = await newShare(page, { kind: 'text', text: yaml, language: 'yaml', title: 'deploy.yaml', public: true });
  await vpage.goto(`${BASE}/s/${y.slug}`);
  check(await vpage.isVisible('.chroma'), 'YAML is syntax highlighted');
  await shot(vpage, '05-visitor-yaml');

  step('file share: several files, chunked, byte-exact');
  const bigSize = process.env.BIG ? 100 * MB : 25 * MB;
  const bin = fixture(`random-${bigSize / MB}mb.bin`, bigSize);
  const pdf = path.join(FIX, 'notes.pdf');
  fs.writeFileSync(pdf, '%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj 2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj 3 0 obj<</Type/Page/MediaBox[0 0 200 200]/Parent 2 0 R>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n');
  const t1 = Date.now();
  const files = await newShare(page, { kind: 'file', files: [bin, pdf], public: true, timeout: 900000, shot: '06-new-files' });
  check(files.files.length === 2, `2 files stored (${bigSize / MB} MB in ${((Date.now() - t1) / 1000).toFixed(1)}s)`);
  const dl = await visitorGet(visitor, `${BASE}/d/${files.slug}/${files.files[0].id}`);
  check(dl.status === 200 && sha(dl.body) === sha(fs.readFileSync(bin)), 'visitor download is byte-identical (sha256)');
  check((dl.headers['content-disposition'] || '').startsWith('attachment'), 'files download as attachments');
  const zip = await visitorGet(visitor, `${BASE}/z/${files.slug}`);
  check(zip.status === 200 && zip.body.subarray(0, 2).toString() === 'PK', 'download-all gives a zip');
  await vpage.goto(`${BASE}/s/${files.slug}`);
  check(await vpage.isVisible('text=Download all (.zip)'), 'file page lists both files');
  await shot(vpage, '07-visitor-files');
  const tooBig = await page.evaluate(() => CP.api('POST', '/api/uploads', { name: 'x', size: 100 * 1048576 + 1 }).then(() => 'accepted', (e) => e.message));
  check(tooBig !== 'accepted', 'a file over 100 MB is refused');

  step('link share: many links at once');
  const urls = ['https://example.com/docs?page=2', 'https://example.org/a', 'https://example.net/b/c'];
  const link = await newShare(page, { kind: 'link', link: urls.join('\n'), public: true });
  check(link.links.length === 3, 'one share holds all 3 links');
  await vpage.goto(`${BASE}/s/${link.slug}`);
  let allShown = true;
  for (const u of urls) allShown = allShown && (await vpage.isVisible(`a.link-url:text-is("${u}")`));
  check(allShown, 'visitor sees every link');
  check((await vpage.locator('.link-list a.link-url').count()) === 3 && (await vpage.locator('a:text-is("Open ↗"), button:text-is("Open")').count()) === 0, 'each link is clickable itself, no Open buttons');
  check(((await vpage.getAttribute('.link-list a', 'rel')) || '').includes('noreferrer'), 'links open without leaking a referrer');
  await shot(vpage, '07b-visitor-links');

  step('one share with text, a file and links together');
  const txt = path.join(FIX, 'readme.txt');
  fs.writeFileSync(txt, 'hello from a text file\n');
  const mixed = await newShare(page, { text: 'deploy notes\nline 2', files: [pdf, txt], link: 'https://example.com/m1\nhttps://example.org/m2', public: true, shot: '07c-new-mixed' });
  check(!!mixed.text && mixed.files.length === 2 && mixed.links.length === 2, 'one share holds the text, the files and both links');
  await vpage.goto(`${BASE}/s/${mixed.slug}`);
  check((await vpage.isVisible('text=deploy notes')) && (await vpage.isVisible('text=notes.pdf')) &&
    (await vpage.isVisible('a.link-url:text-is("https://example.com/m1")')) && (await vpage.isVisible('a.link-url:text-is("https://example.org/m2")')),
    'visitor sees the text, the file and every link on one page');
  check(await vpage.isVisible(`h1:text-is("Share #${mixed.number}")`), `a mixed share is headed "Share #${mixed.number}"`);
  check(await vpage.isVisible(`.share-head a[href="/z/${mixed.slug}"]`), 'Download all (.zip) sits at the top, not inside the files block');
  await shot(vpage, '07d-visitor-mixed');
  await page.goto(`${BASE}/`);
  await page.waitForSelector(`.share-row[data-slug="${mixed.slug}"]`);
  check((await page.textContent(`.share-row[data-slug="${mixed.slug}"] .contents`)) === '📝 text · 📄 2 files · 🔗 2 links', 'the dashboard row says what is inside');
  await page.click(`.share-row[data-slug="${mixed.slug}"] .share-title`);
  await page.waitForFunction(() => document.getElementById('view-dialog').open);
  check(await page.isVisible(`#view-body .share-bar a[href="/z/${mixed.slug}"]`), 'in the popup, Download all (.zip) is at the top');
  await page.click('#view-body button[data-name="readme.txt"]');
  await page.waitForFunction(() => { const d = document.getElementById('file-preview'); return d && d.open; });
  check((await page.textContent('#fp-body pre')).includes('hello from a text file'), 'previewing a .txt file shows its text (no white screen)');
  await shot(page, '07e-file-preview');
  await page.keyboard.press('Escape');
  await page.waitForTimeout(200);
  check(!(await page.evaluate(() => document.getElementById('file-preview').open)) && (await page.evaluate(() => document.getElementById('view-dialog').open)),
    'Esc closes only the preview, the share popup stays open');
  await page.click('#view-body button[data-name="notes.pdf"]');
  await page.waitForFunction(() => document.getElementById('file-preview').open);
  check((await page.locator('#fp-body iframe').count()) === 1, 'a PDF previews in the built-in viewer');
  await page.mouse.click(4, 4);
  await page.waitForTimeout(200);
  check(!(await page.evaluate(() => document.getElementById('file-preview').open)) && (await page.evaluate(() => document.getElementById('view-dialog').open)),
    'clicking outside closes only the preview');
  await page.keyboard.press('Escape');

  step('private, password, burn-after-reading, download limit');
  const priv = await newShare(page, { kind: 'text', text: 'private note', public: false });
  check((await visitorGet(visitor, `${BASE}/s/${priv.slug}`)).status === 404, 'private share is a plain 404 for visitors');
  await page.goto(`${BASE}/s/${priv.slug}`);
  check(await page.isVisible('text=private note'), 'owner still opens the private share');
  await page.goto(`${BASE}/`);
  const pw = await newShare(page, { kind: 'text', text: 'behind a password', public: true, password: 'open-sesame' });
  await vpage.goto(`${BASE}/s/${pw.slug}`);
  check((await vpage.isVisible('text=Password required')) && !(await vpage.isVisible('text=behind a password')), 'password share asks first');
  await vpage.fill('input[name=password]', 'nope');
  await vpage.click('button[type=submit]');
  check(await vpage.isVisible('text=Wrong password'), 'wrong share password refused');
  await vpage.fill('input[name=password]', 'open-sesame');
  await vpage.click('button[type=submit]');
  check(await vpage.isVisible('text=behind a password'), 'right share password unlocks');
  const burn = await newShare(page, { kind: 'text', text: 'read me once', public: true, burn: true });
  await vpage.goto(`${BASE}/s/${burn.slug}`);
  await shot(vpage, '08-burn-confirm');
  await vpage.click('text=Reveal now');
  check(await vpage.isVisible('text=read me once'), 'first visitor reveals the burn share');
  const second = await browser.newContext();
  check((await visitorGet(second, `${BASE}/s/${burn.slug}`)).status === 404, 'a second visitor gets nothing');
  await second.close();
  const once = await newShare(page, { kind: 'text', text: 'one download', public: true, maxDownloads: 1 });
  const d1 = await visitorGet(visitor, `${BASE}/r/${once.slug}`);
  const d2 = await visitorGet(visitor, `${BASE}/r/${once.slug}`);
  check(d1.status === 200 && d2.status === 404, 'max downloads = 1 allows exactly one download');

  step('dashboard actions');
  await page.goto(`${BASE}/`);
  await page.waitForSelector(`.share-row[data-slug="${y.slug}"]`);
  await shot(page, '09-dashboard-list');
  check((await page.locator('.share-row [target], .share-row :text-is("Open")').count()) === 0, 'no Open button or new-tab link on the rows');
  const row = page.locator(`.share-row[data-slug="${y.slug}"]`);
  const viewOpen = () => page.evaluate(() => document.getElementById('view-dialog').open);
  const openRow = async (r) => { await r.locator('.share-title').click(); await page.waitForFunction(() => document.getElementById('view-dialog').open); };
  await openRow(row);
  check(await page.isVisible('#view-body .chroma') && (await page.isVisible('#view-body >> text=replicas')), 'clicking a row opens it in a popup on the same page');
  check(page.url() === `${BASE}/` && ownerCtx.pages().length === 1 && (await page.locator('#view-dialog iframe').count()) === 0, 'no new page, tab or frame was opened');
  check((await page.textContent('#view-title')).includes('deploy.yaml'), 'popup header shows the share name');
  await shot(page, '09b-view-popup');
  await page.keyboard.press('Escape');
  check(!(await viewOpen()), 'Esc closes the popup');
  await openRow(row);
  await page.mouse.click(8, 8); // the dimmed area outside the popup
  await page.waitForTimeout(300);
  check(!(await viewOpen()), 'clicking outside closes the popup');
  const big = page.locator(`.share-row[data-slug="${text.slug}"]`);
  await openRow(big);
  await page.click('#view-body [data-show-all]');
  await page.waitForFunction(() => !document.querySelector('#view-body [data-show-all]'), null, { timeout: 60000 });
  const fullLen = await page.evaluate(() => document.querySelector('#view-body #code').textContent.length);
  check(fullLen === lines.length && ownerCtx.pages().length === 1, 'Show all loads the full million lines inside the popup');
  await page.keyboard.press('Escape');
  await row.locator('[data-act=copy]').click();
  await page.waitForTimeout(200);
  check((await page.evaluate(() => navigator.clipboard.readText())).endsWith(`/s/${y.slug}`), 'Copy link copies the share URL');
  await row.locator('[data-act=qr]').click();
  const qrOk = await page.waitForFunction(() => document.getElementById('qr-img').naturalWidth > 0, null, { timeout: 10000 }).then(() => true, () => false);
  check(qrOk, 'QR code renders');
  await shot(page, '10-qr');
  await page.click('#qr-dialog button');
  await row.locator('summary').click();
  await row.locator('[data-act=regenerate]').click();
  await row.locator('[data-act=regenerate]').click();
  await page.waitForFunction((s) => !document.querySelector(`.share-row[data-slug="${s}"]`), y.slug);
  check((await visitorGet(visitor, `${BASE}/s/${y.slug}`)).status === 404, 'New link kills the old URL');
  const newSlug = (await page.evaluate(() => navigator.clipboard.readText())).split('/s/')[1];
  check((await visitorGet(visitor, `${BASE}/s/${newSlug}`)).status === 200, 'the new URL works');
  created.push(newSlug);
  const frow = page.locator(`.share-row[data-slug="${files.slug}"]`);
  const fbadges = await frow.locator('.badges').textContent();
  check(fbadges.includes('⬇ 2 downloads') && !/\bdl\b/.test(fbadges), 'downloads read "⬇ 2 downloads", not "2 dl"');
  await frow.locator('summary').click();
  await frow.locator('[data-act=edit]').click();
  await page.waitForFunction(() => document.getElementById('edit-dialog').open);
  check((await page.getAttribute('#edit-title', 'placeholder')).includes('random-'), 'rename box shows the automatic name it falls back to');
  await page.fill('#edit-title', 'HKS behind proxy (test)');
  await page.click('#edit-form button[type=submit]');
  await page.waitForFunction((s) => (document.querySelector(`.share-row[data-slug="${s}"] .share-title`) || {}).textContent?.startsWith('HKS behind proxy (test)'), files.slug);
  check(true, 'a share can be renamed from the ⋯ menu');
  await page.click('#new-share');
  await page.waitForFunction(() => document.getElementById('new-dialog').open);
  await page.setInputFiles('#file-input', [pdf]);
  check((await page.getAttribute('#title', 'placeholder')) === 'Leave empty to use: notes.pdf', 'the Name box at the top suggests the automatic name');
  await page.click('#new-dialog footer [data-close]');
  const menuOpen = () => page.evaluate(() => !!document.querySelector('details.more[open]'));
  const firstMenu = page.locator('.share-row details.more summary').first();
  await firstMenu.click();
  check(await menuOpen(), 'the ⋯ menu opens');
  await page.keyboard.press('Escape');
  check(!(await menuOpen()), 'Esc closes the ⋯ menu');
  await firstMenu.click();
  await page.click('.drop-hint');
  check(!(await menuOpen()), 'clicking outside closes the ⋯ menu');
  await firstMenu.click();
  await page.locator('.share-row details.more summary').last().click(); // a row the first menu does not cover
  check((await page.locator('details.more[open]').count()) === 1, 'opening another ⋯ menu closes the first');
  await page.keyboard.press('Escape');
  const lrow = page.locator(`.share-row[data-slug="${link.slug}"]`);
  await lrow.locator('summary').click();
  await lrow.locator('[data-act=toggle-public]').click();
  await page.waitForTimeout(500);
  check((await visitorGet(visitor, `${BASE}/s/${link.slug}`)).status === 404, 'Make private hides it from visitors at once');
  await page.fill('#search', 'million');
  await page.waitForTimeout(800);
  const hits = await page.locator('.share-row').evaluateAll((rows) => rows.map((r) => r.dataset.slug));
  check(hits.includes(text.slug) && !hits.includes(link.slug) && !hits.includes(files.slug), 'search filters the list');
  await page.fill('#search', '');
  await page.waitForTimeout(800);
  const prow = page.locator(`.share-row[data-slug="${priv.slug}"]`);
  await prow.locator('summary').click();
  await prow.locator('[data-act=delete]').click();
  await prow.locator('[data-act=delete]').click();
  await page.waitForFunction((s) => !document.querySelector(`.share-row[data-slug="${s}"]`), priv.slug);
  check((await page.evaluate((s) => fetch('/s/' + s).then((r) => r.status), priv.slug)) !== 200, 'Delete removes the share');

  step('paste anywhere opens a new share');
  await page.evaluate(() => navigator.clipboard.writeText('pasted from clipboard'));
  await page.click('.drop-hint');
  await page.keyboard.press('Control+V');
  await page.waitForTimeout(300);
  check((await page.isVisible('#new-dialog[open]')) && (await page.inputValue('#text-input')) === 'pasted from clipboard', 'Ctrl+V on the dashboard pre-fills a text share');
  await page.click('#new-dialog footer [data-close]').catch(() => {});

  step('settings');
  await page.goto(`${BASE}/settings`);
  await page.waitForFunction(() => document.getElementById('un-new').value !== '');
  await shot(page, '11-settings');
  await page.uncheck('#s-public-sharing');
  await page.click('#settings-form button[type=submit]');
  await page.waitForSelector('#settings-msg:text("Saved")');
  check((await visitorGet(visitor, `${BASE}/s/${text.slug}`)).status === 404, 'global switch off hides every public link');
  await page.check('#s-public-sharing');
  await page.click('#settings-form button[type=submit]');
  await page.waitForTimeout(500);
  check((await visitorGet(visitor, `${BASE}/s/${text.slug}`)).status === 200, 'global switch on restores them');
  const temp = 'Temp-' + crypto.randomBytes(9).toString('hex');
  await page.fill('#pw-current', PASS);
  await page.fill('#pw-new', temp);
  await page.fill('#pw-repeat', temp);
  await page.click('#password-form button[type=submit]');
  await page.waitForSelector('#pw-msg:text("Password changed")');
  tempPassword = temp;
  const other = await browser.newContext();
  const opage = await other.newPage();
  check(!(await login(opage, USER, PASS)), 'old password stops working');
  check(await login(opage, USER, temp), 'new password works');
  await other.close();
  await page.fill('#pw-current', temp);
  await page.fill('#pw-new', PASS);
  await page.fill('#pw-repeat', PASS);
  await page.click('#password-form button[type=submit]');
  await page.waitForSelector('#pw-msg:text("Password changed")');
  tempPassword = null;
  check(true, 'password restored to the .env value');

  step('look and feel');
  await page.goto(`${BASE}/`);
  await page.click('#theme-toggle');
  check((await page.getAttribute('html', 'data-theme')) === 'dark', 'dark mode toggles');
  await page.waitForSelector('.share-row');
  await shot(page, '12-dashboard-dark');
  await page.click('#theme-toggle');
  const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true });
  const ppage = await phone.newPage();
  await ppage.goto(`${BASE}/s/${text.slug}`);
  const overflow = await ppage.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
  check(!overflow, 'share page fits a phone screen without sideways scrolling');
  await shot(ppage, '13-phone-share');
  await phone.close();

  step('security headers');
  const h = (await visitorGet(visitor, `${BASE}/login`)).headers;
  check(h['referrer-policy'] === 'same-origin', 'Referrer-Policy: same-origin (share URLs never sent to other sites)');
  check((h['content-security-policy'] || '').includes("default-src 'self'"), 'strict Content-Security-Policy');
  check(h['x-robots-tag'] === 'noindex, nofollow', 'search engines told not to index');

  step('cleanup');
  await page.goto(`${BASE}/`);
  for (const slug of created) {
    const res = await page.evaluate((s) => CP.api('DELETE', '/api/shares/' + s).then(() => 'ok', (e) => e.message), slug);
    if (res !== 'ok' && !/not found/.test(res)) console.log(`  delete ${slug.slice(0, 6)}…: ${res}`);
  }
  const left = await page.evaluate(() => CP.api('GET', '/api/shares?status=active'));
  check(!left.shares.some((x) => created.includes(x.slug)), 'all test shares deleted (your own shares are left alone)');
  await browser.close();
  console.log(`\n${failed ? failed + ' check(s) FAILED' : 'all checks passed'}`);
  process.exit(failed ? 1 : 0);
}

run().catch(async (e) => { console.error('FATAL', e.message); await emergencyCleanup(); process.exit(2); });
