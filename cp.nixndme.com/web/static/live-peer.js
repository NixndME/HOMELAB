// Live Transfer connection: the server's event stream carries the WebRTC handshake, then the two
// browsers talk directly over an RTCDataChannel (encrypted by DTLS, local network when possible).
'use strict';

const LivePeer = (() => {
  const root = document.getElementById('live');
  const ICE = (() => { try { return JSON.parse(root.dataset.ice || '[]'); } catch (e) { return []; } })();
  const CONNECT_TIMEOUT_MS = 30000;
  const NO_ROUTE = 'The two devices could not reach each other directly. Make sure both are on the same ' +
    'home network (not a guest Wi-Fi with client isolation) and that no VPN is running.';

  const supported = () => typeof window.RTCPeerConnection === 'function';

  /**
   * Connects to the other device of transfer `id`. Resolves with {pc, dc, stopSignaling} once
   * the data channel is open. `signal` (AbortSignal) cancels; `onStatus(text)` reports progress.
   * @param {{id: string, role: 'sender'|'receiver', signal: AbortSignal, onStatus: (s: string) => void}} o
   */
  function connect({ id, role, signal, onStatus }) {
    return new Promise((resolve, reject) => {
      if (signal.aborted) { reject(new DOMException('Cancelled', 'AbortError')); return; }
      const pc = new RTCPeerConnection({ iceServers: ICE });
      const es = new EventSource(`/api/live/${encodeURIComponent(id)}/events?role=${role}`);
      const pendingIce = [];
      let remoteSet = false;
      let settled = false;
      let timer = null;
      let chain = Promise.resolve();

      const stopSignaling = () => es.close();
      const fail = (err) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        es.close();
        pc.close();
        reject(err);
      };
      const startTimer = () => { timer = setTimeout(() => fail(new Error(NO_ROUTE)), CONNECT_TIMEOUT_MS); };
      const post = (msg) => CP.api('POST', `/api/live/${encodeURIComponent(id)}/signal?role=${role}`, msg).catch(fail);

      signal.addEventListener('abort', () => fail(new DOMException('Cancelled', 'AbortError')), { once: true });

      const opened = (dc) => {
        dc.binaryType = 'arraybuffer';
        const done = () => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          resolve({ pc, dc, stopSignaling });
        };
        if (dc.readyState === 'open') done();
        else dc.addEventListener('open', done, { once: true });
      };

      pc.onicecandidate = (ev) => { if (ev.candidate) post({ type: 'ice', candidate: ev.candidate.toJSON() }); };
      pc.onconnectionstatechange = () => { if (pc.connectionState === 'failed') fail(new Error(NO_ROUTE)); };
      if (role === 'receiver') {
        pc.ondatachannel = (ev) => opened(ev.channel);
        startTimer();
      }

      const setRemote = async (type, sdp) => {
        await pc.setRemoteDescription({ type, sdp });
        remoteSet = true;
        for (const c of pendingIce.splice(0)) await pc.addIceCandidate(c);
      };

      async function handle(msg) {
        switch (msg.type) {
          case 'joined': { // sender: a receiver accepted, start the handshake
            onStatus('Connecting to the other device…');
            startTimer();
            opened(pc.createDataChannel('files', { ordered: true }));
            const offer = await pc.createOffer();
            await pc.setLocalDescription(offer);
            await post({ type: 'offer', sdp: offer.sdp });
            return;
          }
          case 'offer': {
            onStatus('Connecting to the other device…');
            await setRemote('offer', msg.sdp);
            const answer = await pc.createAnswer();
            await pc.setLocalDescription(answer);
            await post({ type: 'answer', sdp: answer.sdp });
            return;
          }
          case 'answer':
            await setRemote('answer', msg.sdp);
            return;
          case 'ice':
            if (remoteSet) await pc.addIceCandidate(msg.candidate);
            else pendingIce.push(msg.candidate);
            return;
          default:
        }
      }

      es.onmessage = (ev) => {
        let msg;
        try { msg = JSON.parse(ev.data); } catch (e) { return; }
        chain = chain.then(() => handle(msg)).catch(fail);
      };
      es.addEventListener('end', () => fail(new Error('The transfer was closed on the other device.')));
      es.onerror = () => { if (es.readyState === EventSource.CLOSED) fail(new Error('Lost contact with cp. Check the connection and try again.')); };
    });
  }

  return { connect, supported };
})();
