// Shared helpers for every page: API calls, toasts, copy, QR dialog, theme, relative times.
'use strict';

const CP = (() => {
  const csrfMeta = document.querySelector('meta[name="csrf"]');
  const csrf = csrfMeta ? csrfMeta.content : '';

  async function api(method, path, body) {
    const opts = { method, headers: { 'X-CSRF-Token': csrf }, credentials: 'same-origin' };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    if (res.status === 401) { location.href = '/login'; throw new Error('Logged out'); }
    if (res.status === 204) return null;
    let data = null;
    try { data = await res.json(); } catch (e) { data = null; }
    if (!res.ok) throw new Error((data && data.error) || `Request failed (${res.status})`);
    return data;
  }

  let toastTimer = null;
  function toast(msg, kind) {
    const el = document.getElementById('toast');
    if (!el) return;
    el.textContent = msg;
    el.className = 'show ' + (kind || '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { el.className = ''; }, 3200);
  }

  async function copyText(text) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch (e) {
      // Fallback for browsers that block the async clipboard API.
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      let ok = false;
      try { ok = document.execCommand('copy'); } catch (err) { ok = false; }
      ta.remove();
      return ok;
    }
  }

  function bytes(n) {
    if (n < 1024) return n + ' B';
    const units = ['KB', 'MB', 'GB', 'TB'];
    let v = n / 1024, i = 0;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return v.toFixed(v < 10 ? 1 : 0) + ' ' + units[i];
  }

  function duration(sec) {
    sec = Math.max(0, Math.round(sec));
    if (sec < 60) return sec + 's';
    const m = Math.round(sec / 60);
    if (m < 60) return m + ' min';
    const h = Math.round(sec / 3600);
    if (h < 48) return h + ' h';
    return Math.round(sec / 86400) + ' days';
  }

  const now = () => Date.now() / 1000;
  const left = (ts) => (ts - now() <= 0 ? 'expired' : 'in ' + duration(ts - now()));
  const ago = (ts) => duration(now() - ts) + ' ago';
  const date = (ts) => new Date(ts * 1000).toLocaleString();

  function openQR(slug) {
    const dlg = document.getElementById('qr-dialog');
    if (!dlg) return;
    document.getElementById('qr-img').src = '/qr/' + encodeURIComponent(slug) + '?t=' + Date.now();
    const url = document.getElementById('qr-url');
    if (url) url.textContent = location.origin + '/s/' + slug;
    dlg.showModal();
  }

  // Two-click confirmation instead of window.confirm(): first click arms, second acts.
  function armed(btn, label) {
    if (btn.dataset.armed === '1') return true;
    const original = btn.textContent;
    btn.dataset.armed = '1';
    btn.textContent = label || 'Sure?';
    btn.classList.add('armed');
    setTimeout(() => { btn.dataset.armed = ''; btn.textContent = original; btn.classList.remove('armed'); }, 3000);
    return false;
  }

  function refreshTimes(root) {
    (root || document).querySelectorAll('[data-left]').forEach((el) => {
      el.textContent = left(Number(el.dataset.left));
      el.title = date(Number(el.dataset.left));
    });
  }

  document.addEventListener('click', async (ev) => {
    const copyBtn = ev.target.closest('[data-copy]');
    if (copyBtn) {
      toast((await copyText(copyBtn.dataset.copy)) ? 'Copied' : 'Copy failed', 'ok');
      return;
    }
    const qrBtn = ev.target.closest('[data-qr]');
    if (qrBtn) { openQR(qrBtn.dataset.qr); return; }
    const closeBtn = ev.target.closest('[data-close]');
    if (closeBtn) { const d = closeBtn.closest('dialog'); if (d) d.close(); }
  });

  const themeBtn = document.getElementById('theme-toggle');
  if (themeBtn) {
    themeBtn.addEventListener('click', () => {
      const next = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
      document.documentElement.setAttribute('data-theme', next);
      try { localStorage.setItem('cp-theme', next); } catch (e) { /* not persisted, fine */ }
    });
  }

  refreshTimes();
  setInterval(() => refreshTimes(), 30000);

  return { api, toast, copyText, bytes, duration, left, ago, date, openQR, armed, refreshTimes, csrf };
})();
