// Live Transfer page: send files/folders from this device, or receive what another device offers.
//
// Data channel protocol (ordered, reliable):
//   sender → {t:'manifest', files:[{path,size,mtime}]}   receiver → {t:'ready'} | {t:'reject', reason}
//   sender → binary chunks, file after file in manifest order (a chunk never spans two files)
//   receiver → {t:'ack', bytes} as data reaches the disk; the sender keeps at most WINDOW unacked
//   sender → {t:'done'}                                 receiver → {t:'saved'}
//   either → {t:'cancel'}
'use strict';

(() => {
  const $ = (id) => document.getElementById(id);
  const CHUNK = 64 << 10;        // safe message size between every browser pair
  const READ_BLOCK = 4 << 20;    // read the file in 4 MB slices, send them in CHUNK pieces
  const HIGH_WATER = 8 << 20;    // pause while this much sits in the channel's send buffer
  const WINDOW = 32 << 20;       // ...or while this much is sent but not yet on the receiver's disk
  const ACK_EVERY = 4 << 20;
  const MANIFEST_PART = 32 << 10; // file list pieces stay well under every browser's message limit
  const POLL_MS = 3000;

  const send = { files: [], name: '', total: 0, folder: false, id: null, ctl: null, running: false };
  const recv = { ctl: null, running: false, result: null };

  // ---------- small helpers ----------

  function el(tag, attrs, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (k === 'class') node.className = v;
      else if (k === 'text') node.textContent = v;
      else node.setAttribute(k, v);
    }
    for (const c of children) if (c) node.append(c);
    return node;
  }

  function deviceLabel() {
    const ua = navigator.userAgent;
    const browser = /Edg\//.test(ua) ? 'Edge' : /OPR\//.test(ua) ? 'Opera' : /Firefox\//.test(ua) ? 'Firefox'
      : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'Browser';
    const os = /iPhone/.test(ua) ? 'iPhone' : /iPad/.test(ua) ? 'iPad' : /Android/.test(ua) ? 'Android'
      : /Mac OS X/.test(ua) ? 'Mac' : /Windows/.test(ua) ? 'Windows' : /Linux/.test(ua) ? 'Linux' : 'device';
    return `${browser} on ${os}`;
  }

  const errText = (e) => (e && e.name === 'AbortError' ? 'Cancelled.' : (e && e.message) || String(e));

  /** Progress bar with speed and time left, smoothed over the last few seconds. */
  function meter(box, total) {
    const samples = [];
    box.hidden = false;
    return (done) => {
      const now = performance.now();
      samples.push([now, done]);
      while (samples.length > 2 && now - samples[0][0] > 4000) samples.shift();
      const [t0, d0] = samples[0];
      const rate = now > t0 ? ((done - d0) / (now - t0)) * 1000 : 0;
      const pct = total ? (done / total) * 100 : 100;
      box.querySelector('span').style.width = pct.toFixed(1) + '%';
      let text = `${CP.bytes(done)} of ${CP.bytes(total)}`;
      if (rate > 0 && done < total) text += ` · ${CP.bytes(rate)}/s · ${CP.duration((total - done) / rate)} left`;
      box.querySelector('b').textContent = text;
    };
  }

  // Keeps the screen (and so the transfer) alive on phones and laptops while running.
  const wake = {
    lock: null,
    async on() {
      try { if (navigator.wakeLock) this.lock = await navigator.wakeLock.request('screen'); } catch (e) { this.lock = null; }
    },
    off() { if (this.lock) this.lock.release().catch(() => {}); this.lock = null; },
  };
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && (send.running || recv.running) && !wake.lock) wake.on();
  });
  window.addEventListener('beforeunload', (ev) => { if (send.running || recv.running) ev.preventDefault(); });

  /** Wraps a data channel: JSON control messages, waiting for a given one, and closure. */
  function channel(dc, onBinary) {
    const waiters = new Map();
    const listeners = new Set();
    let closedErr = null;
    const wakeAll = () => listeners.forEach((fn) => fn());
    dc.bufferedAmountLowThreshold = HIGH_WATER / 2;
    dc.addEventListener('bufferedamountlow', wakeAll);
    const ch = {
      acked: 0,
      parts: [], // manifest entries received so far
      send: (obj) => dc.send(JSON.stringify(obj)),
      next(type) { return new Promise((resolve, reject) => waiters.set(type, { resolve, reject })); },
      /** Resolves once cond() is true; re-checked whenever the channel drains or an ack arrives. */
      until(cond) {
        if (closedErr) return Promise.reject(closedErr);
        if (cond()) return Promise.resolve();
        return new Promise((resolve, reject) => {
          const fn = () => {
            if (closedErr) { listeners.delete(fn); reject(closedErr); } else if (cond()) { listeners.delete(fn); resolve(); }
          };
          listeners.add(fn);
        });
      },
      fail(err) {
        if (closedErr) return;
        closedErr = err;
        waiters.forEach((w) => w.reject(err));
        waiters.clear();
        wakeAll();
      },
    };
    dc.onmessage = (ev) => {
      if (typeof ev.data !== 'string') { onBinary(ev.data); return; }
      let m;
      try { m = JSON.parse(ev.data); } catch (e) { return; }
      if (m.t === 'ack') { ch.acked = m.bytes; wakeAll(); return; }
      if (m.t === 'manifest-part' && Array.isArray(m.files)) { for (const f of m.files) ch.parts.push(f); return; }
      if (m.t === 'cancel') { ch.fail(new Error('The other device cancelled.')); return; }
      if (m.t === 'reject') { ch.fail(new Error(m.reason || 'The other device refused.')); return; }
      const w = waiters.get(m.t);
      if (w) { waiters.delete(m.t); w.resolve(m); }
    };
    dc.onclose = () => ch.fail(new Error('The connection was lost.'));
    return ch;
  }

  // ---------- send ----------

  const relPath = (f) => f.webkitRelativePath || f.name;

  function setFiles(list) {
    const files = Array.from(list).filter((f) => f.name);
    if (!files.length) return;
    const top = relPath(files[0]).split('/')[0];
    const fromFolder = files.every((f) => f.webkitRelativePath && f.webkitRelativePath.split('/')[0] === top);
    send.files = files;
    send.total = files.reduce((n, f) => n + f.size, 0);
    send.name = fromFolder ? top : files.length === 1 ? files[0].name : `${files.length} files`;
    send.folder = fromFolder;
    $('send-summary').hidden = false;
    $('send-summary').textContent = `${fromFolder ? '📁' : '📄'} ${send.name} · ${files.length} file${files.length === 1 ? '' : 's'} · ${CP.bytes(send.total)}`;
    $('send-start').disabled = false;
  }

  async function startSend() {
    send.running = true;
    send.ctl = new AbortController();
    $('send-pick').hidden = true;
    $('send-run').hidden = false;
    $('send-progress').hidden = true;
    $('send-cancel').textContent = 'Cancel';
    const status = (t) => { $('send-status').textContent = t; };
    status('Waiting for the other device… open cp → Live there and tap Receive.');
    wake.on();
    let conn = null;
    try {
      const { id } = await CP.api('POST', '/api/live', { device: deviceLabel(), name: send.name, files: send.files.length, bytes: send.total, folder: send.folder });
      send.id = id;
      conn = await LivePeer.connect({ id, role: 'sender', signal: send.ctl.signal, onStatus: status });
      send.ctl.signal.addEventListener('abort', () => { try { conn.dc.send(JSON.stringify({ t: 'cancel' })); } catch (e) { /* closed */ } conn.pc.close(); });
      await sendAll(conn, status);
      status(`✅ Done. All ${send.files.length} file${send.files.length === 1 ? '' : 's'} arrived on the other device.`);
    } catch (e) {
      status('❌ ' + errText(e));
      if (conn) { try { conn.dc.send(JSON.stringify({ t: 'cancel' })); } catch (err) { /* closed */ } }
    } finally {
      send.running = false;
      wake.off();
      if (send.id) CP.api('DELETE', '/api/live/' + send.id).catch(() => {});
      send.id = null;
      if (conn) setTimeout(() => conn.pc.close(), 1500);
      $('send-cancel').textContent = 'Send something else';
    }
  }

  async function sendAll(conn, status) {
    const { dc } = conn;
    const ch = channel(dc, () => {});
    send.ctl.signal.addEventListener('abort', () => ch.fail(new DOMException('Cancelled', 'AbortError')));
    status('Connected. Waiting for the other device to get ready…');
    const ready = ch.next('ready');
    sendManifest(ch, send.files.map((f) => ({ path: relPath(f), size: f.size, mtime: f.lastModified })));
    await ready;
    conn.stopSignaling();
    CP.api('DELETE', '/api/live/' + send.id).catch(() => {}); // introductions are over
    send.id = null;

    status('Sending… keep this tab open.');
    const show = meter($('send-progress'), send.total);
    let sent = 0;
    const room = () => dc.bufferedAmount < HIGH_WATER && sent - ch.acked < WINDOW;
    const ticker = setInterval(() => show(ch.acked), 500);
    try {
      for (const f of send.files) {
        for (let off = 0; off < f.size; off += READ_BLOCK) {
          const block = await f.slice(off, Math.min(off + READ_BLOCK, f.size)).arrayBuffer();
          for (let p = 0; p < block.byteLength; p += CHUNK) {
            await ch.until(room);
            const n = Math.min(CHUNK, block.byteLength - p);
            dc.send(block.slice(p, p + n)); // an exact buffer: some engines send a view's whole backing buffer
            sent += n;
          }
        }
      }
      const saved = ch.next('saved');
      ch.send({ t: 'done' });
      status('Sent. Waiting for the other device to finish saving…');
      await saved;
      show(send.total);
    } finally {
      clearInterval(ticker);
    }
  }

  /** Sends the file list in pieces that fit one data channel message, then the end marker. */
  function sendManifest(ch, entries) {
    let part = [];
    let size = 0;
    for (const e of entries) {
      const n = JSON.stringify(e).length;
      if (part.length && size + n > MANIFEST_PART) { ch.send({ t: 'manifest-part', files: part }); part = []; size = 0; }
      part.push(e);
      size += n;
    }
    if (part.length) ch.send({ t: 'manifest-part', files: part });
    ch.send({ t: 'manifest', count: entries.length });
  }

  function resetSend() {
    if (send.running) { send.ctl.abort(); return; }
    send.files = [];
    $('pick-files').value = '';
    $('pick-folder').value = '';
    $('send-summary').hidden = true;
    $('send-start').disabled = true;
    $('send-run').hidden = true;
    $('send-pick').hidden = false;
  }

  // ---------- receive ----------

  async function refreshOffers() {
    if (recv.running || document.visibilityState !== 'visible') return;
    let list = [];
    try { list = (await CP.api('GET', '/api/live')).transfers.filter((t) => t.id !== send.id); } catch (e) { return; }
    const ul = $('recv-offers');
    ul.replaceChildren(...list.map((t) => {
      const btn = el('button', { type: 'button', class: 'primary', text: 'Receive' });
      btn.addEventListener('click', () => startReceive(t));
      const icon = t.offer.files > 1 ? '📁' : '📄';
      return el('li', {},
        el('div', { class: 'offer-text' },
          el('div', { class: 'offer-name', text: `${icon} ${t.offer.name}` }),
          el('div', { class: 'offer-meta', text: `${t.offer.files} file${t.offer.files === 1 ? '' : 's'} · ${CP.bytes(t.offer.bytes)} · from ${t.offer.device} · ${CP.ago(t.created)}` })),
        btn);
    }));
    $('recv-empty').hidden = list.length > 0;
  }

  function validManifest(m, offer) {
    return m && Array.isArray(m.files) && m.files.length === offer.files && m.files.every((f) =>
      f && typeof f.path === 'string' && f.path.length <= 4096 && Number.isSafeInteger(f.size) && f.size >= 0);
  }

  async function startReceive(offer) {
    let target;
    try {
      target = await LiveSink.choose(offer.offer); // first: the folder/file picker needs this click
    } catch (e) {
      if (e.name !== 'AbortError') CP.toast(errText(e), 'err');
      return;
    }
    recv.running = true;
    recv.ctl = new AbortController();
    recv.result = null;
    $('recv-pick').hidden = true;
    $('recv-run').hidden = false;
    $('recv-saved').hidden = true;
    $('recv-clear').hidden = true;
    $('recv-cancel').hidden = false;
    $('recv-progress').hidden = true;
    const status = (t) => { $('recv-status').textContent = t; };
    status(`Connecting to ${offer.offer.device}…`);
    wake.on();
    let conn = null;
    let sink = null;
    try {
      await CP.api('POST', `/api/live/${offer.id}/join`);
      conn = await LivePeer.connect({ id: offer.id, role: 'receiver', signal: recv.ctl.signal, onStatus: status });
      const r = await receiveAll(conn, offer, target, status, (s) => { sink = s; });
      recv.result = r;
      showResult(r, status);
    } catch (e) {
      status('❌ ' + errText(e));
      if (conn) { try { conn.dc.send(JSON.stringify({ t: 'cancel' })); } catch (err) { /* closed */ } }
      if (sink) sink.abort();
      $('recv-cancel').textContent = 'Back';
    } finally {
      recv.running = false;
      wake.off();
      if (conn) setTimeout(() => conn.pc.close(), 1500);
    }
  }

  async function receiveAll(conn, offer, target, status, keepSink) {
    const files = [];
    let sink = null;
    let cur = -1;
    let left = 0;
    let got = 0;
    let lastAck = 0;
    let chain = Promise.resolve();
    let show = () => {};
    const total = offer.offer.bytes;

    const onWritten = (written) => {
      if (written - lastAck >= ACK_EVERY || written === total) {
        lastAck = written;
        try { ch.send({ t: 'ack', bytes: written }); } catch (e) { /* channel closing */ }
      }
    };
    const nextFile = async () => {
      if (cur >= 0) await sink.end(cur);
      cur++;
      if (cur < files.length) { await sink.begin(cur); left = files[cur].size; }
    };
    const settle = async () => { while (cur < files.length && left === 0) await nextFile(); };
    let chainErr = null;
    const onChunk = async (buf) => {
      if (chainErr) return;
      const bytes = new Uint8Array(buf);
      const n = bytes.length; // read first: the browser-storage sink hands the buffer to its worker
      if (cur >= files.length || n > left) throw new Error('The sender sent more data than announced.');
      await sink.write(bytes);
      left -= n;
      got += n;
      await settle();
    };
    const ch = channel(conn.dc, (buf) => {
      chain = chain.then(() => onChunk(buf)).catch((e) => { chainErr = chainErr || e; ch.fail(e); });
    });
    recv.ctl.signal.addEventListener('abort', () => ch.fail(new DOMException('Cancelled', 'AbortError')));

    const end = await ch.next('manifest');
    const manifest = { files: ch.parts };
    if (end.count !== ch.parts.length || !validManifest(manifest, offer.offer)) {
      ch.send({ t: 'reject', reason: 'The file list did not match the offer.' });
      throw new Error('The file list did not match the offer.');
    }
    for (const f of manifest.files) files.push(f); // no spread: folders may hold 100k+ files
    try {
      sink = await LiveSink.open(target, files, offer.offer.name, onWritten);
    } catch (e) {
      ch.send({ t: 'reject', reason: 'The receiving device could not save: ' + errText(e) });
      throw e;
    }
    keepSink(sink);
    if ('onError' in sink) sink.onError = (e) => ch.fail(e);
    const done = ch.next('done');
    await nextFile();
    await settle();
    ch.send({ t: 'ready' });
    conn.stopSignaling();

    status(`Receiving from ${offer.offer.device}…`);
    show = meter($('recv-progress'), total);
    const ticker = setInterval(() => show(got), 500);
    try {
      await done;
      await chain;
      if (chainErr) throw chainErr;
      if (cur < files.length || got !== total) throw new Error('The transfer ended early.');
      status('Saving…');
      $('recv-cancel').hidden = true; // finishing cannot be interrupted halfway
      const result = await sink.finish();
      ch.send({ t: 'saved' });
      show(total);
      return result;
    } finally {
      clearInterval(ticker);
    }
  }

  function showResult(r, status) {
    $('recv-cancel').hidden = true;
    if (r.kind !== 'opfs') {
      status(`✅ Saved into “${r.name}”.`);
      $('recv-clear').hidden = false;
      $('recv-clear').textContent = 'Receive something else';
      return;
    }
    status(r.downloads.length > 1 ? '✅ Received. Tap Save on each file to put it in your downloads.' : '✅ Received. Tap Save to put it in your downloads.');
    const ul = $('recv-saved');
    ul.replaceChildren(...r.downloads.map((d) => el('li', {},
      el('span', { class: 'file-name', text: d.name }),
      el('span', { class: 'muted', text: CP.bytes(d.size) }),
      el('a', { class: 'btn primary', href: d.url, download: d.name, text: 'Save' }))));
    ul.hidden = false;
    $('recv-clear').hidden = false;
    $('recv-clear').textContent = 'Done, free the space';
  }

  async function resetReceive() {
    if (recv.running) { recv.ctl.abort(); return; }
    if (recv.result) await LiveSink.discard(recv.result);
    recv.result = null;
    $('recv-cancel').textContent = 'Cancel';
    $('recv-run').hidden = true;
    $('recv-pick').hidden = false;
    refreshOffers();
  }

  // ---------- wiring ----------

  if (!LivePeer.supported()) {
    $('live-unsupported').hidden = false;
    return;
  }
  $('pick-files').addEventListener('change', (ev) => setFiles(ev.target.files));
  $('pick-folder').addEventListener('change', (ev) => setFiles(ev.target.files));
  const drop = $('send-drop');
  drop.addEventListener('dragover', (ev) => { ev.preventDefault(); drop.classList.add('hot'); });
  drop.addEventListener('dragleave', () => drop.classList.remove('hot'));
  drop.addEventListener('drop', (ev) => {
    ev.preventDefault();
    drop.classList.remove('hot');
    const items = Array.from(ev.dataTransfer.items || []);
    if (items.some((i) => i.webkitGetAsEntry && i.webkitGetAsEntry() && i.webkitGetAsEntry().isDirectory)) {
      CP.toast('To send a folder, use “Choose a folder”.', 'err');
      return;
    }
    setFiles(ev.dataTransfer.files);
  });
  $('send-start').addEventListener('click', startSend);
  $('send-cancel').addEventListener('click', resetSend);
  $('recv-cancel').addEventListener('click', resetReceive);
  $('recv-clear').addEventListener('click', resetReceive);
  $('recv-where').textContent = LiveSink.describe();

  LiveSink.sweep();
  refreshOffers();
  setInterval(refreshOffers, POLL_MS);
  document.addEventListener('visibilitychange', refreshOffers);
})();
