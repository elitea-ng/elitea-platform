#!/usr/bin/env node
// Fail early, in plain words, when the pinned ELITEA release cannot build a
// brand repository: it predates the brand system (ADR-0052), or it lacks the
// packaging actions this template calls.
//
//   node scripts/check-release.mjs <elitea-checkout>
//
// Node's standard library only.

import { existsSync } from 'node:fs';
import { join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

/** What a release must carry, and what a missing entry means. */
export const REQUIRED = Object.freeze([
  ['scripts/brand/load.mjs', 'the brand loader (ELITEA_BRAND)'],
  ['scripts/brand-check.mjs', 'brand-check'],
  ['apps/elitea-mobile/app.config.ts', 'the mobile app'],
  ['apps/elitea-desktop/scripts/tauri-brand.mjs', "the desktop app's brand overlay"],
  ['.github/actions/desktop-macos-package/action.yml', 'the macOS packaging action'],
  ['.github/actions/desktop-windows-package/action.yml', 'the Windows packaging action'],
]);

/** The missing entries under `root`, as messages (empty: the release is usable). */
export function missing(root) {
  return REQUIRED.filter(([path]) => !existsSync(join(root, path))).map(([path, what]) => `${path} (${what})`);
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const root = process.argv[2];
  if (!root) {
    console.error('usage: check-release.mjs <elitea-checkout>');
    process.exit(2);
  }
  const gaps = missing(root);
  if (gaps.length) {
    console.log(
      `::error::the pinned ELITEA release cannot build a brand repository; it lacks ${gaps.join(', ')}. Pin a newer release in upstream.json.`,
    );
    process.exit(1);
  }
  console.log('the pinned release carries everything a brand repository builds with');
}
