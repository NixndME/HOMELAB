// Service worker: receives shares from the phone's share sheet (Web Share Target).
// It keeps what was shared in this browser (IndexedDB) and opens the dashboard, which
// pre-fills the New share dialog and uploads it the normal, chunked way. Nothing else is
// intercepted or cached: every other request goes straight to the server.
'use strict';

const DB = 'cp-share-target';
const STORE = 'pending';

self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()));

function openDB() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB, 1);
    req.onupgradeneeded = () => req.result.createObjectStore(STORE);
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

async function stash(item) {
  const db = await openDB();
  try {
    await new Promise((resolve, reject) => {
      const tx = db.transaction(STORE, 'readwrite');
      tx.objectStore(STORE).put(item, 'latest'); // one pending share; a newer one replaces it
      tx.oncomplete = resolve;
      tx.onerror = () => reject(tx.error);
    });
  } finally {
    db.close();
  }
}

const MAX_SHARED_BYTES = 1024 * 1024 * 1024; // bigger shares belong in Live transfer

// The system share sheet sends no referrer. A form on another site that posts here does
// (unless it hides it); those are refused, so a site cannot quietly pre-fill the dialog.
function fromOtherSite(request) {
  if (!request.referrer || request.referrer === 'about:client') return false;
  try {
    return new URL(request.referrer).origin !== self.location.origin;
  } catch (e) {
    return true;
  }
}

async function receive(request) {
  if (fromOtherSite(request)) return Response.redirect('/', 303);
  try {
    const form = await request.formData();
    const files = form.getAll('files').filter((f) => f instanceof File && f.size > 0);
    if (files.reduce((n, f) => n + f.size, 0) > MAX_SHARED_BYTES) return Response.redirect('/?shared=toobig', 303);
    await stash({
      at: Date.now(),
      title: String(form.get('title') || ''),
      text: String(form.get('text') || ''),
      url: String(form.get('url') || ''),
      files,
    });
    return Response.redirect('/?shared=1', 303);
  } catch (e) {
    return Response.redirect('/?shared=failed', 303);
  }
}

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method === 'POST' && url.origin === self.location.origin && url.pathname === '/share-target') {
    e.respondWith(receive(e.request));
  }
});
