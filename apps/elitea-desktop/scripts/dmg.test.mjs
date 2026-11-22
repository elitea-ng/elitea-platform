// node --test scripts/dmg.test.mjs
//
// The styled disk image's brand inputs (dmg-settings.mjs) and the check that proves a
// mounted image is laid out (dmg-verify.mjs), against a .DS_Store built here.
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
import { blobRecord, iconLocation, layoutProblems, volumeProblems } from './dmg-verify.mjs';

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
  assert.deepEqual(s.window, { width: DMG_LAYOUT.width, height: DMG_LAYOUT.height });
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
  } finally {
    rmSync(dir, { recursive: true, force: true });
    rmSync(out, { recursive: true, force: true });
  }
});

/** A .DS_Store's records as Finder writes them (the B-tree around them is not read). */
function record(name, code, data) {
  const head = Buffer.alloc(4);
  head.writeUInt32BE(name.length);
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  return Buffer.concat([head, Buffer.from(name, 'utf16le').swap16(), Buffer.from(`${code}blob`, 'latin1'), len, data]);
}
function iloc(x, y) {
  const b = Buffer.alloc(16, 0xff);
  b.writeUInt32BE(x, 0);
  b.writeUInt32BE(y, 4);
  return b;
}
const SETTINGS = dmgSettings(sample, '1.0.0');
const ICVP = Buffer.from('bplist00...backgroundImageAlias...\x00\x0fbackground.tiff...backgroundType', 'latin1');
function dsStore({ app = iloc(170, 185), apps = iloc(490, 185), icvp = ICVP, bwsp = true } = {}) {
  return Buffer.concat([
    Buffer.from('\x00\x00\x00\x01Bud1', 'latin1'),
    Buffer.alloc(64),
    bwsp ? record('.', 'bwsp', Buffer.from('bplist00 WindowBounds', 'latin1')) : Buffer.alloc(0),
    icvp ? record('.', 'icvp', icvp) : Buffer.alloc(0),
    app ? record('Sample.app', 'Iloc', app) : Buffer.alloc(0),
    apps ? record('Applications', 'Iloc', apps) : Buffer.alloc(0),
  ]);
}

test('a .DS_Store with the view, the window and both positions passes', () => {
  const ds = dsStore();
  assert.deepEqual(layoutProblems(ds, SETTINGS), []);
  assert.deepEqual(iconLocation(ds, 'Applications'), { x: 490, y: 185 });
  assert.ok(blobRecord(ds, '.', 'icvp').includes('background.tiff'));
  assert.equal(blobRecord(ds, 'Nothing', 'Iloc'), null);
});

test('a layout Finder did not apply fails, saying what is missing', () => {
  assert.match(layoutProblems(dsStore({ icvp: null }), SETTINGS).join('\n'), /no icon view settings \(icvp\)/);
  assert.match(layoutProblems(dsStore({ icvp: Buffer.from('bplist00 iconSize', 'latin1') }), SETTINGS).join('\n'), /no background picture/);
  assert.match(layoutProblems(dsStore({ bwsp: false }), SETTINGS).join('\n'), /no window settings \(bwsp\)/);
  assert.match(layoutProblems(dsStore({ app: null }), SETTINGS).join('\n'), /no position for "Sample\.app"/);
  assert.match(layoutProblems(dsStore({ apps: iloc(400, 100) }), SETTINGS).join('\n'), /"Applications" is at 400,100, the layout says 490,185/);
  // A record cut short by the end of the file is not read past it.
  const ds = dsStore();
  assert.equal(iconLocation(ds.subarray(0, ds.length - 12), 'Applications'), null);
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
