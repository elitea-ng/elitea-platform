#!/usr/bin/env node
// Rasterises the LE-03 icon pack from its vector sources.
//
//   node brand/scripts/render-icon-pack.mjs [--out <dir>] [--variant <name>]
//
// The sources are brand/assets/icon-pack/src/<variant>/*.svg, which
// libs/ts/client-core/scripts/gen-brand.mjs derives from the canonical mark
// (change the mark there, run gen-brand, then this). The output reproduces the
// approved pack's layout and file names, the .ico included, so a rendered set
// can replace brand/assets/icon-pack/<variant>/ after a design change.
// Measured against the approved PNGs: the median file differs on 0.05% of its
// pixels. The rest is edge antialiasing (the approved 16 and 32 px macOS tiles
// have hard 0/255 corners) and the approved Android foreground, which sits
// 1.5 px up-left of centre; the source centres it. The
// default --out is brand/dist/icon-pack (git-ignored); pass
// --out brand/assets/icon-pack to overwrite the committed rasters on purpose.
//
// Node built-ins only. The rasteriser is the first of resvg, rsvg-convert or
// inkscape found on PATH; set ICON_RASTERIZER to pick one.

import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const BRAND_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const SRC_DIR = join(BRAND_DIR, 'assets/icon-pack/src');
const VARIANTS = ['colored-light', 'colored-dark', 'bw', 'inverted'];

/** Every file of one variant: [folder, file name, source kind, width, height]. */
function plan(v) {
  const sq = (folder, prefix, sizes, kind = 'square') => sizes.map((n) => [folder, `elitea-${v}-${prefix}-${n}.png`, kind, n, n]);
  return [
    ...sq('macos', 'macos', [1024, 512, 256, 128, 64, 32, 16], 'macos'),
    ...sq('windows', 'win', [128, 64, 48, 32, 24, 16]),
    ...sq('android', 'android', [512, 192, 144, 96, 72, 48]),
    ['android', `elitea-${v}-android-foreground-432.png`, 'android-foreground', 432, 432],
    ['android', `elitea-${v}-android-background-432.png`, 'android-background', 432, 432],
    ...sq('ios', 'ios', [1024, 180, 120]),
    ...sq('github', 'github-avatar', [1024, 500]),
    ['github', `elitea-${v}-github-social-1280x640.png`, 'social', 1280, 640],
    ...sq('web', 'web', [512, 192, 32, 16]),
  ];
}

/** The sizes packed into windows/elitea-<variant>.ico, smallest first as in the approved pack. */
const ICO_SIZES = [16, 24, 32, 48, 64, 128, 256];

function onPath(cmd) {
  try {
    execFileSync(process.platform === 'win32' ? 'where' : 'which', [cmd], { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}

function rasterizer() {
  const wanted = process.env.ICON_RASTERIZER;
  const found = (wanted ? [wanted] : ['resvg', 'rsvg-convert', 'inkscape']).find(onPath);
  if (!found) throw new Error('render-icon-pack: no rasteriser on PATH (install resvg, librsvg or Inkscape)');
  return (svg, png, w, h) => {
    const args = {
      resvg: ['-w', `${w}`, '-h', `${h}`, svg, png],
      'rsvg-convert': ['-w', `${w}`, '-h', `${h}`, '-o', png, svg],
      inkscape: [svg, '--export-type=png', `--export-filename=${png}`, '-w', `${w}`, '-h', `${h}`],
    }[found];
    if (!args) throw new Error(`render-icon-pack: unsupported rasteriser ${found}`);
    execFileSync(found, args, { stdio: ['ignore', 'ignore', 'inherit'] });
  };
}

/** An .ico whose entries are PNG payloads (Windows Vista and later). */
export function packIco(images) {
  const header = Buffer.alloc(6 + 16 * images.length);
  header.writeUInt16LE(0, 0);
  header.writeUInt16LE(1, 2);
  header.writeUInt16LE(images.length, 4);
  let offset = header.length;
  images.forEach(({ size, png }, i) => {
    const at = 6 + 16 * i;
    header.writeUInt8(size >= 256 ? 0 : size, at);
    header.writeUInt8(size >= 256 ? 0 : size, at + 1);
    header.writeUInt16LE(1, at + 4);
    header.writeUInt16LE(32, at + 6);
    header.writeUInt32LE(png.length, at + 8);
    header.writeUInt32LE(offset, at + 12);
    offset += png.length;
  });
  return Buffer.concat([header, ...images.map((i) => i.png)]);
}

function main(argv) {
  const opt = (name, fallback) => {
    const i = argv.indexOf(name);
    return i === -1 ? fallback : argv[i + 1];
  };
  const out = resolve(opt('--out', join(BRAND_DIR, 'dist/icon-pack')));
  const only = opt('--variant', null);
  const variants = only === null ? VARIANTS : VARIANTS.filter((v) => v === only);
  if (variants.length === 0) throw new Error(`render-icon-pack: unknown variant ${only}`);
  const render = rasterizer();
  let count = 0;
  for (const v of variants) {
    const src = (kind) => join(SRC_DIR, v, `elitea-${v}-${kind}.svg`);
    for (const [folder, name, kind, w, h] of plan(v)) {
      const png = join(out, v, folder, name);
      mkdirSync(dirname(png), { recursive: true });
      render(src(kind), png, w, h);
      count++;
    }
    const tmp = join(out, '.ico-tmp');
    mkdirSync(tmp, { recursive: true });
    const images = ICO_SIZES.map((size) => {
      const png = join(tmp, `${v}-${size}.png`);
      render(src('square'), png, size, size);
      return { size, png: readFileSync(png) };
    });
    writeFileSync(join(out, v, 'windows', `elitea-${v}.ico`), packIco(images));
    count++;
    rmSync(tmp, { recursive: true, force: true });
  }
  console.log(`render-icon-pack: wrote ${count} files to ${out}`);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  try {
    main(process.argv.slice(2));
  } catch (err) {
    console.error(err.message);
    process.exit(1);
  }
}
