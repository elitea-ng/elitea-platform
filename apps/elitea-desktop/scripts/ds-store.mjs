#!/usr/bin/env node
// A Finder .DS_Store and the binary property lists inside it: read and write, as much
// as the disk image needs (scripts/macos-dmg.sh, dmg-verify.mjs).
//
// THE WINDOW'S CHROME. Finder's AppleScript can hide the toolbar and the status bar,
// but not the tab bar, the path bar or the sidebar, and a Finder that shows tabs
// (macOS 26 and later) writes `ShowTabView: true` into the window settings (`bwsp`).
// So after Finder has laid the window out, `hide-chrome` rewrites that one record with
// every bar off, keeping the window's bounds and every other record byte for byte:
//
//   node scripts/ds-store.mjs hide-chrome <path/to/.DS_Store>
//
// The format: a buddy allocator whose blocks are addressed by offset with the log2 of
// their size in the low 5 bits (offsets count from byte 4), a directory naming the
// `DSDB` block, and a B-tree of records sorted by name (case-insensitively), then code:
// `<name length u32><name UTF-16BE><code 4cc><type 4cc><value>`.
// Node's standard library only.

import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

/** Bytes a value of each .DS_Store record type takes (a `blob` and `ustr` carry their length). */
const FIXED = { long: 4, shor: 4, bool: 1, type: 4, comp: 8, dutc: 8 };

/** The page size of a B-tree node, as Finder writes it. */
const PAGE = 0x1000;

/**
 * Every record in the live B-tree of the .DS_Store `buf` (never a stale one left in a
 * free block), as `{ name, code, type, value, raw }`: `value` a Buffer for `blob`, a
 * string for `ustr`, a number for the 1- and 4-byte types; `raw` the value's bytes as
 * stored, so a record is written back unchanged. Throws on a file that is not a .DS_Store.
 */
export function dsStoreRecords(buf) {
  if (buf.length < 36 || buf.readUInt32BE(0) !== 1 || buf.toString('latin1', 4, 8) !== 'Bud1') throw new Error('not a .DS_Store (no Bud1 header)');
  const at = (offset) => offset + 4;
  let p = at(buf.readUInt32BE(8));
  const blockCount = buf.readUInt32BE(p);
  p += 8;
  const blocks = [];
  for (let i = 0; i < blockCount; i++) blocks.push(buf.readUInt32BE(p + i * 4));
  // The block table is padded to a multiple of 256 entries.
  p += Math.ceil(blockCount / 256) * 256 * 4;
  const dirCount = buf.readUInt32BE(p);
  p += 4;
  const directory = new Map();
  for (let i = 0; i < dirCount; i++) {
    const len = buf.readUInt8(p);
    directory.set(buf.toString('latin1', p + 1, p + 1 + len), buf.readUInt32BE(p + 1 + len));
    p += 1 + len + 4;
  }
  const block = (id) => {
    if (id >= blocks.length) throw new Error(`.DS_Store: block ${id} is out of range`);
    return at(blocks[id] & ~0x1f);
  };
  if (!directory.has('DSDB')) throw new Error('.DS_Store: no DSDB tree');
  const root = buf.readUInt32BE(block(directory.get('DSDB')));
  const records = [];
  const readRecord = (q) => {
    const nameLen = buf.readUInt32BE(q);
    const name = Buffer.from(buf.subarray(q + 4, q + 4 + nameLen * 2)).swap16().toString('utf16le');
    q += 4 + nameLen * 2;
    const code = buf.toString('latin1', q, q + 4);
    const type = buf.toString('latin1', q + 4, q + 8);
    q += 8;
    const start = q;
    let value;
    if (type === 'blob') {
      const len = buf.readUInt32BE(q);
      value = buf.subarray(q + 4, q + 4 + len);
      q += 4 + len;
    } else if (type === 'ustr') {
      const len = buf.readUInt32BE(q);
      value = Buffer.from(buf.subarray(q + 4, q + 4 + len * 2)).swap16().toString('utf16le');
      q += 4 + len * 2;
    } else if (type in FIXED) {
      value = FIXED[type] === 1 ? buf.readUInt8(q) : buf.readUInt32BE(q);
      q += FIXED[type];
    } else {
      throw new Error(`.DS_Store: record "${name}" ${code} has an unknown type ${type}`);
    }
    if (q > buf.length) throw new Error(`.DS_Store: record "${name}" ${code} runs past the end of the file`);
    records.push({ name, code, type, value, raw: Buffer.from(buf.subarray(start, q)) });
    return q;
  };
  const walk = (id, depth) => {
    if (depth > 32) throw new Error('.DS_Store: the tree is too deep');
    let q = block(id);
    const next = buf.readUInt32BE(q);
    const count = buf.readUInt32BE(q + 4);
    q += 8;
    for (let i = 0; i < count; i++) {
      // An inner node: each record follows its left child; `next` is the rightmost child.
      if (next !== 0) {
        walk(buf.readUInt32BE(q), depth + 1);
        q += 4;
      }
      q = readRecord(q);
    }
    if (next !== 0) walk(next, depth + 1);
  };
  walk(root, 0);
  return records;
}

/** A `blob` record for `writeDsStore`. */
export function blobRecord(name, code, bytes) {
  const raw = Buffer.alloc(4 + bytes.length);
  raw.writeUInt32BE(bytes.length);
  bytes.copy(raw, 4);
  return { name, code, type: 'blob', value: bytes, raw };
}

const u32 = (...vs) => {
  const b = Buffer.alloc(vs.length * 4);
  vs.forEach((v, i) => b.writeUInt32BE(v >>> 0, i * 4));
  return b;
};

function encodeRecord(r) {
  return Buffer.concat([u32(r.name.length), Buffer.from(r.name, 'utf16le').swap16(), Buffer.from(r.code + r.type, 'latin1'), r.raw]);
}

/** Finder's order: the name case-insensitively (UTF-16 code units), then the code. */
export function compareRecords(a, b) {
  const x = a.name.toLowerCase();
  const y = b.name.toLowerCase();
  if (x !== y) return x < y ? -1 : 1;
  return a.code < b.code ? -1 : a.code > b.code ? 1 : 0;
}

/**
 * A .DS_Store holding `records` (each with `raw`, as dsStoreRecords returns them): a
 * single leaf when they fit one page, else leaves under one inner node.
 */
export function writeDsStore(records) {
  const sorted = [...records].sort(compareRecords);
  const encoded = sorted.map(encodeRecord);
  // Leaves: records packed into pages; between two leaves one record moves up into the inner node.
  const room = PAGE - 8;
  const leaves = [];
  const separators = [];
  let current = [];
  let used = 0;
  for (const e of encoded) {
    if (e.length > room - 4) throw new Error('.DS_Store: a record does not fit a page');
    if (used + e.length > room && current.length) {
      leaves.push(current);
      current = [];
      used = 0;
      separators.push(e);
      continue;
    }
    current.push(e);
    used += e.length;
  }
  leaves.push(current);
  if (separators.length && current.length === 0) {
    // The last record became a separator with nothing after it: it opens the last
    // leaf instead, and the record before it moves up in its place.
    const before = leaves[leaves.length - 2];
    if (before.length < 2) throw new Error('.DS_Store: records too large to split into pages');
    leaves[leaves.length - 1] = [separators.pop()];
    separators.push(before.pop());
  }
  // Blocks: 0 the allocator root, 1 DSDB, then the nodes (the root node first).
  const nodes = [];
  const leafIds = leaves.map((_, i) => 3 + i);
  if (leaves.length === 1) {
    nodes.push(Buffer.concat([u32(0, leaves[0].length), ...leaves[0]]));
  } else {
    const inner = [u32(leafIds[leafIds.length - 1], separators.length)];
    separators.forEach((s, i) => inner.push(u32(leafIds[i]), s));
    nodes.push(Buffer.concat(inner));
    for (const leaf of leaves) nodes.push(Buffer.concat([u32(0, leaf.length), ...leaf]));
  }
  if (nodes.some((n) => n.length > PAGE)) throw new Error('.DS_Store: too many records for one inner node');
  // Offsets count from byte 4: the allocator root at 0x800 (2 KiB), DSDB at 0x20 (32 B),
  // each node on its own 4 KiB page from 0x1000.
  const offsets = [0x800, 0x20, ...nodes.map((_, i) => PAGE * (1 + i))];
  const log2 = [11, 5, ...nodes.map(() => 12)];
  const rootNode = 2;
  const levels = leaves.length === 1 ? 0 : 1;
  const dsdb = u32(rootNode, levels, sorted.length, nodes.length, PAGE);
  // Free lists (32, one per size): the gaps between 0x40 and 0x800 are free, one block each size.
  const free = [];
  for (let i = 0; i < 32; i++) free.push(i >= 6 && i <= 10 ? u32(1, 1 << i) : u32(0));
  const allocator = Buffer.concat([
    u32(offsets.length, 0),
    u32(...offsets.map((o, i) => o | log2[i])),
    Buffer.alloc((256 - offsets.length) * 4),
    u32(1),
    Buffer.from([4]),
    Buffer.from('DSDB', 'latin1'),
    u32(1),
    ...free,
  ]);
  if (allocator.length > 0x800) throw new Error('.DS_Store: the allocator does not fit its block');
  const end = PAGE * (1 + nodes.length);
  const file = Buffer.alloc(4 + end);
  file.writeUInt32BE(1, 0);
  file.write('Bud1', 4, 'latin1');
  u32(0x800, 0x800, 0x800).copy(file, 8);
  allocator.copy(file, 4 + 0x800);
  dsdb.copy(file, 4 + 0x20);
  nodes.forEach((n, i) => n.copy(file, 4 + PAGE * (1 + i)));
  return file;
}

/**
 * The value of a binary property list (`bplist00`): dictionaries, arrays, strings,
 * integers, reals, booleans and data, which is all Finder's view records hold.
 */
export function parseBplist(buf) {
  if (buf.toString('latin1', 0, 8) !== 'bplist00') throw new Error('not a binary property list');
  const t = buf.length - 32;
  const offsetSize = buf.readUInt8(t + 6);
  const refSize = buf.readUInt8(t + 7);
  const count = Number(buf.readBigUInt64BE(t + 8));
  const top = Number(buf.readBigUInt64BE(t + 16));
  const table = Number(buf.readBigUInt64BE(t + 24));
  const uint = (pos, size) => {
    let v = 0;
    for (let i = 0; i < size; i++) v = v * 256 + buf[pos + i];
    return v;
  };
  const offsetOf = (ref) => {
    if (ref >= count) throw new Error('bplist: object reference out of range');
    return uint(table + ref * offsetSize, offsetSize);
  };
  const read = (ref, depth) => {
    if (depth > 32) throw new Error('bplist: too deep');
    let pos = offsetOf(ref);
    const marker = buf[pos];
    const kind = marker >> 4;
    let n = marker & 0x0f;
    if (kind === 0) {
      if (n === 8) return false;
      if (n === 9) return true;
      return null;
    }
    if (kind === 1) return uint(pos + 1, 1 << n);
    if (kind === 2) return n === 2 ? buf.readFloatBE(pos + 1) : buf.readDoubleBE(pos + 1);
    pos += 1;
    if (n === 0x0f) {
      const bytes = 1 << (buf[pos] & 0x0f);
      n = uint(pos + 1, bytes);
      pos += 1 + bytes;
    }
    if (kind === 4) return buf.subarray(pos, pos + n);
    if (kind === 5) return buf.toString('latin1', pos, pos + n);
    if (kind === 6) return Buffer.from(buf.subarray(pos, pos + n * 2)).swap16().toString('utf16le');
    if (kind === 0xa) return Array.from({ length: n }, (_, i) => read(uint(pos + i * refSize, refSize), depth + 1));
    if (kind === 0xd) {
      const out = {};
      for (let i = 0; i < n; i++) out[read(uint(pos + i * refSize, refSize), depth + 1)] = read(uint(pos + (n + i) * refSize, refSize), depth + 1);
      return out;
    }
    throw new Error(`bplist: unsupported object type 0x${marker.toString(16)}`);
  };
  return read(top, 0);
}

/**
 * A binary property list of `value`: plain objects (dictionaries), arrays, strings,
 * integers (non-negative, below 2^32), finite non-integer numbers (reals), booleans
 * and Buffers (data).
 */
export function encodeBplist(value) {
  const objects = [];
  const length = (kind, n) => {
    if (n < 15) return Buffer.from([(kind << 4) | n]);
    return Buffer.concat([Buffer.from([(kind << 4) | 0x0f]), int(n)]);
  };
  function int(n) {
    if (!Number.isInteger(n) || n < 0 || n >= 2 ** 32) throw new Error(`bplist: integer ${n} is out of range`);
    if (n < 0x100) return Buffer.from([0x10, n]);
    if (n < 0x10000) return Buffer.from([0x11, n >> 8, n & 0xff]);
    return Buffer.concat([Buffer.from([0x12]), u32(n)]);
  }
  const add = (v) => {
    const at = objects.length;
    objects.push(null);
    let bytes;
    if (typeof v === 'boolean') bytes = Buffer.from([v ? 0x09 : 0x08]);
    else if (typeof v === 'number' && Number.isInteger(v)) bytes = int(v);
    else if (typeof v === 'number') {
      bytes = Buffer.alloc(9);
      bytes[0] = 0x23;
      bytes.writeDoubleBE(v, 1);
    } else if (typeof v === 'string') {
      // ASCII as type 5, anything else as UTF-16BE (type 6).
      bytes = /^[\x00-\x7f]*$/.test(v) ? Buffer.concat([length(5, v.length), Buffer.from(v, 'latin1')]) : Buffer.concat([length(6, v.length), Buffer.from(v, 'utf16le').swap16()]);
    } else if (Buffer.isBuffer(v)) bytes = Buffer.concat([length(4, v.length), v]);
    else if (Array.isArray(v)) {
      const refs = v.map(add);
      bytes = { kind: 0xa, n: v.length, refs };
    } else if (v && typeof v === 'object') {
      const keys = Object.keys(v);
      const refs = [...keys.map(add), ...keys.map((k) => add(v[k]))];
      bytes = { kind: 0xd, n: keys.length, refs };
    } else throw new Error(`bplist: cannot encode ${typeof v}`);
    objects[at] = bytes;
    return at;
  };
  add(value);
  const refSize = objects.length < 0x100 ? 1 : 2;
  const ref = (r) => (refSize === 1 ? Buffer.from([r]) : Buffer.from([r >> 8, r & 0xff]));
  const bodies = objects.map((o) => (Buffer.isBuffer(o) ? o : Buffer.concat([length(o.kind, o.n), ...o.refs.map(ref)])));
  const head = Buffer.from('bplist00', 'latin1');
  const offsets = [];
  let pos = head.length;
  for (const b of bodies) {
    offsets.push(pos);
    pos += b.length;
  }
  const offsetSize = pos < 0x100 ? 1 : pos < 0x10000 ? 2 : 4;
  const table = Buffer.alloc(offsets.length * offsetSize);
  offsets.forEach((o, i) => table.writeUIntBE(o, i * offsetSize, offsetSize));
  const trailer = Buffer.alloc(32);
  trailer.writeUInt8(offsetSize, 6);
  trailer.writeUInt8(refSize, 7);
  trailer.writeBigUInt64BE(BigInt(objects.length), 8);
  trailer.writeBigUInt64BE(0n, 16);
  trailer.writeBigUInt64BE(BigInt(pos), 24);
  return Buffer.concat([head, ...bodies, table, trailer]);
}

/** The window settings with every bar hidden: what `hide-chrome` writes into `bwsp`. */
export const HIDDEN_CHROME = Object.freeze({
  ShowToolbar: false,
  ShowStatusBar: false,
  ShowTabView: false,
  ShowPathbar: false,
  ShowSidebar: false,
  ContainerShowSidebar: false,
});

/**
 * The .DS_Store `buf` with the root window's settings (`.`, `bwsp`) set to
 * HIDDEN_CHROME, its other keys (the bounds) kept; every other record unchanged.
 */
export function hideChrome(buf) {
  const records = dsStoreRecords(buf);
  const i = records.findIndex((r) => r.name === '.' && r.code === 'bwsp');
  if (i < 0) throw new Error('.DS_Store has no window settings (bwsp) to rewrite: Finder did not save the window');
  const settings = { ...parseBplist(records[i].value), ...HIDDEN_CHROME };
  records[i] = blobRecord('.', 'bwsp', encodeBplist(settings));
  return writeDsStore(records);
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const [command, file] = process.argv.slice(2);
  try {
    if (command !== 'hide-chrome' || !file) throw new Error('usage: ds-store.mjs hide-chrome <path/to/.DS_Store>');
    writeFileSync(file, hideChrome(readFileSync(file)));
    console.log(`${file}: window chrome hidden (${Object.keys(HIDDEN_CHROME).join(', ')} false)`);
  } catch (error) {
    console.log(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
