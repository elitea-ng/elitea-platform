#!/usr/bin/env node
// Write a full placeholder asset set (every file of the asset table in
// ./schema.mjs) into a brand folder: a plain disc on a tile, in two colours.
// brand/sample's assets come from here, and so do the brand-check test
// fixtures. No ELITEA artwork is involved.
//
//   node scripts/brand/placeholder-assets.mjs <brand-folder> [--fg #rrggbb] [--bg #rrggbb]
//
// The output is deterministic: the same colours write the same bytes.

import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { encodeIcns, encodeIco, ICNS_PNG_TYPE, placeholderPng, placeholderSvg, solidPng } from './raster.mjs';
import { ASSETS } from './schema.mjs';

/** The default placeholder colours: a teal disc on slate. */
export const PLACEHOLDER_COLOURS = Object.freeze({ fg: '#0E7490', bg: '#1F2933' });

/**
 * The bytes of every placeholder asset, keyed by its path in the asset table.
 * Exported so tests can write a brand folder without the CLI.
 */
const cache = new Map();

export function placeholderAssets({ fg, bg } = PLACEHOLDER_COLOURS) {
  const key = `${fg}${bg}`.toLowerCase();
  if (!cache.has(key)) cache.set(key, build(fg, bg));
  return cache.get(key);
}

function build(fg, bg) {
  // The single-colour layers (monochrome, tray) would be the same for every
  // colour pair; their disc size follows the colours so two placeholder
  // brands never share a file.
  const seed = [...`${fg}${bg}`.toLowerCase()].reduce((h, c) => (h * 31 + c.charCodeAt(0)) >>> 0, 7);
  const mono = 0.16 + (seed % 9) / 100;
  const tileOpaque = (size) => placeholderPng(size, { fg, bg, r: 0.3 }, { alpha: false });
  const tileRounded = (size) => placeholderPng(size, { fg, bg, r: 0.3, tile: true });
  const spec = (path) => ASSETS.find((a) => a.path === path);
  const out = new Map();
  for (const a of ASSETS) {
    if (a.optional) continue;
    let bytes;
    switch (a.path) {
      case 'ios/icon-1024.png':
      case 'android/icon-512.png':
        bytes = tileOpaque(a.size);
        break;
      case 'android/adaptive-foreground.png':
        bytes = placeholderPng(a.size, { fg, r: 0.2 });
        break;
      case 'android/adaptive-background.png':
        bytes = solidPng(a.size, bg);
        break;
      case 'android/adaptive-monochrome.png':
        bytes = placeholderPng(a.size, { fg: '#FFFFFF', r: mono });
        break;
      case 'macos/icon.icns':
        bytes = encodeIcns(spec(a.path).sizes.map((s) => ({ type: ICNS_PNG_TYPE[s], png: tileRounded(s) })));
        break;
      case 'windows/icon.ico':
        bytes = encodeIco(spec(a.path).sizes.map((s) => ({ size: s, png: tileRounded(s) })));
        break;
      case 'desktop/tray-template.png':
        bytes = placeholderPng(a.size, { fg: '#000000', r: mono + 0.14 });
        break;
      case 'splash/splash-icon.png':
        bytes = placeholderPng(a.size, { fg, r: 0.2 });
        break;
      case 'web/favicon.svg':
        bytes = Buffer.from(placeholderSvg({ fg, bg }));
        break;
      case 'web/favicon-48.png':
        bytes = tileOpaque(a.size);
        break;
      case 'logo/logo-mark.svg':
        bytes = Buffer.from(placeholderSvg({ fg }));
        break;
      case 'logo/logo-full.svg':
        // A disc and a bar where a wordmark would be: no lettering, so no name to leak.
        bytes = Buffer.from(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1200 372"><circle cx="186" cy="186" r="150" fill="${fg}"/><rect x="420" y="146" width="700" height="80" rx="40" fill="currentColor"/></svg>\n`);
        break;
      default:
        if (a.kind !== 'png') throw new Error(`placeholder-assets: no placeholder for ${a.path}`);
        bytes = tileRounded(a.size);
    }
    out.set(a.path, bytes);
  }
  return out;
}

/** Write the placeholder set into `<brandDir>/assets/`. Returns the paths written. */
export function writePlaceholderAssets(brandDir, colours) {
  const written = [];
  for (const [path, bytes] of placeholderAssets(colours)) {
    const file = join(brandDir, 'assets', path);
    mkdirSync(dirname(file), { recursive: true });
    writeFileSync(file, bytes);
    written.push(path);
  }
  return written;
}

function main(argv) {
  const dir = argv.find((a) => !a.startsWith('--') && !/^#/.test(a));
  const opt = (name) => {
    const at = argv.indexOf(name);
    return at === -1 ? undefined : argv[at + 1];
  };
  if (!dir) {
    console.error('usage: node scripts/brand/placeholder-assets.mjs <brand-folder> [--fg #rrggbb] [--bg #rrggbb]');
    return 2;
  }
  const colours = { fg: opt('--fg') ?? PLACEHOLDER_COLOURS.fg, bg: opt('--bg') ?? PLACEHOLDER_COLOURS.bg };
  for (const c of Object.values(colours)) {
    if (!/^#[0-9a-f]{6}$/i.test(c)) {
      console.error(`placeholder-assets: "${c}" is not #rrggbb`);
      return 2;
    }
  }
  const written = writePlaceholderAssets(resolve(dir), colours);
  console.log(`placeholder-assets: wrote ${written.length} files into ${join(dir, 'assets')}`);
  return 0;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) process.exitCode = main(process.argv.slice(2));
