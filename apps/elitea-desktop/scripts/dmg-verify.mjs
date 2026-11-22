#!/usr/bin/env node
// Proves a mounted disk image is laid out as settings.json says (scripts/dmg-settings.mjs),
// instead of trusting that Finder applied it: macos-dmg.sh runs it after styling the
// writable image, and desktop-macos-package runs it again on the finished .dmg.
//
//   node scripts/dmg-verify.mjs <mount-point> <settings.json>
//
// Checks: the app bundle, `Applications` as a symlink to /Applications, the background
// (.background/background.tiff) and the volume icon (.VolumeIcon.icns) are there; and
// the .DS_Store Finder wrote holds, in its live B-tree (never a stale record left in a
// free block), the icon view (`icvp`: a picture background whose alias names
// background.tiff, the icon and text sizes, no arrangement), the window (`bwsp`: its
// size, no toolbar, no status bar) and each icon's position (`Iloc`).
//
// The .DS_Store format: a buddy allocator holding a B-tree of records
// `<name length u32><name UTF-16BE><code 4cc><type 4cc><value>`; `icvp` and `bwsp` are
// binary property lists. Both readers below take only what this check needs.
// Node's standard library only.

import { existsSync, lstatSync, readFileSync, readlinkSync } from 'node:fs';
import { join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

/** Bytes a value of each .DS_Store record type takes (a `blob` and `ustr` carry their length). */
const FIXED = { long: 4, shor: 4, bool: 1, type: 4, comp: 8, dutc: 8 };

/**
 * Every record in the live B-tree of the .DS_Store `buf`, as `{ name, code, type, value }`
 * (`value` a Buffer for `blob`, a string for `ustr`, a number for the 4-byte types).
 * Throws on a file that is not a .DS_Store.
 */
export function dsStoreRecords(buf) {
  if (buf.length < 36 || buf.readUInt32BE(0) !== 1 || buf.toString('latin1', 4, 8) !== 'Bud1') throw new Error('not a .DS_Store (no Bud1 header)');
  // Every offset in the file counts from byte 4.
  const at = (offset) => offset + 4;
  const rootOffset = buf.readUInt32BE(8);
  let p = at(rootOffset);
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
    const name = buf.toString('latin1', p + 1, p + 1 + len);
    directory.set(name, buf.readUInt32BE(p + 1 + len));
    p += 1 + len + 4;
  }
  // A block's address: its offset with the log2 of its size in the low 5 bits.
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
    records.push({ name, code, type, value });
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
      const sizeMarker = buf[pos];
      const bytes = 1 << (sizeMarker & 0x0f);
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

const record = (records, name, code) => records.find((r) => r.name === name && r.code === code);

/** An icon's position (`Iloc` blob: x u32, y u32, then 8 bytes) for `name`, or null. */
export function iconLocation(records, name) {
  const r = record(records, name, 'Iloc');
  if (!r || r.type !== 'blob' || r.value.length < 8) return null;
  return { x: r.value.readUInt32BE(0), y: r.value.readUInt32BE(4) };
}

/** The problems with the .DS_Store bytes `dsStore` against `settings`; empty when it is right. */
export function layoutProblems(dsStore, settings) {
  let records;
  try {
    records = dsStoreRecords(dsStore);
  } catch (error) {
    return [error.message];
  }
  const problems = [];
  const plist = (code, what) => {
    const r = record(records, '.', code);
    if (!r || r.type !== 'blob') {
      problems.push(`.DS_Store has no ${what} (${code}): Finder did not save it`);
      return null;
    }
    try {
      return parseBplist(r.value);
    } catch (error) {
      problems.push(`${code}: ${error.message}`);
      return null;
    }
  };
  const view = plist('icvp', 'icon view settings');
  if (view) {
    // backgroundType 2: a picture (0 is the default white, 1 a colour).
    if (view.backgroundType !== 2) problems.push(`the icon view's background is type ${view.backgroundType}, not a picture (2)`);
    const alias = view.backgroundImageAlias;
    if (!Buffer.isBuffer(alias) || !alias.includes('background.tiff')) problems.push('the icon view has no background alias naming background.tiff');
    if (view.iconSize !== settings.iconSize) problems.push(`the icons are ${view.iconSize} points, the layout says ${settings.iconSize}`);
    if (view.textSize !== settings.textSize) problems.push(`the labels are ${view.textSize} points, the layout says ${settings.textSize}`);
    if (view.arrangeBy !== 'none') problems.push(`the icons are arranged by ${view.arrangeBy}, so Finder would move them`);
  }
  const win = plist('bwsp', 'window settings');
  if (win) {
    const m = /^\{\{(-?\d+), (-?\d+)\}, \{(\d+), (\d+)\}\}$/.exec(String(win.WindowBounds ?? ''));
    const want = [settings.window.width, settings.window.height + settings.window.titleBar];
    if (!m) problems.push(`the window has no bounds (${win.WindowBounds})`);
    else if (Number(m[3]) !== want[0] || Number(m[4]) !== want[1]) problems.push(`the window is ${m[3]}x${m[4]}, the layout says ${want[0]}x${want[1]}`);
    if (win.ShowToolbar !== false || win.ShowStatusBar !== false) problems.push('the window shows its toolbar or status bar');
  }
  for (const [name, want] of [
    [settings.app, settings.appPosition],
    ['Applications', settings.applicationsPosition],
  ]) {
    const got = iconLocation(records, name);
    if (!got) problems.push(`.DS_Store has no position for "${name}"`);
    else if (got.x !== want.x || got.y !== want.y) problems.push(`"${name}" is at ${got.x},${got.y}, the layout says ${want.x},${want.y}`);
  }
  return problems;
}

/** The problems with the volume mounted at `mount` against `settings`. */
export function volumeProblems(mount, settings) {
  const problems = [];
  const at = (rel) => join(mount, rel);
  if (!existsSync(at(`${settings.app}/Contents/Info.plist`))) problems.push(`no app bundle ${settings.app}`);
  let link;
  try {
    link = lstatSync(at('Applications')).isSymbolicLink() ? readlinkSync(at('Applications')) : null;
  } catch {
    link = undefined;
  }
  if (link === undefined) problems.push('Applications is missing, not a link to /Applications');
  else if (link === null) problems.push('Applications is not a symlink, not a link to /Applications');
  else if (link !== '/Applications') problems.push(`Applications is a link to ${link}, not a link to /Applications`);
  for (const rel of ['.background/background.tiff', '.VolumeIcon.icns']) if (!existsSync(at(rel))) problems.push(`no ${rel}`);
  if (!existsSync(at('.DS_Store'))) problems.push('no .DS_Store: Finder saved no layout');
  else problems.push(...layoutProblems(readFileSync(at('.DS_Store')), settings));
  return problems;
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const [mount, settingsFile] = process.argv.slice(2);
  if (!mount || !settingsFile) {
    console.log('::error::usage: dmg-verify.mjs <mount-point> <settings.json>');
    process.exit(2);
  }
  const settings = JSON.parse(readFileSync(settingsFile, 'utf8'));
  const problems = volumeProblems(mount, settings);
  if (problems.length) {
    for (const p of problems) console.log(`dmg layout: ${p}`);
    process.exitCode = 1;
  } else {
    const { window: w, iconSize, appPosition: a, applicationsPosition: f } = settings;
    console.log(`dmg layout: window ${w.width}x${w.height}, ${iconSize}-point icons, picture background.tiff; ${settings.app} at ${a.x},${a.y}; Applications -> /Applications at ${f.x},${f.y}; .VolumeIcon.icns`);
  }
}
