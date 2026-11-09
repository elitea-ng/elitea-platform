#!/usr/bin/env node
// Stamps a release version into the desktop app before a build (ADR-0052 W2b), the one
// place .github/workflows/desktop-package.yml's macOS and Windows jobs do it:
//
// - Cargo.toml [package] takes the full version (X.Y.Z or X.Y.Z-pre), so
//   `X-Client-Version` (CARGO_PKG_VERSION) says exactly which build this is;
// - tauri.conf.json takes the numeric X.Y.Z (CFBundleShortVersionString, the MSI
//   ProductVersion), and, with a build number, bundle.macOS.bundleVersion.
//
//   node scripts/stamp-version.mjs <version> [build-number]

import { readFileSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { TAURI_DIR } from './tauri-brand.mjs';

const SEMVER = /^(\d+\.\d+\.\d+)(-[0-9A-Za-z.-]+)?$/;

/** Cargo.toml text with the [package] version (the first `version =` line) set. */
export function stampCargo(text, version) {
  let done = false;
  const out = text.replace(/^version = ".*"$/m, () => {
    done = true;
    return `version = "${version}"`;
  });
  if (!done) throw new Error('Cargo.toml: no `version = "…"` line');
  return out;
}

/** The tauri.conf.json object with the numeric version (and build number) set. */
export function stampConf(conf, version, build) {
  const m = SEMVER.exec(version);
  if (!m) throw new Error(`"${version}" is not X.Y.Z or X.Y.Z-pre`);
  const next = { ...conf, version: m[1] };
  if (build !== undefined) {
    if (!/^\d+$/.test(String(build))) throw new Error(`build number "${build}" is not an integer`);
    next.bundle = { ...conf.bundle, macOS: { ...conf.bundle?.macOS, bundleVersion: String(build) } };
  }
  return next;
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const [version, build] = process.argv.slice(2);
  try {
    if (!version) throw new Error('usage: stamp-version.mjs <version> [build-number]');
    const confFile = join(TAURI_DIR, 'tauri.conf.json');
    const cargoFile = join(TAURI_DIR, 'Cargo.toml');
    const conf = stampConf(JSON.parse(readFileSync(confFile, 'utf8')), version, build);
    const cargo = stampCargo(readFileSync(cargoFile, 'utf8'), version);
    writeFileSync(confFile, `${JSON.stringify(conf, null, 2)}\n`);
    writeFileSync(cargoFile, cargo);
    console.log(`stamped: Cargo.toml ${version}, tauri.conf.json ${conf.version}${build ? ` (build ${build})` : ''}`);
  } catch (error) {
    console.log(`::error::${error.message}`);
    process.exitCode = 1;
  }
}
