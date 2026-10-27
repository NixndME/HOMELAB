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
  $('sessions').textContent = `${data.sessions} active login${data.sessions === 1 ? '' : 's'} (including this one).`;
  const l = data.limits;
  $('limits').textContent = `Server limits: ${CP.bytes(l.max_file_bytes)} per file, ${l.max_files} files per share, ` +
    `${CP.bytes(l.storage_bytes)} total storage. Change them with CP_* variables in .env.`;
  $('totp-status').textContent = data.totp_enabled ? '2FA is ON: a code is needed at every login.' : '2FA is off.';
  $('totp-off').hidden = data.totp_enabled;
  $('totp-on').hidden = !data.totp_enabled;
}

$('settings-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  try {
    await CP.api('PUT', '/api/settings', {
      public_sharing: $('s-public-sharing').checked,
      default_public: $('s-default-public').checked,
      default_expiry: Number($('s-default-expiry').value),
      cleanup_interval: Number($('s-cleanup').value),
      keep_history: $('s-keep-history').checked,
      history_days: Number($('s-history-days').value) || 0,
    });
    flash('settings-msg', 'Saved', true);
  } catch (e) {
    flash('settings-msg', e.message, false);
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
