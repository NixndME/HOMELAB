// Share page: copy the full text, show all, wrap toggle, file previews.
'use strict';

document.addEventListener('click', async (ev) => {
  const raw = ev.target.closest('[data-copy-raw]');
  if (raw) {
    raw.disabled = true;
    try {
      const res = await fetch(raw.dataset.copyRaw, { credentials: 'same-origin' });
      if (!res.ok) throw new Error('could not load the text');
      const ok = await CP.copyText(await res.text());
      CP.toast(ok ? 'Full text copied' : 'Copy failed; use Download', ok ? 'ok' : 'err');
    } catch (e) {
      CP.toast(e.message, 'err');
    } finally {
      raw.disabled = false;
    }
    return;
  }
  const prev = ev.target.closest('[data-preview]');
  if (prev) openPreview(prev.dataset);
});

// "Show all" swaps the truncated preview for the full text, on the same page.
document.addEventListener('click', async (ev) => {
  const btn = ev.target.closest('[data-show-all]');
  if (!btn) return;
  btn.disabled = true;
  btn.textContent = 'Loading…';
  try {
    const res = await fetch(btn.dataset.showAll, { credentials: 'same-origin' });
    if (!res.ok) throw new Error('could not load the full text');
    const pre = document.createElement('pre');
    pre.className = 'code plain';
    pre.id = 'code';
    pre.textContent = await res.text();
    document.getElementById('code').replaceWith(pre);
    btn.remove();
  } catch (e) {
    CP.toast(e.message, 'err');
    btn.disabled = false;
    btn.textContent = 'Show all';
  }
});

// Delegated, because on the dashboard this content is inserted into the view popup later.
document.addEventListener('click', (ev) => {
  const wrap = ev.target.closest('#wrap-toggle');
  if (!wrap) return;
  const on = document.getElementById('code').classList.toggle('wrap');
  wrap.setAttribute('aria-pressed', String(on));
});

// File preview: its own window on top of everything, so Esc or a click outside closes just
// the preview and leaves the share (or the dashboard popup) open underneath.
function previewDialog() {
  let dlg = document.getElementById('file-preview');
  if (dlg) return dlg;
  dlg = document.createElement('dialog');
  dlg.id = 'file-preview';
  dlg.className = 'modal preview';
  dlg.innerHTML = '<header class="modal-head"><h2 id="fp-name"></h2><div class="actions">' +
    '<a class="btn small" id="fp-download">Download</a>' +
    '<button type="button" class="icon-btn" data-close aria-label="Close preview">✕</button></div></header>' +
    '<div id="fp-body" class="fp-body"></div>';
  dlg.addEventListener('click', (ev) => { if (ev.target === dlg) dlg.close(); });
  dlg.addEventListener('close', () => document.getElementById('fp-body').replaceChildren());
  document.body.appendChild(dlg);
  return dlg;
}

async function openPreview({ preview, type, name, download }) {
  const dlg = previewDialog();
  document.getElementById('fp-name').textContent = name || 'Preview';
  document.getElementById('fp-download').href = download;
  const body = document.getElementById('fp-body');
  let node;
  if (type === 'image') {
    node = document.createElement('img');
    node.alt = name || 'Preview';
    node.src = preview;
  } else if (type === 'pdf') {
    node = document.createElement('iframe');
    node.title = name || 'PDF preview';
    node.src = preview;
  } else {
    node = document.createElement('pre');
    node.className = 'code plain';
    try {
      const res = await fetch(preview, { credentials: 'same-origin' });
      if (!res.ok) throw new Error('could not load the file');
      node.textContent = await res.text();
    } catch (e) {
      CP.toast(e.message, 'err');
      return;
    }
  }
  body.replaceChildren(node);
  if (!dlg.open) dlg.showModal();
}
