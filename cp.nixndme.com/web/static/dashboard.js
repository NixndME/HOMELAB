// Dashboard: list shares, create shares (chunked uploads), row actions, paste and drop.
'use strict';

const state = { status: 'active', query: '', shares: [], settings: null, limits: null, files: [] };
const $ = (id) => document.getElementById(id);

function el(tag, attrs, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v === null || v === undefined) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else node.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children) if (c) node.append(c);
  return node;
}

// ---------- list ----------

let loadSeq = 0;

async function loadShares() {
  const seq = ++loadSeq;
  const q = new URLSearchParams({ status: state.status, q: state.query });
  try {
    const data = await CP.api('GET', '/api/shares?' + q);
    if (seq !== loadSeq) return; // a newer request (e.g. a search) is in flight; drop this stale answer
    state.shares = data.shares;
    renderShares();
  } catch (e) {
    CP.toast(e.message, 'err');
  }
}

async function loadUsage() {
  try {
    const u = await CP.api('GET', '/api/usage');
    const pct = Math.min(100, (u.used_bytes / u.cap_bytes) * 100);
    $('usage-fill').style.width = pct.toFixed(1) + '%';
    $('usage-fill').classList.toggle('high', pct > 85);
    $('usage-text').textContent = `${CP.bytes(u.used_bytes)} of ${CP.bytes(u.cap_bytes)} used`;
  } catch (e) {
    $('usage-text').textContent = 'usage unavailable';
  }
}

const hostOf = (u) => { try { return new URL(u).host; } catch (e) { return u; } };

const partsOf = (s) => ({ text: !!s.text, files: s.files.length, links: s.links.length });
const isMixed = (s) => { const p = partsOf(s); return [p.text, p.files > 0, p.links > 0].filter(Boolean).length > 1; };

// Titles people can say out loud: "Paste #12", "Share #13", "Links #14 · example.com +2".
function shareTitle(s) {
  if (s.title) return s.title;
  if (isMixed(s)) return `Share #${s.number}`;
  if (s.text) return `Paste #${s.number}`;
  if (s.links.length) return `Links #${s.number} · ${hostOf(s.links[0])}${s.links.length > 1 ? ` +${s.links.length - 1}` : ''}`;
  if (s.files.length) return s.files.length > 1 ? `${s.files[0].name} +${s.files.length - 1}` : s.files[0].name;
  return `Share #${s.number}`;
}

// What is inside, e.g. "📝 text · 📄 2 files · 🔗 3 links".
function contentsLine(s) {
  const p = partsOf(s);
  const out = [];
  if (p.text) out.push('📝 text');
  if (p.files) out.push(`📄 ${p.files} file${p.files === 1 ? '' : 's'}`);
  if (p.links) out.push(`🔗 ${p.links} link${p.links === 1 ? '' : 's'}`);
  return out.join(' · ');
}

// The number is part of auto titles already; add it as a tag when the title is a custom
// name or a file name.
const showsNumber = (s) => !s.title && (isMixed(s) || !!s.text || s.links.length > 0);

function titleNode(s) {
  const t = shareTitle(s);
  const node = el('div', { class: 'share-title', title: t, text: t });
  if (!showsNumber(s)) node.append(el('span', { class: 'num', text: ` #${s.number}` }));
  return node;
}

const kindIcon = { text: '📝', file: '📄', link: '🔗' };
const iconFor = (s) => (isMixed(s) ? '📦' : kindIcon[s.kind]);

function statusLine(s) {
  if (s.ended_at) return `ended ${CP.ago(s.ended_at)} · ${s.end_reason}`;
  if (!s.active) return 'ending…';
  if (s.consumed_until) return `revealed, destroyed ${CP.left(s.consumed_until)}`;
  return `expires ${CP.left(s.expires_at)}`;
}

function badges(s) {
  const out = el('span', { class: 'badges' });
  if (!s.ended_at) out.append(el('span', { class: 'badge ' + (s.public ? 'ok' : ''), text: s.public ? 'public' : 'private' }));
  if (s.has_password) out.append(el('span', { class: 'badge', text: '🔒 password' }));
  if (s.burn) out.append(el('span', { class: 'badge warn', text: '🔥 read once' }));
  const n = (count, word) => `${count} ${word}${count === 1 ? '' : 's'}`;
  if (s.max_downloads) out.append(el('span', { class: 'badge', title: 'Downloads used of the limit', text: `⬇ ${s.downloads} of ${n(s.max_downloads, 'download')}` }));
  else if (s.downloads) out.append(el('span', { class: 'badge', title: 'Times a visitor downloaded it', text: `⬇ ${n(s.downloads, 'download')}` }));
  if (s.views) out.append(el('span', { class: 'badge', title: 'Times a visitor opened the page', text: `👁 ${n(s.views, 'view')}` }));
  return out;
}

function rowActions(s) {
  const box = el('div', { class: 'row-actions' });
  if (s.ended_at) {
    box.append(el('button', { class: 'btn small', 'data-act': 'log', text: 'Log' }));
    box.append(el('button', { class: 'btn small danger-outline', 'data-act': 'delete', text: 'Remove' }));
    return box;
  }
  box.append(
    el('button', { class: 'btn small', 'data-act': 'copy', text: 'Copy link' }),
    el('button', { class: 'btn small', 'data-act': 'qr', text: 'QR' }),
  );
  if (s.text) box.append(el('button', { class: 'btn small', 'data-act': 'copytext', text: 'Copy text' }));
  if (s.files.length) {
    const href = s.files.length > 1 || s.text ? '/z/' + s.slug : `/d/${s.slug}/${s.files[0].id}`;
    box.append(el('a', { class: 'btn small', href, text: s.files.length > 1 || s.text ? 'Download all' : 'Download' }));
  }
  const more = el('details', { class: 'more' }, el('summary', { class: 'btn small', 'aria-label': 'More actions', text: '⋯' }));
  more.append(el('div', { class: 'menu' },
    el('button', { 'data-act': 'toggle-public', text: s.public ? 'Make private' : 'Make public' }),
    el('button', { 'data-act': 'edit', text: 'Rename / edit' }),
    el('button', { 'data-act': 'regenerate', text: 'New link (kill old)' }),
    el('button', { 'data-act': 'log', text: 'Access log' }),
    el('button', { 'data-act': 'delete', class: 'danger-text', text: 'Delete now' })));
  box.append(more);
  return box;
}

function shareRow(s) {
  const row = el('article', { class: 'share-row' + (s.ended_at ? ' ended' : ''), 'data-slug': s.slug });
  if (!s.ended_at) {
    row.tabIndex = 0;
    row.setAttribute('role', 'button');
    row.title = 'Click to view';
  }
  const main = el('div', { class: 'share-main' },
    el('span', { class: 'kind', 'aria-hidden': 'true', text: iconFor(s) }),
    el('div', { class: 'share-text' },
      titleNode(s),
      el('div', { class: 'share-meta' },
        el('span', { class: 'contents', text: contentsLine(s) }),
        el('span', { text: CP.bytes(s.size || 0) }),
        el('span', { text: 'created ' + CP.ago(s.created_at) }),
        el('span', { class: s.ended_at ? '' : 'left', text: statusLine(s) }),
        badges(s))));
  row.append(main, rowActions(s));
  return row;
}

function renderShares() {
  $('shares').replaceChildren(...state.shares.map(shareRow));
  $('empty').hidden = state.shares.length > 0;
}

// ---------- view popup: click a row to open it, Esc or click outside to close ----------
// The share page is fetched and its content placed straight into the popup (no iframe), so it
// appears already filled, in the current theme, with no blank flash.

let viewing = null;
let viewSeq = 0;

async function openView(row) {
  const s = findShare(row.dataset.slug);
  if (!s) return;
  const seq = ++viewSeq;
  row.classList.add('loading');
  try {
    const res = await fetch(`/s/${s.slug}?embed=1`, { credentials: 'same-origin' });
    if (!res.ok) throw new Error(res.status === 404 || res.status === 410 ? 'This share has ended' : 'Could not open this share');
    const doc = new DOMParser().parseFromString(await res.text(), 'text/html');
    if (seq !== viewSeq) return; // another row was clicked meanwhile
    const main = doc.querySelector('main');
    main.querySelectorAll('dialog').forEach((d) => d.remove()); // the dashboard has its own QR dialog
    const nodes = Array.from(main.childNodes); // copy first: adopting a node removes it from the live list
    $('view-body').replaceChildren(...nodes.map((n) => document.adoptNode(n)));
    $('view-title').textContent = shareTitle(s) + (showsNumber(s) ? '' : `  #${s.number}`);
    viewing = s;
    CP.refreshTimes($('view-body'));
    if (!$('view-dialog').open) $('view-dialog').showModal();
    $('view-body').scrollTop = 0;
  } catch (e) {
    CP.toast(e.message, 'err');
  } finally {
    row.classList.remove('loading');
  }
}

function closeView() {
  if ($('view-dialog').open) $('view-dialog').close();
}

$('view-dialog').addEventListener('close', () => {
  viewing = null;
  $('view-body').replaceChildren();
});
// A click on the dimmed backdrop lands on the <dialog> itself, not on its content.
$('view-dialog').addEventListener('click', (ev) => { if (ev.target === ev.currentTarget) closeView(); });
$('view-copy').addEventListener('click', async () => {
  if (viewing) CP.toast((await CP.copyText(viewing.url)) ? 'Link copied' : 'Copy failed', 'ok');
});
$('view-qr').addEventListener('click', () => { if (viewing) CP.openQR(viewing.slug); });

$('shares').addEventListener('click', (ev) => {
  const row = ev.target.closest('.share-row');
  if (row && !row.classList.contains('ended') && !ev.target.closest('.row-actions')) openView(row);
});
$('shares').addEventListener('keydown', (ev) => {
  const row = ev.target.closest('.share-row');
  if ((ev.key === 'Enter' || ev.key === ' ') && row && ev.target === row && !row.classList.contains('ended')) {
    ev.preventDefault();
    openView(row);
  }
});

// ---------- ⋯ menus: Esc, a click outside, or opening another one closes them ----------

function closeMenus(except) {
  document.querySelectorAll('details.more[open]').forEach((d) => { if (d !== except) d.open = false; });
}
document.addEventListener('click', (ev) => closeMenus(ev.target.closest('details.more')));
document.addEventListener('keydown', (ev) => { if (ev.key === 'Escape') closeMenus(null); });

// ---------- row actions ----------

const findShare = (slug) => state.shares.find((s) => s.slug === slug);

async function fetchText(slug) {
  const res = await fetch('/r/' + slug, { credentials: 'same-origin' });
  if (!res.ok) throw new Error('could not load the text');
  return res.text();
}

const actions = {
  copy: async (s) => CP.toast((await CP.copyText(s.url)) ? 'Link copied' : 'Copy failed', 'ok'),
  qr: async (s) => CP.openQR(s.slug),
  copytext: async (s) => CP.toast((await CP.copyText(await fetchText(s.slug))) ? 'Text copied' : 'Copy failed', 'ok'),
  'toggle-public': async (s) => {
    await CP.api('PATCH', '/api/shares/' + s.slug, { public: !s.public });
    CP.toast(s.public ? 'Now private' : 'Now public', 'ok');
    await loadShares();
  },
  regenerate: async (s, btn) => {
    if (!CP.armed(btn, 'Click again: old link dies')) return false;
    const upd = await CP.api('POST', `/api/shares/${s.slug}/regenerate`);
    await CP.copyText(upd.url);
    CP.toast('New link created and copied', 'ok');
    await loadShares();
  },
  delete: async (s, btn) => {
    if (!CP.armed(btn, 'Click again to delete')) return false;
    await CP.api('DELETE', '/api/shares/' + s.slug);
    CP.toast('Deleted', 'ok');
    await Promise.all([loadShares(), loadUsage()]);
  },
  log: async (s) => showLog(s),
  edit: async (s) => openEdit(s),
};

async function onRowAction(btn) {
  const s = findShare(btn.closest('.share-row').dataset.slug);
  const fn = s && actions[btn.dataset.act];
  if (!fn) return;
  try {
    if ((await fn(s, btn)) === false) return; // armed, waiting for the confirming click
  } catch (e) {
    CP.toast(e.message, 'err');
  }
  const details = btn.closest('details');
  if (details) details.open = false;
}

async function showLog(s) {
  const data = await CP.api('GET', `/api/shares/${s.slug}/log`);
  const rows = data.entries.map((e) => el('tr', { class: e.ok ? '' : 'bad' },
    el('td', { text: CP.date(e.at) }), el('td', { text: e.action + (e.ok ? '' : ' (refused)') }),
    el('td', { text: e.ip }), el('td', { class: 'ua', text: e.ua, title: e.ua })));
  if (!rows.length) rows.push(el('tr', {}, el('td', { colspan: '4', class: 'muted', text: 'No visitor has opened this share yet. Your own views are not logged.' })));
  $('log-body').replaceChildren(...rows);
  $('log-dialog').showModal();
}

let editing = null;
function openEdit(s) {
  editing = s;
  $('edit-title').value = s.title;
  $('edit-title').placeholder = `Empty = automatic: ${shareTitle({ ...s, title: '' })}`;
  $('edit-expiry').value = '';
  $('edit-max').value = s.max_downloads;
  $('edit-password').value = '';
  $('edit-clear-password').checked = false;
  $('edit-clear-password').disabled = !s.has_password;
  $('edit-error').hidden = true;
  $('edit-dialog').showModal();
}

$('edit-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const body = { max_downloads: Number($('edit-max').value) || 0 };
  body.title = $('edit-title').value.trim();
  if ($('edit-expiry').value) body.expires_in = Number($('edit-expiry').value);
  if ($('edit-clear-password').checked) body.password = '';
  else if ($('edit-password').value) body.password = $('edit-password').value;
  try {
    await CP.api('PATCH', '/api/shares/' + editing.slug, body);
    $('edit-dialog').close();
    CP.toast('Saved', 'ok');
    loadShares();
  } catch (e) {
    $('edit-error').textContent = e.message;
    $('edit-error').hidden = false;
  }
});

// ---------- new share ----------

// Puts the cursor in the part that was pre-filled (or in the text box for a blank share).
function focusPart(part) {
  if (part === 'files') return; // the picked files are already listed
  setTimeout(() => $(part === 'links' ? 'link-input' : 'text-input').focus(), 0);
}

function renderPicked() {
  $('picked').replaceChildren(...state.files.map((f, i) => el('li', {},
    el('span', { class: 'file-name', text: f.name }),
    el('span', { class: 'muted', text: CP.bytes(f.size) }),
    el('button', { type: 'button', class: 'icon-btn', 'data-remove': String(i), 'aria-label': 'Remove ' + f.name, text: '✕' }))));
}

function addFiles(fileList) {
  state.files = state.files.concat(Array.from(fileList));
  renderPicked();
  updateNameHint();
}

const linkLines = () => $('link-input').value.split('\n').map((l) => l.trim()).filter(Boolean);

function updateLinkStats() {
  const n = linkLines().length;
  $('link-stats').textContent = n ? `${n} link${n === 1 ? '' : 's'}` : '';
}

// The Name box shows what the share will be called if it is left empty.
function updateNameHint() {
  const files = state.files;
  const hasText = $('text-input').value.trim() !== '';
  const links = linkLines().length;
  const kinds = [hasText, files.length > 0, links > 0].filter(Boolean).length;
  let auto = 'an automatic name';
  if (kinds > 1) auto = 'Share #…';
  else if (hasText) auto = 'Paste #…';
  else if (files.length) auto = files.length > 1 ? `${files[0].name} +${files.length - 1}` : files[0].name;
  else if (links) auto = 'Links #…';
  $('title').placeholder = `Leave empty to use: ${auto}`;
}

function updateTextStats() {
  const t = $('text-input').value;
  const lines = t ? t.split('\n').length : 0;
  $('text-stats').textContent = t ? `${lines.toLocaleString()} lines · ${CP.bytes(new Blob([t]).size)}` : '';
}

function openNew(opts) {
  opts = opts || {};
  state.files = [];
  $('new-form').reset();
  $('text-input').value = opts.text || '';
  $('link-input').value = opts.link || '';
  renderPicked();
  updateTextStats();
  updateLinkStats();
  updateNameHint();
  $('new-error').hidden = true;
  $('progress').hidden = true;
  $('create-btn').disabled = false;
  const s = state.settings;
  if (s) {
    $('expiry').value = String(s.default_expiry);
    $('public').checked = s.default_public;
    $('public-off').hidden = s.public_sharing;
  }
  if (opts.files) addFiles(opts.files);
  if (!$('new-dialog').open) $('new-dialog').showModal();
  focusPart(opts.part);
}

function setProgress(done, total, label) {
  $('progress').hidden = false;
  const pct = total ? Math.min(100, (done / total) * 100) : 100;
  $('progress-fill').style.width = pct.toFixed(1) + '%';
  $('progress-text').textContent = label || `${CP.bytes(done)} / ${CP.bytes(total)}`;
}

function putChunk(url, blob, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', url);
    xhr.setRequestHeader('X-CSRF-Token', CP.csrf);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve();
      let msg = `upload failed (${xhr.status})`;
      try { msg = JSON.parse(xhr.responseText).error || msg; } catch (e) { /* keep default */ }
      const err = new Error(msg);
      err.retry = xhr.status >= 500 || xhr.status === 429;
      reject(err);
    };
    xhr.onerror = () => { const e = new Error('network error during upload'); e.retry = true; reject(e); };
    xhr.send(blob);
  });
}

const MAX_RETRIES = 4;

async function uploadBlob(blob, name, mime, base, total) {
  const init = await CP.api('POST', '/api/uploads', { name, size: blob.size, mime: mime || 'application/octet-stream' });
  const size = init.chunk_bytes;
  for (let i = 0, off = 0; off < blob.size; i++, off += size) {
    const part = blob.slice(off, off + size);
    for (let attempt = 0; ; attempt++) {
      try {
        await putChunk(`/api/uploads/${init.id}/${i}`, part, (n) => setProgress(base + off + n, total));
        break;
      } catch (e) {
        if (!e.retry || attempt >= MAX_RETRIES) throw e;
        await new Promise((r) => setTimeout(r, 1000 * (attempt + 1)));
      }
    }
  }
  return init.id;
}

function checkFiles() {
  const max = state.limits ? state.limits.max_file_bytes : Infinity;
  const tooBig = state.files.find((f) => f.size > max);
  if (tooBig) throw new Error(`${tooBig.name} is larger than ${CP.bytes(max)}`);
  const empty = state.files.find((f) => f.size === 0);
  if (empty) throw new Error(`${empty.name} is empty`);
}

// Uploads the text (if any) and every file, with one progress bar over all bytes.
async function collectUploads() {
  const text = $('text-input').value;
  const hasText = text.trim() !== '';
  if (!hasText && !state.files.length && !linkLines().length) throw new Error('Add some text, a file or a link');
  checkFiles();
  const textBlob = hasText ? new Blob([text], { type: 'text/plain' }) : null;
  const total = (textBlob ? textBlob.size : 0) + state.files.reduce((a, f) => a + f.size, 0);
  let base = 0;
  let textUpload = '';
  if (textBlob) {
    textUpload = await uploadBlob(textBlob, 'paste.txt', 'text/plain', 0, total);
    base = textBlob.size;
  }
  const uploads = [];
  for (const f of state.files) {
    uploads.push(await uploadBlob(f, f.name, f.type, base, total));
    base += f.size;
  }
  return { textUpload, uploads };
}

function createBody({ textUpload, uploads }) {
  return {
    title: $('title').value, text_upload: textUpload, uploads, links: linkLines(),
    language: textUpload ? $('language').value : '',
    expires_in: Number($('expiry').value), public: $('public').checked, burn: $('burn').checked,
    max_downloads: Number($('max-downloads').value) || 0, password: $('share-password').value,
  };
}

$('new-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const btn = $('create-btn');
  const errEl = $('new-error');
  errEl.hidden = true;
  btn.disabled = true;
  try {
    const parts = await collectUploads();
    setProgress(1, 1, 'Finishing…');
    const share = await CP.api('POST', '/api/shares', createBody(parts));
    $('new-dialog').close();
    const copied = await CP.copyText(share.url);
    CP.toast(copied ? 'Share created, link copied' : 'Share created', 'ok');
    state.status = 'active';
    syncTabs();
    await Promise.all([loadShares(), loadUsage()]);
  } catch (e) {
    errEl.textContent = e.message;
    errEl.hidden = false;
    $('progress').hidden = true;
  } finally {
    btn.disabled = false;
  }
});

// ---------- wiring ----------

function syncTabs() {
  document.querySelectorAll('.toolbar [data-status]').forEach((b) => b.setAttribute('aria-selected', String(b.dataset.status === state.status)));
}

document.querySelectorAll('.toolbar [data-status]').forEach((b) => b.addEventListener('click', () => {
  state.status = b.dataset.status;
  syncTabs();
  loadShares();
}));

let searchTimer = null;
$('search').addEventListener('input', (ev) => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => { state.query = ev.target.value; loadShares(); }, 250);
});

$('shares').addEventListener('click', (ev) => {
  const btn = ev.target.closest('[data-act]');
  if (btn) onRowAction(btn);
});

$('new-share').addEventListener('click', () => openNew());
$('file-input').addEventListener('change', (ev) => { addFiles(ev.target.files); ev.target.value = ''; });
$('picked').addEventListener('click', (ev) => {
  const b = ev.target.closest('[data-remove]');
  if (!b) return;
  state.files = state.files.filter((_, i) => i !== Number(b.dataset.remove));
  renderPicked();
  updateNameHint();
});
$('text-input').addEventListener('input', () => { updateTextStats(); updateNameHint(); });
$('link-input').addEventListener('input', () => { updateLinkStats(); updateNameHint(); });

const dz = $('dropzone');
['dragenter', 'dragover'].forEach((t) => dz.addEventListener(t, (e) => { e.preventDefault(); dz.classList.add('hot'); }));
['dragleave', 'drop'].forEach((t) => dz.addEventListener(t, () => dz.classList.remove('hot')));
dz.addEventListener('drop', (e) => { e.preventDefault(); e.stopPropagation(); addFiles(e.dataTransfer.files); });

// Page-wide drop and paste open the New share dialog pre-filled.
const overlay = $('drop-overlay');
let dragDepth = 0;
window.addEventListener('dragenter', (e) => {
  if ($('new-dialog').open || !e.dataTransfer.types.includes('Files')) return;
  dragDepth++;
  overlay.hidden = false;
});
window.addEventListener('dragleave', () => { if (--dragDepth <= 0) { dragDepth = 0; overlay.hidden = true; } });
window.addEventListener('dragover', (e) => e.preventDefault());
window.addEventListener('drop', (e) => {
  e.preventDefault();
  dragDepth = 0;
  overlay.hidden = true;
  if (!$('new-dialog').open && e.dataTransfer.files.length) openNew({ files: e.dataTransfer.files, part: 'files' });
});

const URL_RE = /^https?:\/\/\S+$/i;
// Pasted text where every non-empty line is a URL becomes a link share.
const allLinks = (t) => { const ls = t.split('\n').map((l) => l.trim()).filter(Boolean); return ls.length > 0 && ls.every((l) => URL_RE.test(l)); };
document.addEventListener('paste', (e) => {
  const t = e.target;
  if (document.querySelector('dialog[open]') || (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA'))) return;
  const files = e.clipboardData.files;
  if (files && files.length) { e.preventDefault(); openNew({ files, part: 'files' }); return; }
  const text = e.clipboardData.getData('text');
  if (!text) return;
  e.preventDefault();
  if (allLinks(text)) openNew({ link: text.trim(), part: 'links' });
  else openNew({ text, part: 'text' });
});

const REFRESH_MS = 30000;

async function init() {
  try {
    const data = await CP.api('GET', '/api/settings');
    state.settings = data.settings;
    state.limits = data.limits;
    $('file-limit').textContent = `Up to ${CP.bytes(data.limits.max_file_bytes)} per file, ${data.limits.max_files} files per share`;
  } catch (e) {
    CP.toast(e.message, 'err');
  }
  await Promise.all([loadShares(), loadUsage()]);
  setInterval(() => {
    if (!document.querySelector('dialog[open]') && !document.querySelector('details.more[open]')) loadShares();
  }, REFRESH_MS);
}

init();
