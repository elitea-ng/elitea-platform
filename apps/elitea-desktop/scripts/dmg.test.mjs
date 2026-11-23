// node --test scripts/dmg.test.mjs
//
// The styled disk image's brand inputs (dmg-settings.mjs) and the check that proves a
// mounted image is laid out (dmg-verify.mjs), against a .DS_Store and property lists built here;
// and the writer (ds-store.mjs) that hides the window's bars.
import assert from 'node:assert/strict';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, unlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { fileURLToPath } from 'node:url';

import { DMG_LAYOUT } from '../../../scripts/brand/dmg.mjs';
import { pngInfo } from '../../../scripts/brand/images.mjs';
import { loadBrandFrom } from '../../../scripts/brand/load.mjs';
import { dmgSettings, writeDmgInputs } from './dmg-settings.mjs';
import { dsStoreRecords, iconLocation, layoutProblems, parseBplist, volumeProblems } from './dmg-verify.mjs';
import { blobRecord, BplistRaw, compareRecords, encodeBplist, freeLists, HIDDEN_CHROME, hideChrome, writeDsStore } from './ds-store.mjs';

const BRANDS = fileURLToPath(new URL('../../../brand/', import.meta.url));
const sample = loadBrandFrom(join(BRANDS, 'sample'));
const elitea = loadBrandFrom(join(BRANDS, 'elitea'));

function tmp() {
  return mkdtempSync(join(tmpdir(), 'dmg-test-'));
}

test('the volume is <shortName> <version>, the layout is the shared one, the background the brand’s', () => {
  const s = dmgSettings(sample, '1.2.3-nightly.20261122.7');
  assert.equal(s.volumeName, 'Sample 1.2.3-nightly.20261122.7');
  assert.equal(s.app, 'Sample.app');
  assert.equal(s.background, 'brand');
  assert.deepEqual(s.window, { width: DMG_LAYOUT.width, height: DMG_LAYOUT.height, titleBar: DMG_LAYOUT.titleBar });
  assert.deepEqual([s.appPosition, s.applicationsPosition], [{ ...DMG_LAYOUT.app }, { ...DMG_LAYOUT.applications }]);
  assert.equal(dmgSettings(elitea, '7.8.0').volumeName, `${elitea.manifest.shortName} 7.8.0`);
  assert.throws(() => dmgSettings(sample, '7.8'), /not X\.Y\.Z/);
  // Nothing of the default brand reaches another brand's image.
  assert.doesNotMatch(JSON.stringify(s), new RegExp(elitea.manifest.shortName, 'i'));
});

test('the inputs: the brand’s own backgrounds and icon, copied', () => {
  const out = tmp();
  try {
    writeDmgInputs(sample, '1.0.0', out);
    assert.ok(readFileSync(join(out, 'background.png')).equals(readFileSync(sample.path('assets/macos/dmg-background.png'))));
    assert.ok(readFileSync(join(out, 'background@2x.png')).equals(readFileSync(sample.path('assets/macos/dmg-background@2x.png'))));
    assert.ok(readFileSync(join(out, 'volume.icns')).equals(readFileSync(sample.path('assets/macos/icon.icns'))));
    assert.equal(JSON.parse(readFileSync(join(out, 'settings.json'), 'utf8')).volumeName, 'Sample 1.0.0');
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
});

test('a brand that ships no background gets one drawn from its hue', () => {
  const dir = tmp();
  const out = tmp();
  try {
    const copy = join(dir, 'sample');
    cpSync(sample.dir, copy, { recursive: true });
    unlinkSync(join(copy, 'assets/macos/dmg-background.png'));
    unlinkSync(join(copy, 'assets/macos/dmg-background@2x.png'));
    const s = writeDmgInputs(loadBrandFrom(copy), '1.0.0', out);
    assert.equal(s.background, 'hue');
    const one = pngInfo(readFileSync(join(out, 'background.png')));
    const two = pngInfo(readFileSync(join(out, 'background@2x.png')));
    assert.deepEqual([one.width, one.height, two.width, two.height], [660, 400, 1320, 800]);
    // One file alone is refused, not swapped for the hue in silence.
    cpSync(sample.path('assets/macos/dmg-background.png'), join(copy, 'assets/macos/dmg-background.png'));
    assert.throws(() => dmgSettings(loadBrandFrom(copy), '1.0.0'), /needs both, or neither/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
    rmSync(out, { recursive: true, force: true });
  }
});

/** A binary property list of `value` (dicts, strings, integers, booleans, Buffers). */
function bplist(value) {
  const objects = [];
  const add = (v) => {
    const at = objects.length;
    objects.push(null);
    let bytes;
    const len = (kind, n) => (n < 15 ? Buffer.from([(kind << 4) | n]) : Buffer.from([(kind << 4) | 0x0f, 0x11, n >> 8, n & 0xff]));
    if (typeof v === 'boolean') bytes = Buffer.from([v ? 0x09 : 0x08]);
    else if (typeof v === 'number') bytes = Buffer.from([0x11, v >> 8, v & 0xff]);
    else if (typeof v === 'string') bytes = Buffer.concat([len(5, v.length), Buffer.from(v, 'latin1')]);
    else if (Buffer.isBuffer(v)) bytes = Buffer.concat([len(4, v.length), v]);
    else {
      const keys = Object.keys(v);
      const refs = [...keys.map((k) => add(k)), ...keys.map((k) => add(v[k]))];
      bytes = Buffer.concat([len(0xd, keys.length), Buffer.from(refs)]);
    }
    objects[at] = bytes;
    return at;
  };
  add(value);
  const head = Buffer.from('bplist00', 'latin1');
  const offsets = [];
  let pos = head.length;
  for (const o of objects) {
    offsets.push(pos);
    pos += o.length;
  }
  const table = Buffer.alloc(offsets.length * 2);
  offsets.forEach((o, i) => table.writeUInt16BE(o, i * 2));
  const trailer = Buffer.alloc(32);
  trailer.writeUInt8(2, 6);
  trailer.writeUInt8(1, 7);
  trailer.writeBigUInt64BE(BigInt(objects.length), 8);
  trailer.writeBigUInt64BE(0n, 16);
  trailer.writeBigUInt64BE(BigInt(pos), 24);
  return Buffer.concat([head, ...objects, table, trailer]);
}

/** One .DS_Store record: `<name length><name UTF-16BE><code><type><value>`. */
function rec(name, code, type, value) {
  const n = Buffer.alloc(4);
  n.writeUInt32BE(name.length);
  let v = value;
  if (type === 'blob') {
    v = Buffer.alloc(4 + value.length);
    v.writeUInt32BE(value.length);
    value.copy(v, 4);
  } else if (type === 'long') {
    v = Buffer.alloc(4);
    v.writeUInt32BE(value);
  }
  return Buffer.concat([n, Buffer.from(name, 'utf16le').swap16(), Buffer.from(code + type, 'latin1'), v]);
}

/**
 * A .DS_Store as Finder lays one out: the buddy allocator, the DSDB block and a B-tree
 * of an inner node over two leaves, the records split between them, and `stale` bytes
 * in a free block (a record an earlier write left), which the reader must not see.
 */
function dsStoreFile(records, { stale = Buffer.alloc(0) } = {}) {
  const u32 = (...vs) => {
    const b = Buffer.alloc(vs.length * 4);
    vs.forEach((v, i) => b.writeUInt32BE(v, i * 4));
    return b;
  };
  const page = 0x1000;
  // Blocks (offsets from byte 4): 0 the allocator root, 1 DSDB, 2 the inner node, 3 and 4 the leaves.
  const offsets = [0x800, 0x1000, 0x2000, 0x3000, 0x4000];
  const sizes = [11, 5, 12, 12, 12];
  const half = Math.ceil(records.length / 2);
  const [left, mid, right] = [records.slice(0, half - 1), records[half - 1], records.slice(half)];
  const blocks = [
    Buffer.concat([u32(offsets.length, 0), u32(...offsets.map((o, i) => o | sizes[i])), Buffer.alloc((256 - offsets.length) * 4), u32(1), Buffer.from([4]), Buffer.from('DSDB', 'latin1'), u32(1), Buffer.alloc(32 * 4)]),
    u32(2, 1, records.length, 3, page),
    Buffer.concat([u32(4, 1, 3), mid]),
    Buffer.concat([u32(0, left.length), ...left]),
    Buffer.concat([u32(0, right.length), ...right]),
  ];
  const file = Buffer.alloc(4 + 0x5000 + page);
  file.writeUInt32BE(1, 0);
  file.write('Bud1', 4, 'latin1');
  u32(0x800, 0x800, 0x800).copy(file, 8);
  blocks.forEach((b, i) => b.copy(file, 4 + offsets[i]));
  stale.copy(file, 4 + 0x5000);
  return file;
}

function iloc(x, y) {
  const b = Buffer.alloc(16, 0xff);
  b.writeUInt32BE(x, 0);
  b.writeUInt32BE(y, 4);
  return b;
}
const SETTINGS = dmgSettings(sample, '1.0.0');
const VIEW = { backgroundType: 2, backgroundImageAlias: Buffer.from('\x00\x0fbackground.tiff', 'latin1'), iconSize: 128, textSize: 13, arrangeBy: 'none', labelOnBottom: true, backgroundColorRed: 1, backgroundColorGreen: 1, backgroundColorBlue: 1 };
const WINDOW = { WindowBounds: '{{200, 569}, {660, 428}}', ...HIDDEN_CHROME };
// What Finder on macOS 26 and later saves: the tab bar on, no path-bar key at all.
const FINDER_WINDOW = { ShowStatusBar: false, ShowToolbar: false, ShowTabView: true, ContainerShowSidebar: false, WindowBounds: '{{200, 569}, {660, 428}}', ShowSidebar: false };
function dsStore({ view = VIEW, win = WINDOW, app = iloc(170, 185), apps = iloc(490, 185), stale } = {}) {
  const records = [
    win && rec('.', 'bwsp', 'blob', bplist(win)),
    view && rec('.', 'icvp', 'blob', bplist(view)),
    rec('.', 'vSrn', 'long', 1),
    apps && rec('Applications', 'Iloc', 'blob', apps),
    app && rec('Sample.app', 'Iloc', 'blob', app),
  ].filter(Boolean);
  return dsStoreFile(records, { stale });
}

test('the reader walks the live tree and reads the property lists', () => {
  const records = dsStoreRecords(dsStore());
  assert.deepEqual(records.map((r) => `${r.name} ${r.code}`), ['. bwsp', '. icvp', '. vSrn', 'Applications Iloc', 'Sample.app Iloc']);
  assert.deepEqual(iconLocation(records, 'Applications'), { x: 490, y: 185 });
  assert.equal(iconLocation(records, 'Nothing'), null);
  const view = parseBplist(records.find((r) => r.code === 'icvp').value);
  assert.equal(view.iconSize, 128);
  assert.ok(view.backgroundImageAlias.includes('background.tiff'));
  assert.throws(() => dsStoreRecords(Buffer.alloc(64)), /not a \.DS_Store/);
});

test('a .DS_Store with the view, the window and both positions passes', () => {
  assert.deepEqual(layoutProblems(dsStore(), SETTINGS), []);
});

test('a layout Finder did not apply fails, saying what is wrong', () => {
  const problems = (opts) => layoutProblems(dsStore(opts), SETTINGS).join('\n');
  assert.match(problems({ view: null }), /no icon view settings \(icvp\)/);
  assert.match(problems({ view: { ...VIEW, backgroundType: 0, backgroundImageAlias: Buffer.alloc(4) } }), /background is type 0, not a picture[\s\S]*no background alias naming background\.tiff/);
  assert.match(problems({ view: { ...VIEW, iconSize: 64, textSize: 12, arrangeBy: 'name' } }), /icons are 64 points[\s\S]*labels are 12 points[\s\S]*arranged by name/);
  assert.match(problems({ win: null }), /no window settings \(bwsp\)/);
  assert.match(problems({ win: { ...WINDOW, WindowBounds: '{{200, 120}, {770, 460}}', ShowToolbar: true } }), /window is 770x460, the layout says 660x428[\s\S]*does not hide ShowToolbar:/);
  // The window Finder saves shows its tab bar and says nothing of the path bar: both fail.
  assert.match(problems({ win: FINDER_WINDOW }), /does not hide ShowTabView, ShowPathbar: only the artwork may show/);
  assert.match(problems({ view: { ...VIEW, backgroundColorRed: 0 } }), /background colour is 0,1,1, not white/);
  assert.match(problems({ app: null }), /no position for "Sample\.app"/);
  assert.match(problems({ apps: iloc(400, 100) }), /"Applications" is at 400,100, the layout says 490,185/);
  assert.match(layoutProblems(Buffer.from('not a store'), SETTINGS).join('\n'), /not a \.DS_Store/);
});

test('a stale record in a free block is not read as the layout', () => {
  // The live tree has no Applications position; an earlier write left a right one behind.
  const stale = rec('Applications', 'Iloc', 'blob', iloc(490, 185));
  assert.match(layoutProblems(dsStore({ apps: null, stale }), SETTINGS).join('\n'), /no position for "Applications"/);
});

test('the mounted volume: app, Applications link, background, volume icon and layout', () => {
  const mnt = tmp();
  try {
    mkdirSync(join(mnt, 'Sample.app/Contents'), { recursive: true });
    writeFileSync(join(mnt, 'Sample.app/Contents/Info.plist'), '<plist/>');
    mkdirSync(join(mnt, '.background'));
    writeFileSync(join(mnt, '.background/background.tiff'), 'II*');
    writeFileSync(join(mnt, '.VolumeIcon.icns'), 'icns');
    writeFileSync(join(mnt, '.DS_Store'), dsStore());
    assert.match(volumeProblems(mnt, SETTINGS).join('\n'), /Applications is missing/);
    symlinkSync('/Users', join(mnt, 'Applications'));
    assert.match(volumeProblems(mnt, SETTINGS).join('\n'), /Applications is a link to \/Users/);
    unlinkSync(join(mnt, 'Applications'));
    symlinkSync('/Applications', join(mnt, 'Applications'));
    assert.deepEqual(volumeProblems(mnt, SETTINGS), []);
    unlinkSync(join(mnt, '.VolumeIcon.icns'));
    unlinkSync(join(mnt, '.DS_Store'));
    assert.deepEqual(volumeProblems(mnt, SETTINGS), ['no .VolumeIcon.icns', 'no .DS_Store: Finder saved no layout']);
  } finally {
    rmSync(mnt, { recursive: true, force: true });
  }
});

test('hide-chrome turns every bar off, keeps the bounds and every other record byte for byte', () => {
  const before = dsStore({ win: FINDER_WINDOW });
  assert.notDeepEqual(layoutProblems(before, SETTINGS), []);
  const after = hideChrome(before);
  assert.deepEqual(layoutProblems(after, SETTINGS), []);
  const win = parseBplist(dsStoreRecords(after).find((r) => r.code === 'bwsp').value);
  assert.deepEqual(win, { ...FINDER_WINDOW, ...HIDDEN_CHROME });
  const key = (r) => `${r.name} ${r.code}`;
  const old = dsStoreRecords(before).filter((r) => r.code !== 'bwsp');
  const now = new Map(dsStoreRecords(after).map((r) => [key(r), r.raw]));
  for (const r of old) assert.ok(now.get(key(r))?.equals(r.raw), key(r));
  // Idempotent, and refuses a file Finder never saved a window into.
  assert.ok(hideChrome(after).equals(after));
  assert.throws(() => hideChrome(dsStore({ win: null })), /no window settings \(bwsp\)/);
});

test('the property-list writer round-trips what Finder stores', () => {
  const value = { a: true, b: false, n: 7, big: 70000, r: 0.5, s: 'ascii', u: 'Ünïcode', d: Buffer.from([1, 2, 3]), list: [1, 'x', { nested: true }], long: 'x'.repeat(40) };
  assert.deepEqual(parseBplist(encodeBplist(value)), { ...value, d: Buffer.from([1, 2, 3]) });
  assert.throws(() => encodeBplist({ n: -1 }), /out of range/);
});

test('the .DS_Store writer keeps Finder order and splits records over pages', () => {
  // Enough positions to need several leaves under the inner node.
  const many = Array.from({ length: 300 }, (_, i) => blobRecord(`Item ${String(i).padStart(3, '0')}.app`, 'Iloc', iloc(i, i)));
  const file = writeDsStore([...many].reverse());
  const back = dsStoreRecords(file);
  assert.equal(back.length, many.length);
  assert.deepEqual(back.map((r) => r.name), [...many].sort(compareRecords).map((r) => r.name));
  assert.deepEqual(iconLocation(back, 'Item 299.app'), { x: 299, y: 299 });
  // Case-insensitive by name, then by code.
  assert.ok(compareRecords({ name: 'applications', code: 'Iloc' }, { name: 'B', code: 'Iloc' }) < 0);
  assert.ok(compareRecords({ name: '.', code: 'bwsp' }, { name: '.', code: 'icvp' }) < 0);
});

test('hide-chrome keeps every other window key with its type: reals, 64-bit and negative integers, dates, null', () => {
  // A window record as a newer Finder might write it, with values the plain encoder cannot express.
  const real = Buffer.from([0x23, 0x3f, 0xf0, 0, 0, 0, 0, 0, 0]); // 1.0 as a real
  const negative = Buffer.from([0x13, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xfe]); // -2
  const date = Buffer.from([0x33, 0x41, 0xc6, 0, 0, 0, 0, 0, 0]);
  const win = { ...FINDER_WINDOW, SidebarWidth: new BplistRaw(real), Offset: new BplistRaw(negative), Saved: new BplistRaw(date), Nothing: new BplistRaw(Buffer.from([0x00])) };
  const before = dsStoreFile([rec('.', 'bwsp', 'blob', encodeBplist(win))]);
  const after = parseBplist(dsStoreRecords(hideChrome(before))[0].value, { raw: true });
  for (const k of ['SidebarWidth', 'Offset', 'Saved', 'Nothing']) assert.ok(after[k].bytes.equals(win[k].bytes), k);
  assert.equal(after.WindowBounds, FINDER_WINDOW.WindowBounds);
  const plain = parseBplist(encodeBplist(win));
  assert.deepEqual([plain.SidebarWidth, plain.Offset, plain.Nothing], [1, -2, null]);
  assert.ok(plain.Saved instanceof Date);
});

test('the allocator keeps free lists as Finder does: every unused buddy block, none overlapping a used one', () => {
  const used = [[0, 5], [0x20, 5], [0x800, 11], [0x1000, 12]];
  const lists = freeLists(used);
  // As Finder writes for the same blocks: 64 B to 1 KiB below the allocator, then 8 KiB to 1 GiB.
  assert.deepEqual(lists.map((l, i) => (l.length ? `${i}:${l.map((o) => o.toString(16)).join('/')}` : '')).filter(Boolean).slice(0, 6), ['6:40', '7:80', '8:100', '9:200', '10:400', '13:2000']);
  assert.deepEqual(lists[30], [0x40000000]);
  // Free and used blocks tile the 2 GiB space exactly.
  const total = lists.reduce((sum, l, i) => sum + l.length * 2 ** i, 0) + used.reduce((sum, [, l]) => sum + 2 ** l, 0);
  assert.equal(total, 2 ** 31);
  // And the writer's own file carries them.
  const file = writeDsStore([blobRecord('.', 'bwsp', encodeBplist(WINDOW))]);
  assert.equal(file.readUInt32BE(4 + 0x800 + 8 + 256 * 4 + 4 + 1 + 4 + 4 + 6 * 4), 1);
});
