// node --test scripts/dmg.test.mjs
//
// The styled disk image's brand inputs (dmg-settings.mjs) and the check that proves a
// mounted image is laid out (dmg-verify.mjs), against a .DS_Store and property lists built here.
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
const VIEW = { backgroundType: 2, backgroundImageAlias: Buffer.from('\x00\x0fbackground.tiff', 'latin1'), iconSize: 128, textSize: 13, arrangeBy: 'none', labelOnBottom: true };
const WINDOW = { WindowBounds: '{{200, 569}, {660, 428}}', ShowToolbar: false, ShowStatusBar: false, ShowSidebar: false };
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
  assert.match(problems({ win: { ...WINDOW, WindowBounds: '{{200, 120}, {770, 460}}', ShowToolbar: true } }), /window is 770x460, the layout says 660x428[\s\S]*toolbar or status bar/);
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
