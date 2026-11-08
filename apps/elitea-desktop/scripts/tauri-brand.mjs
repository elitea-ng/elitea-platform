#!/usr/bin/env node
// Writes src-tauri/tauri.brand.json, the Tauri configuration overlay of the brand
// ELITEA_BRAND selects (ADR-0052 decision 4): the product name, the identifier,
// the bundle icons and tray image, the publisher and copyright. Every build merges
// it over tauri.conf.json, which holds the neutral brand/sample values:
//
//   tauri build --config src-tauri/tauri.brand.json      (the CLI, npm scripts, release)
//   TAURI_CONFIG="$(cat src-tauri/tauri.brand.json)" cargo test   (cargo alone, CI)
//
// The overlay is generated, never committed (.gitignore). Brand files are named by
// paths relative to src-tauri, where Tauri resolves them, so it holds no machine path.
//
//   node scripts/tauri-brand.mjs                write the overlay
//   node scripts/tauri-brand.mjs --print        print it instead
//   node scripts/tauri-brand.mjs --github-env   also export it as TAURI_CONFIG to the
//                                               later steps of a GitHub Actions job

import { appendFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { loadBrand } from '../../../scripts/brand/load.mjs';

/** The Tauri project folder (src-tauri), the base every relative path in the config is read from. */
export const TAURI_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '../src-tauri');
export const OVERLAY_FILE = join(TAURI_DIR, 'tauri.brand.json');

/**
 * The overlay for `brand` (a loaded brand, scripts/brand/load.mjs).
 *
 * The desktop app is named by the brand's `shortName`: the macOS app menu, the
 * bundle (`<shortName>.app`) and every host string ("About Elitea"), as it has always
 * been for ELITEA. The identifier names the config, data, log and credentials folders.
 */
export function tauriOverlay(brand, tauriDir = TAURI_DIR) {
  const m = brand.manifest;
  const asset = (rel) => brand.relativeTo(tauriDir, `assets/${rel}`);
  return {
    productName: m.shortName,
    identifier: m.desktop.identifier,
    app: {
      // Read into the build (tauri::generate_context!) for the menu-bar item the host
      // builds itself (src/activity.rs); the host drops it from the config before start,
      // so Tauri creates no tray of its own.
      trayIcon: { iconPath: asset('desktop/tray-template.png'), iconAsTemplate: true },
    },
    bundle: {
      icon: [asset('desktop/32x32.png'), asset('desktop/128x128.png'), asset('desktop/128x128@2x.png'), asset('macos/icon.icns'), asset('windows/icon.ico')],
      publisher: m.publisher,
      copyright: `Copyright © ${m.publisher}`,
    },
  };
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const brand = loadBrand();
  const text = `${JSON.stringify(tauriOverlay(brand), null, 2)}\n`;
  if (process.argv.includes('--print')) {
    process.stdout.write(text);
  } else {
    writeFileSync(OVERLAY_FILE, text);
    if (process.argv.includes('--github-env')) {
      if (!process.env.GITHUB_ENV) throw new Error('--github-env: GITHUB_ENV is not set (not a GitHub Actions step)');
      // One line, so the KEY=value form needs no delimiter: cargo alone (clippy, test) reads it.
      appendFileSync(process.env.GITHUB_ENV, `TAURI_CONFIG=${JSON.stringify(tauriOverlay(brand))}\n`);
    }
    console.log(`tauri.brand.json: ${brand.id} (${brand.manifest.desktop.identifier}, "${brand.manifest.shortName}")`);
  }
}
