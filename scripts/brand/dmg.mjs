// The macOS disk image's window (ADR-0052 decision 4): the fixed layout every
// brand's .dmg shares, and the background drawn for it. The app icon sits on the
// left, an alias of Applications on the right, an arrow between them says "drag".
//
// A brand ships its own background as assets/macos/dmg-background.png (660x400)
// and dmg-background@2x.png (1320x800), drawn around DMG_LAYOUT. A brand without
// them gets one drawn here from its hue at package time
// (apps/elitea-desktop/scripts/dmg-settings.mjs), so no brand ever shows another's.
//
// WHY LIGHT. Finder draws the icon labels of a window with a background picture in
// dark text, in light AND dark mode (seen on macOS 26 and 27: dark mode, a black
// background, and the labels still dark). So the canvas is a light, soft tint of the
// brand: every pixel sits at a relative luminance between LUMINANCE.low and
// LUMINANCE.high, where dark text keeps at least LABEL_CONTRAST (the label row is
// checked from the PNG pixels in dmg.test.mjs). A mid-tone could not serve both text
// colours: no colour gives black and white text 4.5:1 each but a grey near
// luminance 0.18, which is what made the first labels hard to read. The arrow is the
// brand hue made dark, at least 3:1 against the canvas (WCAG 1.4.11).
//
// Node's standard library only; deterministic (the same colours give the same pixels).

import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { encodePng, rgb } from './raster.mjs';

/**
 * The window, in points (a @2x background doubles every number). The positions are
 * the icons' centres, as Finder's `position of item` takes them.
 */
export const DMG_LAYOUT = Object.freeze({
  width: 660,
  height: 400,
  iconSize: 128,
  textSize: 13,
  // The window's frame is taller than the picture by its title bar.
  titleBar: 28,
  app: Object.freeze({ x: 170, y: 185 }),
  applications: Object.freeze({ x: 490, y: 185 }),
});

/** The canvas's luminance band: light, so Finder's dark labels keep LABEL_CONTRAST or more (15:1 at `low`). */
export const LUMINANCE = Object.freeze({ low: 0.72, high: 0.9 });

/** Black text on the label row keeps at least this contrast (WCAG 1.4.6 AAA is 7:1). */
export const LABEL_CONTRAST = 7;

/** The arrow's luminance: the `from` colour made dark. */
export const ARROW_LUMINANCE = 0.1;

const toLinear = (c) => {
  const s = c / 255;
  return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
};
const lum = ([r, g, b]) => 0.2126 * r + 0.7152 * g + 0.0722 * b;

// Linear → 8-bit sRGB, through a table (a 1320x800 canvas calls it 3 million times).
const ENCODE = (() => {
  const n = 4096;
  const t = new Uint8Array(n + 1);
  for (let i = 0; i <= n; i++) {
    const l = i / n;
    const s = l <= 0.0031308 ? l * 12.92 : 1.055 * l ** (1 / 2.4) - 0.055;
    t[i] = Math.round(Math.min(1, Math.max(0, s)) * 255);
  }
  return (l) => t[Math.round(Math.min(1, Math.max(0, l)) * n)];
})();

/** WCAG relative luminance of `#rrggbb`. */
export const relativeLuminance = (hex) => lum(rgb(hex).map(toLinear));

/**
 * `hex` mixed toward white or black (in linear light) until its relative luminance
 * is `target`: the same hue family, at the band's brightness. Returns linear RGB.
 */
export function atLuminance(hex, target) {
  const c = rgb(hex).map(toLinear);
  const toward = lum(c) < target ? [1, 1, 1] : [0, 0, 0];
  // Luminance is linear in linear RGB, so the mix fraction is exact.
  const l0 = lum(c);
  const l1 = lum(toward);
  const t = l1 === l0 ? 0 : (target - l0) / (l1 - l0);
  return c.map((v, i) => v + (toward[i] - v) * t);
}

// Distance from (px, py) to the segment (ax, ay)–(bx, by).
function segment(px, py, ax, ay, bx, by) {
  const dx = bx - ax;
  const dy = by - ay;
  const h = Math.min(1, Math.max(0, ((px - ax) * dx + (py - ay) * dy) / (dx * dx + dy * dy)));
  return Math.hypot(px - ax - dx * h, py - ay - dy * h);
}

/**
 * The background as a pixel function at `scale` (1 or 2): a diagonal wash from
 * `from` (top left) to `to` (bottom right), both made light, a lighter pool under each
 * icon, and an arrow from the app to Applications in `from` made dark. `from` and
 * `to` are `#rrggbb`.
 */
export function dmgBackground({ from, to, scale = 1 }) {
  const { width, height, app, applications, iconSize } = DMG_LAYOUT;
  const lo = [atLuminance(from, LUMINANCE.low), atLuminance(to, LUMINANCE.low)];
  const hi = [atLuminance(from, LUMINANCE.high), atLuminance(to, LUMINANCE.high)];
  const ink = atLuminance(from, ARROW_LUMINANCE);
  // The arrow, in points: a shaft between the icons and a chevron head.
  const gap = iconSize / 2 + 28;
  const x0 = app.x + gap;
  const x1 = applications.x - gap;
  const y = (app.y + applications.y) / 2;
  const head = 16;
  const strokes = [
    [x0, y, x1, y],
    [x1 - head, y - head, x1, y],
    [x1 - head, y + head, x1, y],
  ];
  const half = 3.25;
  const pool = iconSize * 0.95;
  return (px, py) => {
    // Pixel centre, in points.
    const x = (px + 0.5) / scale;
    const yy = (py + 0.5) / scale;
    const t = Math.min(1, Math.max(0, (x / width) * 0.6 + (yy / height) * 0.4));
    // The pool: 1 at an icon's centre, easing to 0 at `pool` points away.
    const near = Math.min(Math.hypot(x - app.x, yy - app.y), Math.hypot(x - applications.x, yy - applications.y));
    const g = near >= pool ? 0 : (1 - near / pool) ** 2;
    const colour = [0, 1, 2].map((i) => {
      const low = lo[0][i] + (lo[1][i] - lo[0][i]) * t;
      const high = hi[0][i] + (hi[1][i] - hi[0][i]) * t;
      return low + (high - low) * g;
    });
    // The arrow, anti-aliased over one device pixel.
    const d = Math.min(...strokes.map((s) => segment(x, yy, ...s)));
    const cover = Math.min(1, Math.max(0, (half - d) * scale + 0.5));
    return [...colour.map((c, i) => ENCODE(c + (ink[i] - c) * cover)), 255];
  };
}

/** The background PNG (opaque) at `scale`: 660x400 at 1, 1320x800 at 2. */
export function dmgBackgroundPng({ from, to, scale = 1 }) {
  return encodePng(DMG_LAYOUT.width * scale, DMG_LAYOUT.height * scale, dmgBackground({ from, to, scale }), { alpha: false });
}

/**
 * Where Finder writes an icon's label, in points: a box under the icon centred on its
 * position, wide enough for a 20-character name at the layout's text size.
 */
export function labelBox({ x, y }) {
  const top = y + DMG_LAYOUT.iconSize / 2 + 4;
  return { left: x - 80, right: x + 80, top, bottom: top + 24 };
}

const WCAG = (l1, l2) => (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);

/**
 * The lowest contrast of black text against any pixel of the two label boxes of a
 * decoded background (images.mjs decodePng) drawn at `scale` (1 for 660x400, 2 for
 * 1320x800).
 */
export function labelContrast(image, scale) {
  let worst = Infinity;
  for (const pos of [DMG_LAYOUT.app, DMG_LAYOUT.applications]) {
    const box = labelBox(pos);
    for (let y = box.top * scale; y < box.bottom * scale; y++) {
      for (let x = box.left * scale; x < box.right * scale; x++) {
        const [r, g, b] = image.pixel(x, y);
        worst = Math.min(worst, WCAG(lum([r, g, b].map(toLinear)), 0));
      }
    }
  }
  return worst;
}

/** The two files a brand ships, keyed by their path under assets/. */
export function dmgBackgroundAssets({ from, to }) {
  return new Map([
    ['macos/dmg-background.png', dmgBackgroundPng({ from, to, scale: 1 })],
    ['macos/dmg-background@2x.png', dmgBackgroundPng({ from, to, scale: 2 })],
  ]);
}

// node scripts/brand/dmg.mjs <brand-folder> --from #rrggbb --to #rrggbb
//   writes <brand-folder>/assets/macos/dmg-background.png and @2x.
//   brand/elitea: --from palette.indigo --to palette.violet of
//   libs/ts/client-core/src/brand/tokens.json (elitea-sync.test.mjs holds them equal).
//   brand/sample: placeholder-assets.mjs writes them (its two placeholder colours).
function main(argv) {
  const opt = (name) => {
    const at = argv.indexOf(name);
    return at === -1 ? undefined : argv[at + 1];
  };
  const dir = argv.find((a, i) => !a.startsWith('--') && !['--from', '--to'].includes(argv[i - 1]));
  const from = opt('--from');
  const to = opt('--to');
  if (!dir || ![from, to].every((c) => /^#[0-9a-f]{6}$/i.test(c ?? ''))) {
    console.error('usage: node scripts/brand/dmg.mjs <brand-folder> --from #rrggbb --to #rrggbb');
    return 2;
  }
  for (const [path, bytes] of dmgBackgroundAssets({ from, to })) {
    const file = join(resolve(dir), 'assets', path);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, bytes);
    console.log(`dmg: wrote ${file}`);
  }
  return 0;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) process.exitCode = main(process.argv.slice(2));
