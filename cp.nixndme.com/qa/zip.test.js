// Unit tests for web/static/zip.js (the streaming ZIP writer used by Live Transfer).
// Runs with plain Node: `node --test qa/`. Archives are checked with Python's zipfile and unzip.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');
const { execFileSync } = require('child_process');
const CPZip = require('../web/static/zip.js');

// Writes a whole archive the same way the browser worker does: plan, then for each entry a
// local header, the data, and a CRC patch; finally the central directory.
function buildZip(files, opts) {
  const plan = CPZip.plan(files.map((f) => ({ path: f.path, size: f.data.length, mtime: f.mtime || 0 })), opts);
  const out = Buffer.alloc(plan.total);
  plan.entries.forEach((e, i) => {
    let crc = CPZip.crc32(0, new Uint8Array(0));
    const data = files[i].data;
    for (let o = 0; o < data.length; o += 7) crc = CPZip.crc32(crc, data.subarray(o, o + 7)); // streamed
    Buffer.from(CPZip.localHeader(e, 0)).copy(out, e.offset);
    data.copy(out, e.dataOffset);
    Buffer.from(CPZip.crcPatch(crc)).copy(out, e.offset + CPZip.CRC_AT);
    e.crc = crc;
  });
  Buffer.from(CPZip.centralDirectory(plan)).copy(out, plan.cdOffset);
  return out;
}

function pyList(zipPath) {
  const py = 'import sys,zipfile,json,hashlib\nz=zipfile.ZipFile(sys.argv[1])\nassert z.testzip() is None\n' +
    'print(json.dumps({i.filename:[i.file_size,hashlib.sha256(z.read(i)).hexdigest()] for i in z.infolist()}))';
  return JSON.parse(execFileSync('python3', ['-I', '-c', py, zipPath]).toString());
}

function tmp(name, buf) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'cpzip-'));
  const p = path.join(dir, name);
  fs.writeFileSync(p, buf);
  return p;
}

const sha = (b) => crypto.createHash('sha256').update(b).digest('hex');

test('crc32 matches the standard check value', () => {
  assert.strictEqual(CPZip.crc32(0, new TextEncoder().encode('123456789')), 0xcbf43926);
});

for (const forceZip64 of [false, true]) {
  test(`archive with folders and unicode names is valid (zip64=${forceZip64})`, () => {
    const files = [
      { path: 'holiday/readme.txt', data: Buffer.from('hello\n'), mtime: Date.UTC(2026, 9, 31, 12, 30) },
      { path: 'holiday/photos/ünïcødé 写真.jpg', data: crypto.randomBytes(100003) },
      { path: 'holiday/empty.bin', data: Buffer.alloc(0) },
    ];
    const zipPath = tmp('t.zip', buildZip(files, { zip64: forceZip64 }));
    const got = pyList(zipPath);
    for (const f of files) assert.deepStrictEqual(got[f.path], [f.data.length, sha(f.data)], f.path);
    execFileSync('unzip', ['-tqq', zipPath]); // Info-ZIP agrees too
  });
}

test('plan switches to zip64 by itself for files of 4 GiB and more', () => {
  const plan = CPZip.plan([{ path: 'a', size: 1 }, { path: 'big.iso', size: 5 * 2 ** 30 }, { path: 'c', size: 2 }]);
  assert.strictEqual(plan.entries[0].zip64, false);
  assert.strictEqual(plan.entries[1].zip64, true);
  assert.ok(plan.entries[2].offset > 0xffffffff, 'entry after a 5 GiB file starts past 4 GiB');
  assert.strictEqual(plan.zip64, true);
  assert.strictEqual(plan.total, plan.cdOffset + CPZip.centralDirectory(plan).length);
});

test('safePath strips traversal and characters other systems reject', () => {
  assert.strictEqual(CPZip.safePath('../../etc/passwd'), 'etc/passwd');
  assert.strictEqual(CPZip.safePath('/abs//x/./y'), 'abs/x/y');
  assert.strictEqual(CPZip.safePath('a\\b\\c.txt'), 'a/b/c.txt');
  assert.strictEqual(CPZip.safePath('bad:name?.txt'), 'bad_name_.txt');
  assert.strictEqual(CPZip.safePath('ctl\u0001.txt'), 'ctl_.txt');
  assert.strictEqual(CPZip.safePath('../..'), 'file');
  assert.strictEqual(CPZip.safePath(' trailing. '), 'trailing');
});
