// node --test scripts/brand/dmg.test.mjs
//
// The disk image background (dmg.mjs): its size, that Finder's dark labels read on it
// (checked on the committed PNGs' label rows, pixel by pixel), the arrow between the
// icons, and that it is deterministic.
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { ARROW_LUMINANCE, atLuminance, DMG_LAYOUT, dmgBackground, dmgBackgroundAssets, dmgBackgroundPng, LABEL_CONTRAST, labelBox, labelContrast, LUMINANCE, relativeLuminance } from './dmg.mjs';
import { decodePng, pngInfo } from './images.mjs';
import { ASSETS } from './schema.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');

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

test('every pixel off the arrow sits in the light band where the dark labels read', () => {
  for (const [from, to] of [['#4A4DE8', '#7A5CFF'], ['#0E7490', '#1F2933'], ['#FFF59D', '#000000']]) {
    const px = dmgBackground({ from, to, scale: 1 });
    for (let y = 0; y < DMG_LAYOUT.height; y += 7) {
      for (let x = 0; x < DMG_LAYOUT.width; x += 7) {
        // The arrow's row between the icons is dark on purpose.
        if (Math.abs(y - DMG_LAYOUT.app.y) < 24 && x > DMG_LAYOUT.app.x + 64 && x < DMG_LAYOUT.applications.x - 64) continue;
        const l = lum(px(x, y));
        // 8-bit rounding moves the luminance by a hair.
        assert.ok(l > LUMINANCE.low - 0.004 && l < LUMINANCE.high + 0.004, `${from}→${to} at ${x},${y}: luminance ${l.toFixed(3)}`);
        assert.ok(contrast(l, 0) >= LABEL_CONTRAST, `${from}→${to} at ${x},${y}: ${contrast(l, 0).toFixed(2)}:1 against black`);
      }
    }
  }
});

test('the label rows of every committed background keep dark text at 7:1 or more, read from the PNG pixels', () => {
  for (const brand of ['elitea', 'sample']) {
    for (const [file, scale] of [['dmg-background.png', 1], ['dmg-background@2x.png', 2]]) {
      const image = decodePng(readFileSync(join(ROOT, 'brand', brand, 'assets', 'macos', file)));
      assert.deepEqual([image.width, image.height], [DMG_LAYOUT.width * scale, DMG_LAYOUT.height * scale], `${brand}/${file}`);
      const worst = labelContrast(image, scale);
      assert.ok(worst >= LABEL_CONTRAST, `${brand}/${file}: the label rows reach ${worst.toFixed(2)}:1 against black`);
    }
  }
  // The box sits under each icon, clear of it, and inside the canvas.
  for (const pos of [DMG_LAYOUT.app, DMG_LAYOUT.applications]) {
    const box = labelBox(pos);
    assert.ok(box.top > pos.y + DMG_LAYOUT.iconSize / 2 && box.bottom < DMG_LAYOUT.height && box.left >= 0 && box.right <= DMG_LAYOUT.width);
  }
});

test('labelContrast sees a dark pixel in a label row', () => {
  const dark = dmgBackgroundPng({ from: '#4A4DE8', to: '#7A5CFF', scale: 1 });
  const image = decodePng(dark);
  const box = labelBox(DMG_LAYOUT.app);
  const spoiled = { ...image, pixel: (x, y) => (x === box.left + 10 && y === box.top + 5 ? [60, 60, 60, 255] : image.pixel(x, y)) };
  assert.ok(labelContrast(spoiled, 1) < 4.5);
  assert.ok(labelContrast(image, 1) >= LABEL_CONTRAST);
});

test('the arrow points from the app to Applications, and clears both icons', () => {
  const px = dmgBackground({ from: '#4A4DE8', to: '#7A5CFF', scale: 2 });
  const { app, applications, iconSize } = DMG_LAYOUT;
  const at = (x, y) => lum(px(x * 2, y * 2));
  // Midway between the icons, on the arrow: dark, and at least 3:1 against the canvas (WCAG 1.4.11).
  const mid = at((app.x + applications.x) / 2, app.y);
  assert.ok(Math.abs(mid - ARROW_LUMINANCE) < 0.01, `the arrow's shaft is ${mid.toFixed(3)}`);
  assert.ok(contrast(mid, at((app.x + applications.x) / 2, app.y + 60)) >= 3);
  // Under each icon (inside its 128-point square) nothing is drawn but the canvas.
  for (const c of [app, applications]) {
    for (const dx of [-iconSize / 2 + 2, 0, iconSize / 2 - 2]) assert.ok(at(c.x + dx, c.y) > LUMINANCE.low - 0.004, `${c.x + dx},${c.y}`);
  }
  // The arrow lies between the icons, clear of both.
  const row = Array.from({ length: applications.x - app.x }, (_, i) => at(app.x + i, app.y) < 0.4);
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
