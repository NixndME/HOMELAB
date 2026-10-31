// Where a Live Transfer lands on the receiving device.
//  - Chrome / Edge / Opera (desktop): you pick a folder (or a file name) and bytes are written
//    straight into it as they arrive.
//  - Safari, Firefox, phones: bytes go into the browser's private storage (a worker writes them),
//    then you tap Save. Several files arrive as one .zip that keeps the folder layout.
// Every sink has the same shape: begin(i), write(bytes), end(i), finish() -> result, abort().
'use strict';

const LiveSink = (() => {
  const VERSION = new URL(document.currentScript.src).search; // ?v=… of this build
  const OPFS_DIR = 'live';
  const STALE_MS = 60 * 60 * 1000;

  const canPickFolder = () => typeof window.showDirectoryPicker === 'function';
  const canPickFile = () => typeof window.showSaveFilePicker === 'function';
  const forceBrowser = () => new URLSearchParams(location.search).get('save') === 'browser';

  function mode(offer) {
    if (forceBrowser()) return 'opfs';
    if (!offer.folder && offer.files === 1 && canPickFile()) return 'file';
    if (canPickFolder()) return 'dir';
    return 'opfs';
  }

  /** One line telling the user what will happen on this device. */
  function describe() {
    if (!forceBrowser() && canPickFolder()) return 'You choose where to save; files are written there directly as they arrive.';
    return 'This browser stores the transfer first, then you tap Save for each file (a folder arrives as one .zip). ' +
      'Needs free space for about twice the size until you tap “Done”.';
  }

  /** Asks where to save. Must run inside the click handler (browsers require a user gesture). */
  async function choose(offer) {
    const m = mode(offer);
    if (m === 'file') return { kind: 'file', handle: await window.showSaveFilePicker({ suggestedName: CPZip.safePath(offer.name).split('/').pop() }) };
    if (m === 'dir') return { kind: 'dir', dir: await window.showDirectoryPicker({ mode: 'readwrite', id: 'cp-live' }) };
    await checkStorage();
    return { kind: 'opfs' };
  }

  /** Private windows (Safari especially) have no file storage at all: say so before connecting. */
  async function checkStorage() {
    try {
      await navigator.storage.getDirectory();
    } catch (e) {
      throw new Error('This browser cannot store big files here (private window?). Open cp in a normal window and try again.');
    }
  }

  async function open(target, files, offerName, onWritten) {
    if (target.kind === 'opfs') return openBrowserStorage(files, offerName, onWritten);
    if (target.kind === 'file' && files.length !== 1) throw new Error('Expected one file but the sender offered several.');
    return openPicked(target, files, onWritten);
  }

  // ---------- picked folder or file (File System Access API) ----------

  /** A new file in dir: "name.ext", or "name (1).ext" etc. when that exists (never overwrite). */
  async function freeName(dir, name) {
    const dot = name.lastIndexOf('.');
    const stem = dot > 0 ? name.slice(0, dot) : name;
    const ext = dot > 0 ? name.slice(dot) : '';
    for (let i = 0; i < 1000; i++) {
      const candidate = i ? `${stem} (${i})${ext}` : name;
      try {
        await dir.getFileHandle(candidate); // exists: try the next number
      } catch (e) {
        if (e.name !== 'NotFoundError') throw e;
        return dir.getFileHandle(candidate, { create: true });
      }
    }
    throw new Error(`Too many files named ${name} in the chosen folder.`);
  }

  function openPicked(target, files, onWritten) {
    const dirs = new Map();
    let out = null;
    let written = 0;

    async function dirFor(parts) {
      let dir = target.dir;
      for (let i = 0; i < parts.length; i++) {
        const key = parts.slice(0, i + 1).join('/');
        if (!dirs.has(key)) dirs.set(key, await dir.getDirectoryHandle(parts[i], { create: true }));
        dir = dirs.get(key);
      }
      return dir;
    }

    return {
      async begin(i) {
        let handle = target.handle;
        if (target.kind === 'dir') {
          const parts = CPZip.safePath(files[i].path).split('/');
          const name = parts.pop();
          handle = await freeName(await dirFor(parts), name);
        }
        out = await handle.createWritable();
      },
      async write(bytes) {
        await out.write(bytes);
        written += bytes.length;
        onWritten(written);
      },
      async end() {
        await out.close();
        out = null;
      },
      async finish() {
        return { kind: target.kind, name: target.kind === 'dir' ? target.dir.name : target.handle.name, downloads: [] };
      },
      async abort() {
        if (out) { try { await out.abort(); } catch (e) { /* already closed */ } }
      },
    };
  }

  // ---------- browser storage (OPFS via a worker) ----------

  async function liveDir() {
    const root = await navigator.storage.getDirectory();
    return root.getDirectoryHandle(OPFS_DIR, { create: true });
  }

  async function checkSpace(bytes) {
    if (!navigator.storage || !navigator.storage.estimate) return;
    const est = await navigator.storage.estimate();
    if (est.quota && est.quota - (est.usage || 0) < bytes) {
      throw new Error(`Not enough browser storage here: needs ${CP.bytes(bytes)}, ${CP.bytes(est.quota - (est.usage || 0))} free.`);
    }
  }

  async function openBrowserStorage(files, offerName, onWritten) {
    if (!navigator.storage || !navigator.storage.getDirectory) throw new Error('This browser has no private file storage; try another browser.');
    const total = files.reduce((n, f) => n + f.size, 0);
    await checkSpace(total);
    // Only a folder (paths with "/") becomes a .zip; loose files (videos, zips…) stay separate.
    const zip = files.some((f) => f.path.includes('/'));
    const baseName = (p) => CPZip.safePath(p).split('/').pop();
    const internal = `t-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const worker = new Worker('/static/live-worker.js' + VERSION);
    let failure = null;
    let onDone = null;
    let onOpened = null;

    worker.onmessage = (ev) => {
      const m = ev.data;
      if (m.op === 'progress') onWritten(m.written);
      else if (m.op === 'opened' && onOpened) onOpened();
      else if (m.op === 'done' && onDone) onDone.resolve();
      else if (m.op === 'error') {
        failure = new Error(m.message);
        if (onDone) onDone.reject(failure);
        if (sink.onError) sink.onError(failure);
      }
    };
    worker.onerror = (ev) => { failure = new Error(ev.message || 'storage worker failed'); if (sink.onError) sink.onError(failure); };

    const check = () => { if (failure) throw failure; };
    const sink = {
      onError: null,
      async begin(i) { check(); worker.postMessage({ op: 'begin', i }); },
      async write(bytes) {
        check();
        // Copy only when the bytes are a view into a bigger buffer; otherwise hand the buffer over.
        const buf = bytes.byteOffset === 0 && bytes.byteLength === bytes.buffer.byteLength ? bytes.buffer : bytes.slice().buffer;
        worker.postMessage({ op: 'data', buf }, [buf]);
      },
      async end(i) { check(); worker.postMessage({ op: 'end', i }); },
      async finish() {
        check();
        await new Promise((resolve, reject) => { onDone = { resolve, reject }; worker.postMessage({ op: 'finish' }); });
        worker.terminate();
        const dir = await liveDir();
        const stored = zip ? [[internal, baseName(offerName) + '.zip']]
          : files.map((f, i) => [`${internal}-${i}`, baseName(f.path)]);
        const downloads = [];
        for (const [entry, name] of stored) {
          const file = await (await dir.getFileHandle(entry)).getFile(); // disk-backed, not loaded into memory
          downloads.push({ name, url: URL.createObjectURL(file), size: file.size });
        }
        return { kind: 'opfs', internal, downloads };
      },
      async abort() {
        worker.postMessage({ op: 'abort' });
        setTimeout(() => worker.terminate(), 2000);
      },
    };

    try {
      await new Promise((resolve, reject) => {
        onOpened = resolve;
        sink.onError = reject;
        worker.postMessage({ op: 'open', name: internal, zip, files: files.map((f) => ({ path: f.path, size: f.size, mtime: f.mtime })) });
      });
    } catch (e) {
      worker.terminate();
      throw e;
    }
    sink.onError = null;
    return sink;
  }

  /** Deletes one finished transfer from browser storage (after the user saved it). */
  async function discard(result) {
    for (const d of result.downloads) URL.revokeObjectURL(d.url);
    if (result.kind !== 'opfs') return;
    try {
      const dir = await liveDir();
      const names = [];
      for await (const name of dir.keys()) if (name.startsWith(result.internal)) names.push(name);
      for (const name of names) await dir.removeEntry(name);
    } catch (e) { /* the next visit's sweep removes it */ }
  }

  /** Removes leftovers of transfers that were never cleared (closed tab, crash). */
  async function sweep() {
    if (!navigator.storage || !navigator.storage.getDirectory) return;
    try {
      const dir = await liveDir();
      for await (const [name, handle] of dir.entries()) {
        const file = await handle.getFile();
        if (Date.now() - file.lastModified > STALE_MS) await dir.removeEntry(name);
      }
    } catch (e) { /* entry in use or iteration unsupported: try again next visit */ }
  }

  return { describe, choose, open, discard, sweep };
})();
