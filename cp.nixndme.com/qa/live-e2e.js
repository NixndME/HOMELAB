// End-to-end check of Live Transfer between real browser engines (Playwright): one page sends,
// another receives over WebRTC, and the saved bytes are compared with the originals.
// Firefox stands in for Zen, WebKit for Safari, Chromium for Opera/Chrome.
//
//   LIVE_URL=http://localhost:18090 LIVE_USER=… LIVE_PASS=… node live-e2e.js [MB]
//   LIVE_PAIRS=firefox:webkit,chromium:firefox   choose sender:receiver engine pairs
//
// Point it at a throwaway local instance (see README "Tests"); it never touches .env.
const { chromium, firefox, webkit } = require('playwright');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { execFileSync } = require('child_process');

const BASE = (process.env.LIVE_URL || 'http://localhost:18090').replace(/\/$/, '');
const USER = process.env.LIVE_USER;
const PASS = process.env.LIVE_PASS;
const BIG_MB = Number(process.argv[2] || 64);
const MB = 1 << 20;
const TIMEOUT = 10 * 60 * 1000;
const FOLDER_FILES = ['video/clip ü 1.bin', 'notes.txt', 'empty.dat'];
const MANY = 3000; // a file list far bigger than one data channel message

let failed = 0;
const check = (ok, what) => { console.log(`  ${ok ? 'OK  ' : 'FAIL'} ${what}`); if (!ok) failed++; };
const sha = (p) => crypto.createHash('sha256').update(fs.readFileSync(p)).digest('hex');

function writeRandom(p, bytes) {
  fs.mkdirSync(path.dirname(p), { recursive: true });
  const fd = fs.openSync(p, 'w');
  for (let left = bytes; left > 0; left -= Math.min(left, 4 * MB)) fs.writeSync(fd, crypto.randomBytes(Math.min(left, 4 * MB)));
  fs.closeSync(fd);
}

function fixtures(root) {
  const folder = path.join(root, 'holiday');
  writeRandom(path.join(folder, FOLDER_FILES[0]), BIG_MB * MB + 12345);
  writeRandom(path.join(folder, FOLDER_FILES[1]), 777);
  fs.writeFileSync(path.join(folder, FOLDER_FILES[2]), '');
  const single = path.join(root, 'single.iso');
  writeRandom(single, Math.ceil(BIG_MB / 2) * MB + 3);
  const videos = ['trip-1.mp4', 'trip 2.mov'].map((n, i) => {
    const p = path.join(root, 'videos', n);
    writeRandom(p, (i + 1) * 5 * MB + 17);
    return p;
  });
  const many = path.join(root, 'many');
  for (let i = 0; i < MANY; i++) {
    const p = path.join(many, `dir-${i % 30}`, `file-${i}.txt`);
    fs.mkdirSync(path.dirname(p), { recursive: true });
    fs.writeFileSync(p, `file ${i}\n`.repeat(1 + (i % 5)));
  }
  return { folder, single, videos, many };
}

// Both pages run on this machine; expose real host addresses instead of mDNS names.
// WebKit gets a normal (persistent) profile: like Safari's Private Browsing, its throwaway
// sessions have no file storage, which the receiver needs.
const engines = {
  firefox: () => firefox.launch({ firefoxUserPrefs: { 'media.peerconnection.ice.obfuscate_host_addresses': false } })
    .then((b) => b.newContext({ acceptDownloads: true })),
  webkit: () => webkit.launchPersistentContext(fs.mkdtempSync(path.join(os.tmpdir(), 'cp-live-wk-')), { acceptDownloads: true }),
  chromium: () => chromium.launch({ args: ['--disable-features=WebRtcHideLocalIpsWithMdns'] })
    .then((b) => b.newContext({ acceptDownloads: true })),
};
const closeCtx = (ctx) => (ctx.browser() ? ctx.browser().close() : ctx.close());

async function loggedIn(ctx) {
  const page = await ctx.newPage();
  page.on('pageerror', (e) => console.log('    page error:', e.message));
  await page.goto(BASE + '/login');
  await page.fill('input[name=username]', USER);
  await page.fill('input[name=password]', PASS);
  await Promise.all([page.waitForURL(BASE + '/'), page.click('button[type=submit]')]);
  return page;
}

const finished = () => (el) => /✅|❌/.test(document.getElementById(el).textContent);

async function transfer(sendName, recvName, input, what, verify) {
  console.log(`\n== ${what}: ${sendName} → ${recvName}`);
  let sb = null;
  let rb = null;
  try {
    [sb, rb] = await Promise.all([engines[sendName](), engines[recvName]()]);
    const sender = await loggedIn(sb);
    const receiver = await loggedIn(rb);
    await sender.goto(BASE + '/live');
    await receiver.goto(BASE + '/live?save=browser');

    await sender.setInputFiles(input.selector, input.files);
    await sender.click('#send-start');
    await receiver.click('#recv-offers button', { timeout: 15000 });

    const t0 = Date.now();
    await receiver.waitForFunction(finished(), 'recv-status', { timeout: TIMEOUT });
    const status = (await receiver.textContent('#recv-status')).trim();
    check(status.includes('✅'), `receiver: ${status}`);
    await sender.waitForFunction(finished(), 'send-status', { timeout: 30000 });
    const sstatus = (await sender.textContent('#send-status')).trim();
    check(sstatus.includes('✅'), `sender: ${sstatus}`);
    if (!status.includes('✅')) return;
    const secs = (Date.now() - t0) / 1000;

    const saved = [];
    const links = receiver.locator('#recv-saved a');
    for (let i = 0; i < (await links.count()); i++) {
      const [dl] = await Promise.all([receiver.waitForEvent('download'), links.nth(i).click()]);
      const out = path.join(os.tmpdir(), `cp-live-${Date.now()}-${i}-${dl.suggestedFilename()}`);
      await dl.saveAs(out);
      saved.push({ name: dl.suggestedFilename(), path: out });
    }
    const mb = saved.reduce((n, d) => n + fs.statSync(d.path).size, 0) / MB;
    console.log(`    ${mb.toFixed(1)} MB in ${secs.toFixed(1)} s (${(mb / secs).toFixed(1)} MB/s incl. connecting)`);
    verify(saved);
    await receiver.click('#recv-clear');
    saved.forEach((d) => fs.rmSync(d.path, { force: true }));
  } catch (e) {
    check(false, `${what} crashed: ${e.message.split('\n')[0]}`);
  } finally {
    await Promise.all([sb && closeCtx(sb), rb && closeCtx(rb)]);
  }
}

function verifyFolder(src) {
  return (saved) => {
    check(saved.length === 1 && saved[0].name === 'holiday.zip', `one download named ${saved.map((d) => d.name)}`);
    const zipPath = saved[0].path;
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cp-live-unzip-'));
    execFileSync('unzip', ['-qq', zipPath, '-d', dir]);
    for (const rel of FOLDER_FILES) {
      const b = path.join(dir, 'holiday', rel);
      check(fs.existsSync(b) && sha(path.join(src, rel)) === sha(b), `zip holds holiday/${rel} byte-identical`);
    }
    fs.rmSync(dir, { recursive: true, force: true });
  };
}

function verifyMany(src) {
  return (saved) => {
    check(saved.length === 1 && saved[0].name === 'many.zip', `one download named ${saved.map((d) => d.name)}`);
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cp-live-unzip-'));
    execFileSync('unzip', ['-qq', saved[0].path, '-d', dir]);
    let ok = 0;
    for (let i = 0; i < MANY; i++) {
      const rel = path.join(`dir-${i % 30}`, `file-${i}.txt`);
      const b = path.join(dir, 'many', rel);
      if (fs.existsSync(b) && sha(path.join(src, rel)) === sha(b)) ok++;
    }
    check(ok === MANY, `${ok} of ${MANY} small files byte-identical`);
    fs.rmSync(dir, { recursive: true, force: true });
  };
}

// Loose files (e.g. several videos) must arrive as separate downloads, not a zip.
function verifyLoose(srcs) {
  return (saved) => {
    check(saved.length === srcs.length, `${saved.length} separate downloads for ${srcs.length} files`);
    for (const src of srcs) {
      const d = saved.find((x) => x.name === path.basename(src));
      check(!!d && sha(d.path) === sha(src), `${path.basename(src)} is byte-identical`);
    }
  };
}

(async () => {
  if (!USER || !PASS) { console.error('set LIVE_USER and LIVE_PASS'); process.exit(2); }
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'cp-live-fix-'));
  const fx = fixtures(root);
  const pairs = (process.env.LIVE_PAIRS || 'firefox:webkit,webkit:firefox,firefox:chromium,chromium:webkit').split(',');
  for (const pair of pairs) {
    const [s, r] = pair.split(':');
    await transfer(s, r, { selector: '#pick-folder', files: fx.folder }, 'folder', verifyFolder(fx.folder));
  }
  await transfer('firefox', 'webkit', { selector: '#pick-files', files: [fx.single] }, 'single file', verifyLoose([fx.single]));
  await transfer('webkit', 'chromium', { selector: '#pick-files', files: fx.videos }, 'two videos', verifyLoose(fx.videos));
  await transfer('firefox', 'webkit', { selector: '#pick-folder', files: fx.many }, `${MANY}-file folder`, verifyMany(fx.many));
  fs.rmSync(root, { recursive: true, force: true });
  console.log(failed ? `\n${failed} check(s) failed` : '\nall live transfer checks passed');
  process.exit(failed ? 1 : 0);
})();
