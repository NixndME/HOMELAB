// Live Transfer receiver, browser-storage mode (Safari, Firefox, phones). Writes incoming bytes
// into the Origin Private File System with synchronous access handles, which every current
// browser supports in workers. Nothing is held in memory or copied twice.
//  - loose files (videos, zips…): each file is stored as itself and saved on its own
//  - a folder: one ZIP written in place, so its layout survives
// Messages are handled strictly in order.
'use strict';
importScripts('zip.js' + self.location.search); // same ?v= as this file: never a stale copy

const DIR = 'live';
const REPORT_EVERY = 4 << 20; // tell the page how far we got every 4 MB (it acks the sender)

// { base, zip, plan, files, handle, pos, crc, written, reported }
let st = null;
let broken = false;
let chain = Promise.resolve();

self.onmessage = (ev) => {
  chain = chain.then(() => (broken ? null : run(ev.data))).catch(fail);
};

function fail(err) {
  broken = true;
  const quota = err && (err.name === 'QuotaExceededError' || /quota/i.test(String(err.message)));
  self.postMessage({ op: 'error', message: quota ? 'This device ran out of browser storage.' : String((err && err.message) || err) });
  cleanup();
}

async function liveDir() {
  const root = await navigator.storage.getDirectory();
  return root.getDirectoryHandle(DIR, { create: true });
}

/** Closes the open handle and deletes everything this transfer wrote. */
async function cleanup() {
  if (!st) return;
  const { handle, base } = st;
  st = null;
  try { if (handle) await handle.close(); } catch (e) { /* already closed */ }
  try {
    const dir = await liveDir();
    const names = [];
    for await (const name of dir.keys()) if (name.startsWith(base)) names.push(name);
    for (const name of names) await dir.removeEntry(name);
  } catch (e) { /* the page sweeps leftovers on its next visit */ }
}

function report(force) {
  if (force || st.written - st.reported >= REPORT_EVERY) {
    st.reported = st.written;
    self.postMessage({ op: 'progress', written: st.written });
  }
}

function writeAt(bytes, at) {
  const n = st.handle.write(bytes, { at });
  if (n !== bytes.length) throw new Error('short write (disk full?)');
}

// Sync access handle methods were async in older Safari; awaiting them works everywhere.
async function openHandle(name, size) {
  const fh = await (await liveDir()).getFileHandle(name, { create: true });
  const handle = await fh.createSyncAccessHandle();
  st.handle = handle;
  // Claim the full size now, so a device without room fails at once rather than at 90 %.
  await handle.truncate(size);
}

async function run(m) {
  switch (m.op) {
    case 'open':
      st = { base: m.name, zip: m.zip, files: m.files, plan: m.zip ? CPZip.plan(m.files) : null,
        handle: null, pos: 0, crc: 0, written: 0, reported: 0 };
      if (st.zip) await openHandle(st.base, st.plan.total);
      self.postMessage({ op: 'opened' });
      return;
    case 'begin':
      if (st.zip) {
        const e = st.plan.entries[m.i];
        writeAt(CPZip.localHeader(e, 0), e.offset);
        st.pos = e.dataOffset;
        st.crc = 0;
      } else {
        await openHandle(`${st.base}-${m.i}`, st.files[m.i].size);
        st.pos = 0;
      }
      return;
    case 'data': {
      const bytes = new Uint8Array(m.buf);
      writeAt(bytes, st.pos);
      st.pos += bytes.length;
      if (st.zip) st.crc = CPZip.crc32(st.crc, bytes);
      st.written += bytes.length;
      report(false);
      return;
    }
    case 'end':
      if (st.zip) {
        const e = st.plan.entries[m.i];
        e.crc = st.crc;
        writeAt(CPZip.crcPatch(st.crc), e.offset + CPZip.CRC_AT);
      } else {
        await st.handle.flush();
        await st.handle.close();
        st.handle = null;
      }
      report(true);
      return;
    case 'finish':
      if (st.zip) {
        writeAt(CPZip.centralDirectory(st.plan), st.plan.cdOffset);
        await st.handle.flush();
        await st.handle.close();
      }
      st = null;
      self.postMessage({ op: 'done' });
      return;
    case 'abort':
      await cleanup();
      return;
    default:
      throw new Error('unknown op ' + m.op);
  }
}
