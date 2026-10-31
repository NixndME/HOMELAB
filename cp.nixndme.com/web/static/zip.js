// Streaming ZIP writer for Live Transfer (store only, no compression: fast, and photos/videos
// do not shrink anyway). Sizes are known before any data arrives, so every header position is
// planned up front and data is written straight to its final place: no temp copy, any size
// (ZIP64 is used automatically past 4 GiB). Also used by tests in Node (qa/zip.test.js).
'use strict';

(function (root) {
  const MAX32 = 0xffffffff;
  const MAX16 = 0xffff;
  const VERSION = 20;
  const VERSION64 = 45;
  const UTF8_FLAG = 0x0800;
  const CRC_AT = 14; // offset of the CRC field inside a local header
  const UNIX_FILE = (0o100644 << 16) >>> 0; // -rw-r--r--
  const LOCAL_LEN = 30;
  const CENTRAL_LEN = 46;
  const EOCD_LEN = 22;
  const EOCD64_LEN = 56;
  const LOCATOR_LEN = 20;
  const enc = new TextEncoder();

  const CRC_TABLE = (() => {
    const t = new Uint32Array(256);
    for (let n = 0; n < 256; n++) {
      let c = n;
      for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      t[n] = c >>> 0;
    }
    return t;
  })();

  /** Continues a CRC-32 over more bytes. Start with crc32(0, firstBytes). */
  function crc32(crc, bytes) {
    let c = (crc ^ MAX32) >>> 0;
    for (let i = 0; i < bytes.length; i++) c = CRC_TABLE[(c ^ bytes[i]) & 0xff] ^ (c >>> 8);
    return (c ^ MAX32) >>> 0;
  }

  /** MS-DOS date and time (local time, 2 s resolution, 1980 at the earliest). */
  function dosTime(ms) {
    const d = new Date(ms || Date.now());
    const year = Math.max(1980, d.getFullYear());
    return {
      time: (d.getHours() << 11) | (d.getMinutes() << 5) | (d.getSeconds() >> 1),
      date: ((year - 1980) << 9) | ((d.getMonth() + 1) << 5) | d.getDate(),
    };
  }

  /**
   * Turns a sender-supplied relative path into one that is safe on any OS: no "..", no absolute
   * paths, no characters Windows or macOS refuse.
   */
  function safePath(p) {
    const parts = String(p).replace(/\\/g, '/').split('/')
      .filter((seg) => seg !== '.' && seg !== '..')
      .map((seg) => seg.replace(/[\u0000-\u001f\u007f<>:"|?*]/g, '_').trim().replace(/[. ]+$/, ''))
      .filter((seg) => seg !== '' && seg !== '.' && seg !== '..');
    return parts.length ? parts.join('/') : 'file';
  }

  /**
   * Lays out an archive. files: [{path, size, mtime}]. opts.zip64 forces ZIP64 records (tests).
   * Returns {entries, cdOffset, total, zip64}; each entry has offset (local header) and dataOffset.
   */
  function plan(files, opts) {
    const force = !!(opts && opts.zip64);
    let offset = 0;
    let anyZip64 = force || files.length >= MAX16;
    const entries = files.map((f) => {
      const name = enc.encode(safePath(f.path));
      const zip64 = force || f.size >= MAX32;
      const headerLen = LOCAL_LEN + name.length + (zip64 ? 20 : 0);
      const farOffset = force || offset >= MAX32;
      const centralExtra = (zip64 ? 16 : 0) + (farOffset ? 8 : 0);
      const e = {
        path: f.path, name, size: f.size, mtime: f.mtime || 0, offset, headerLen,
        dataOffset: offset + headerLen, zip64, farOffset,
        centralLen: CENTRAL_LEN + name.length + (centralExtra ? 4 + centralExtra : 0), crc: 0,
      };
      anyZip64 = anyZip64 || zip64 || farOffset;
      offset += headerLen + f.size;
      return e;
    });
    const cdOffset = offset;
    const cdSize = entries.reduce((n, e) => n + e.centralLen, 0);
    const zip64 = anyZip64 || cdOffset >= MAX32 || cdSize >= MAX32;
    const tail = (zip64 ? EOCD64_LEN + LOCATOR_LEN : 0) + EOCD_LEN;
    return { entries, cdOffset, cdSize, zip64, total: cdOffset + cdSize + tail };
  }

  function writer(len) {
    const buf = new Uint8Array(len);
    const v = new DataView(buf.buffer);
    let p = 0;
    return {
      buf,
      u16(x) { v.setUint16(p, x, true); p += 2; },
      u32(x) { v.setUint32(p, x >>> 0, true); p += 4; },
      u64(x) { v.setBigUint64(p, BigInt(x), true); p += 8; },
      bytes(b) { buf.set(b, p); p += b.length; },
    };
  }

  /** The local header for a planned entry. The CRC is patched in after the data (crcPatch). */
  function localHeader(e, crc) {
    const w = writer(e.headerLen);
    const t = dosTime(e.mtime);
    w.u32(0x04034b50);
    w.u16(e.zip64 ? VERSION64 : VERSION);
    w.u16(UTF8_FLAG);
    w.u16(0); // stored
    w.u16(t.time);
    w.u16(t.date);
    w.u32(crc);
    w.u32(e.zip64 ? MAX32 : e.size);
    w.u32(e.zip64 ? MAX32 : e.size);
    w.u16(e.name.length);
    w.u16(e.zip64 ? 20 : 0);
    w.bytes(e.name);
    if (e.zip64) { w.u16(0x0001); w.u16(16); w.u64(e.size); w.u64(e.size); }
    return w.buf;
  }

  /** Four bytes to write at entry.offset + CRC_AT once the entry's data is complete. */
  function crcPatch(crc) {
    const w = writer(4);
    w.u32(crc);
    return w.buf;
  }

  /** Central directory plus end records, to write at plan.cdOffset. Uses entry.crc. */
  function centralDirectory(pl) {
    const tail = (pl.zip64 ? EOCD64_LEN + LOCATOR_LEN : 0) + EOCD_LEN;
    const w = writer(pl.cdSize + tail);
    for (const e of pl.entries) {
      const t = dosTime(e.mtime);
      const extra = (e.zip64 ? 16 : 0) + (e.farOffset ? 8 : 0);
      w.u32(0x02014b50);
      w.u16((3 << 8) | VERSION64); // made by: Unix, so file modes are honoured
      w.u16(e.zip64 || e.farOffset ? VERSION64 : VERSION);
      w.u16(UTF8_FLAG);
      w.u16(0);
      w.u16(t.time);
      w.u16(t.date);
      w.u32(e.crc);
      w.u32(e.zip64 ? MAX32 : e.size);
      w.u32(e.zip64 ? MAX32 : e.size);
      w.u16(e.name.length);
      w.u16(extra ? extra + 4 : 0);
      w.u16(0); // comment
      w.u16(0); // disk
      w.u16(0); // internal attributes
      w.u32(UNIX_FILE);
      w.u32(e.farOffset ? MAX32 : e.offset);
      w.bytes(e.name);
      if (extra) {
        w.u16(0x0001);
        w.u16(extra);
        if (e.zip64) { w.u64(e.size); w.u64(e.size); }
        if (e.farOffset) w.u64(e.offset);
      }
    }
    const n = pl.entries.length;
    if (pl.zip64) {
      const eocd64At = pl.cdOffset + pl.cdSize;
      w.u32(0x06064b50); w.u64(EOCD64_LEN - 12); w.u16((3 << 8) | VERSION64); w.u16(VERSION64);
      w.u32(0); w.u32(0); w.u64(n); w.u64(n); w.u64(pl.cdSize); w.u64(pl.cdOffset);
      w.u32(0x07064b50); w.u32(0); w.u64(eocd64At); w.u32(1);
    }
    w.u32(0x06054b50);
    w.u16(0); w.u16(0);
    w.u16(pl.zip64 ? MAX16 : n); w.u16(pl.zip64 ? MAX16 : n);
    w.u32(pl.zip64 ? MAX32 : pl.cdSize);
    w.u32(pl.zip64 ? MAX32 : pl.cdOffset);
    w.u16(0);
    return w.buf;
  }

  const api = { CRC_AT, crc32, safePath, plan, localHeader, crcPatch, centralDirectory };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.CPZip = api;
})(typeof self !== 'undefined' ? self : this);
