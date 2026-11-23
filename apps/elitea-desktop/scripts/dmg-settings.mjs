#!/usr/bin/env node
// The disk image's brand inputs (ADR-0052 decision 4): everything the styled .dmg
// shows comes from the brand ELITEA_BRAND selects, nothing from this script.
//
//   node scripts/dmg-settings.mjs <out-dir> <version>
//
// writes into <out-dir>:
//   settings.json         the volume name (`<shortName> <version>`, which is also the
//                         Finder window's title), the app bundle's name and the fixed
//                         window layout (scripts/brand/dmg.mjs DMG_LAYOUT)
//   background.png        660x400, and background@2x.png 1320x800: the brand's
//                         assets/macos/dmg-background{,@2x}.png, or, when the brand
//                         ships none, drawn from its hue (dmg.mjs)
//   volume.icns           the brand's assets/macos/icon.icns, the volume's icon
//
// scripts/macos-dmg.sh reads them; scripts/dmg-verify.mjs checks the result against
// settings.json.

import { copyFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { DMG_LAYOUT, dmgBackgroundPng } from '../../../scripts/brand/dmg.mjs';
import { loadBrand } from '../../../scripts/brand/load.mjs';

/** The second colour of a background drawn for a brand that ships none: a neutral slate. */
export const FALLBACK_TO = '#334155';

const VERSION = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/;

/**
 * The settings for `brand` (a loaded brand, scripts/brand/load.mjs) at `version`.
 * `background` says where the picture comes from: `brand` (its own files) or `hue`.
 */
export function dmgSettings(brand, version) {
  if (!VERSION.test(String(version ?? ''))) throw new Error(`dmg settings: "${version}" is not X.Y.Z or X.Y.Z-pre`);
  const m = brand.manifest;
  const has = ['assets/macos/dmg-background.png', 'assets/macos/dmg-background@2x.png'].map((p) => brand.has(p));
  // One alone is a mistake brand-check reports; never drop the brand's artwork in silence.
  if (has[0] !== has[1]) throw new Error(`dmg settings: ${brand.id} ships one of assets/macos/dmg-background.png and dmg-background@2x.png; it needs both, or neither`);
  const own = has[0];
  return {
    volumeName: `${m.shortName} ${version}`,
    app: `${m.shortName}.app`,
    background: own ? 'brand' : 'hue',
    window: { width: DMG_LAYOUT.width, height: DMG_LAYOUT.height, titleBar: DMG_LAYOUT.titleBar },
    iconSize: DMG_LAYOUT.iconSize,
    textSize: DMG_LAYOUT.textSize,
    appPosition: { ...DMG_LAYOUT.app },
    applicationsPosition: { ...DMG_LAYOUT.applications },
  };
}

/** Writes settings.json, the two backgrounds and volume.icns into `out`; returns the settings. */
export function writeDmgInputs(brand, version, out) {
  const settings = dmgSettings(brand, version);
  mkdirSync(out, { recursive: true });
  if (settings.background === 'brand') {
    copyFileSync(brand.path('assets/macos/dmg-background.png'), join(out, 'background.png'));
    copyFileSync(brand.path('assets/macos/dmg-background@2x.png'), join(out, 'background@2x.png'));
  } else {
    const hue = brand.manifest.brand.hue;
    writeFileSync(join(out, 'background.png'), dmgBackgroundPng({ from: hue, to: FALLBACK_TO, scale: 1 }));
    writeFileSync(join(out, 'background@2x.png'), dmgBackgroundPng({ from: hue, to: FALLBACK_TO, scale: 2 }));
  }
  copyFileSync(brand.path('assets/macos/icon.icns'), join(out, 'volume.icns'));
  writeFileSync(join(out, 'settings.json'), `${JSON.stringify(settings, null, 2)}\n`);
  return settings;
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const [out, version] = process.argv.slice(2);
  try {
    if (!out || !version) throw new Error('usage: dmg-settings.mjs <out-dir> <version>');
    const brand = loadBrand();
    const s = writeDmgInputs(brand, version, resolve(out));
    console.log(`dmg: "${s.volumeName}", ${s.app}, background from the ${s.background === 'brand' ? `brand (${brand.id})` : `hue of ${brand.id}`}`);
  } catch (error) {
    console.log(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
