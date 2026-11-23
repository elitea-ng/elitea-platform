// node --test scripts/brand/dmg.test.mjs
//
// The disk image background (dmg.mjs): its size, that its labels read in both Finder
// modes, the arrow between the icons, and that it is deterministic.
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { atLuminance, DMG_LAYOUT, dmgBackground, dmgBackgroundAssets, dmgBackgroundPng, LUMINANCE, relativeLuminance } from './dmg.mjs';
import { pngInfo } from './images.mjs';
import { ASSETS } from './schema.mjs';

const lin = (c) => {
  const s = c / 255;
  return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
};
const lum = ([r, g, b]) => 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
const contrast = (a, b) => (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);

test('the backgrounds are 660x400 and 1320x800, opaque, as the asset table says', () => {
  const assets = dmgBackgroundAssets({ from: '#4A4DE8', to: '#7A5CFF' });
  for (const [path, bytes] of assets) {
    const spec = ASSETS.find((a) => a.path === path);
    assert.ok(spec, path);
    const info = pngInfo(bytes);
    assert.deepEqual([info.width, info.height, info.alpha], [spec.width, spec.height, false], path);
  }
  assert.deepEqual([DMG_LAYOUT.width * 2, DMG_LAYOUT.height * 2], [1320, 800]);
});

test('every pixel off the arrow sits in the band where black and white labels both read', () => {
  for (const [from, to] of [['#4A4DE8', '#7A5CFF'], ['#0E7490', '#1F2933'], ['#FFF59D', '#000000']]) {
    const px = dmgBackground({ from, to, scale: 1 });
    for (let y = 0; y < DMG_LAYOUT.height; y += 7) {
      for (let x = 0; x < DMG_LAYOUT.width; x += 7) {
        // The arrow's row between the icons is white on purpose.
        if (Math.abs(y - DMG_LAYOUT.app.y) < 24 && x > DMG_LAYOUT.app.x + 64 && x < DMG_LAYOUT.applications.x - 64) continue;
        const l = lum(px(x, y));
        // 8-bit rounding moves the luminance by a hair.
        assert.ok(l > LUMINANCE.low - 0.004 && l < LUMINANCE.high + 0.004, `${from}→${to} at ${x},${y}: luminance ${l.toFixed(3)}`);
        assert.ok(contrast(l, 0) >= 4.2 && contrast(l, 1) >= 4.1, `${from}→${to} at ${x},${y}: ${contrast(l, 0).toFixed(2)}:1 black, ${contrast(l, 1).toFixed(2)}:1 white`);
      }
    }
  }
});

test('the arrow points from the app to Applications, and clears both icons', () => {
  const px = dmgBackground({ from: '#4A4DE8', to: '#7A5CFF', scale: 2 });
  const { app, applications, iconSize } = DMG_LAYOUT;
  const at = (x, y) => lum(px(x * 2, y * 2));
  // Midway between the icons, on the arrow: near white, and at least 3:1 against the canvas.
  const mid = at((app.x + applications.x) / 2, app.y);
  assert.ok(mid > 0.7, `the arrow's shaft is ${mid.toFixed(2)}`);
  assert.ok(contrast(mid, at((app.x + applications.x) / 2, app.y + 60)) >= 3);
  // Under each icon (inside its 128-point square) nothing is drawn but the canvas.
  for (const c of [app, applications]) {
    for (const dx of [-iconSize / 2 + 2, 0, iconSize / 2 - 2]) assert.ok(at(c.x + dx, c.y) < LUMINANCE.high + 0.004, `${c.x + dx},${c.y}`);
  }
  // The head sits on the Applications side: the shaft's right end is further right than its left end is left of centre.
  const row = Array.from({ length: applications.x - app.x }, (_, i) => at(app.x + i, app.y) > 0.5);
  const first = row.indexOf(true);
  const last = row.lastIndexOf(true);
  assert.ok(first > iconSize / 2 && last < applications.x - app.x - iconSize / 2, `arrow from ${app.x + first} to ${app.x + last}`);
});

test('a colour moves to the target luminance in its own hue family', () => {
  for (const hex of ['#4A4DE8', '#FFF59D', '#000000', '#FFFFFF']) {
    const c = atLuminance(hex, 0.185);
    assert.ok(Math.abs(0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2] - 0.185) < 1e-9, hex);
  }
  assert.ok(Math.abs(relativeLuminance('#FFFFFF') - 1) < 1e-9);
});

test('the same colours give the same pixels', () => {
  const a = dmgBackgroundPng({ from: '#0E7490', to: '#1F2933' });
  const b = dmgBackgroundPng({ from: '#0E7490', to: '#1F2933' });
  assert.ok(a.equals(b));
  assert.ok(!a.equals(dmgBackgroundPng({ from: '#4A4DE8', to: '#1F2933' })));
});
