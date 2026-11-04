// Settings page: sharing/cleanup preferences, password, username, 2FA, sessions.
'use strict';

const $ = (id) => document.getElementById(id);

function flash(id, msg, ok) {
  const el = $(id);
  el.textContent = msg;
  el.className = ok ? 'muted ok-text' : 'error-text';
}

async function load() {
  const data = await CP.api('GET', '/api/settings');
  const s = data.settings;
  $('s-public-sharing').checked = s.public_sharing;
  $('s-default-public').checked = s.default_public;
  $('s-default-expiry').value = String(s.default_expiry);
  $('s-cleanup').value = String(s.cleanup_interval);
  $('s-keep-history').checked = s.keep_history;
  $('s-history-days').value = s.history_days;
  $('un-new').value = data.username;
  $('s-session-idle').value = String(s.session_idle);
  await loadSessions();
  const l = data.limits;
  $('limits').textContent = `Server limits: ${CP.bytes(l.max_file_bytes)} per file, ${l.max_files} files per share, ` +
    `${CP.bytes(l.storage_bytes)} total storage. Change them with CP_* variables in .env.`;
  $('totp-status').textContent = data.totp_enabled ? '2FA is ON: a code is needed at every login.' : '2FA is off.';
  $('totp-off').hidden = data.totp_enabled;
  $('totp-on').hidden = !data.totp_enabled;
}

// A short, readable device name from a user agent, e.g. "Firefox on Linux".
function device(ua) {
  const browser = [['Edg/', 'Edge'], ['OPR/', 'Opera'], ['Firefox/', 'Firefox'], ['Chrome/', 'Chrome'], ['Safari/', 'Safari']]
    .find(([k]) => ua.includes(k));
  const os = [['Android', 'Android'], ['iPhone', 'iPhone'], ['iPad', 'iPad'], ['Windows', 'Windows'], ['Mac OS X', 'macOS'],
    ['CrOS', 'ChromeOS'], ['Linux', 'Linux']].find(([k]) => ua.includes(k));
  if (!browser && !os) return ua ? ua.slice(0, 60) : 'Unknown device';
  return `${browser ? browser[1] : 'Browser'}${os ? ' on ' + os[1] : ''}`;
}

function sessionItem(sess) {
  const li = document.createElement('li');
  const info = document.createElement('div');
  const name = document.createElement('strong');
  name.textContent = device(sess.ua);
  info.append(name);
  if (sess.current) {
    const b = document.createElement('span');
    b.className = 'badge ok';
    b.textContent = 'This browser';
    info.append(' ', b);
  }
  const meta = document.createElement('div');
  meta.className = 'muted';
  meta.textContent = `${sess.ip || 'unknown IP'} · logged in ${CP.ago(sess.created_at)} · active ${CP.ago(sess.last_seen)}`;
  meta.title = sess.ua;
  info.append(meta);
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'btn small danger-outline';
  btn.textContent = 'Log out';
  btn.addEventListener('click', () => endSession(sess, btn));
  li.append(info, btn);
  return li;
}

async function loadSessions() {
  const r = await CP.api('GET', '/api/account/sessions');
  const n = r.sessions.length;
  $('sessions').textContent = `${n} of at most ${r.max} logins active. A new login beyond ${r.max} ends the least recently used one.`;
  $('session-list').replaceChildren(...r.sessions.map(sessionItem));
}

async function endSession(sess, btn) {
  if (sess.current && !CP.armed(btn, 'Click again: log out here')) return;
  try {
    await CP.api('DELETE', `/api/account/sessions/${sess.id}`);
    if (sess.current) { location.href = '/login'; return; }
    flash('sess-msg', 'Logged out that browser', true);
    await loadSessions();
  } catch (e) {
    flash('sess-msg', e.message, false);
  }
}

function saveSettings() {
  return CP.api('PUT', '/api/settings', {
    public_sharing: $('s-public-sharing').checked,
    default_public: $('s-default-public').checked,
    default_expiry: Number($('s-default-expiry').value),
    cleanup_interval: Number($('s-cleanup').value),
    keep_history: $('s-keep-history').checked,
    history_days: Number($('s-history-days').value) || 0,
    session_idle: Number($('s-session-idle').value),
  });
}

$('settings-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  try {
    await saveSettings();
    flash('settings-msg', 'Saved', true);
  } catch (e) {
    flash('settings-msg', e.message, false);
  }
});

$('idle-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  try {
    await saveSettings();
    flash('idle-msg', 'Saved', true);
  } catch (e) {
    flash('idle-msg', e.message, false);
  }
});

$('clear-history').addEventListener('click', async (ev) => {
  if (!CP.armed(ev.currentTarget, 'Click again to clear')) return;
  try {
    const r = await CP.api('POST', '/api/history/clear');
    CP.toast(`Removed ${r.removed} ended share${r.removed === 1 ? '' : 's'} from history`, 'ok');
  } catch (e) {
    CP.toast(e.message, 'err');
  }
});

$('password-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  if ($('pw-new').value !== $('pw-repeat').value) { flash('pw-msg', 'New passwords do not match', false); return; }
  try {
    const r = await CP.api('POST', '/api/account/password', { current: $('pw-current').value, new: $('pw-new').value });
    ev.target.reset();
    flash('pw-msg', `Password changed. ${r.other_sessions_ended} other login(s) ended.`, true);
    load();
  } catch (e) {
    flash('pw-msg', e.message, false);
  }
});

$('username-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  try {
    await CP.api('POST', '/api/account/username', { username: $('un-new').value, password: $('un-password').value });
    $('un-password').value = '';
    flash('un-msg', 'Username changed', true);
  } catch (e) {
    flash('un-msg', e.message, false);
  }
});

$('totp-setup').addEventListener('click', async () => {
  try {
    const r = await CP.api('POST', '/api/account/totp/setup');
    $('totp-qr').src = r.qr;
    $('totp-secret').textContent = r.secret;
    $('totp-enroll').hidden = false;
    $('totp-code').focus();
  } catch (e) {
    CP.toast(e.message, 'err');
  }
});

$('totp-enable').addEventListener('click', async () => {
  try {
    await CP.api('POST', '/api/account/totp/enable', { code: $('totp-code').value });
    $('totp-enroll').hidden = true;
    CP.toast('2FA is on', 'ok');
    load();
  } catch (e) {
    flash('totp-msg', e.message, false);
  }
});

$('totp-on').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  try {
    await CP.api('POST', '/api/account/totp/disable', { password: $('totp-password').value });
    ev.target.reset();
    CP.toast('2FA is off', 'ok');
    load();
  } catch (e) {
    flash('totp-off-msg', e.message, false);
  }
});

$('logout-others').addEventListener('click', async () => {
  try {
    const r = await CP.api('POST', '/api/account/logout-others');
    flash('sess-msg', `${r.ended} other login(s) ended`, true);
    load();
  } catch (e) {
    flash('sess-msg', e.message, false);
  }
});

load().catch((e) => CP.toast(e.message, 'err'));
