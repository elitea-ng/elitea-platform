#!/usr/bin/env node
// Proves a mounted disk image is laid out as settings.json says (scripts/dmg-settings.mjs),
// instead of trusting that Finder applied it: macos-dmg.sh runs it after styling the
// writable image, and desktop-macos-package runs it again on the finished .dmg.
//
//   node scripts/dmg-verify.mjs <mount-point> <settings.json>
//
// Checks: the app bundle, `Applications` as a symlink to /Applications, the background
// (.background/background.tiff) and the volume icon (.VolumeIcon.icns) are there; the
// .DS_Store Finder wrote holds the icon view (`icvp`) whose background alias names
// background.tiff, the window (`bwsp`), and each icon's position (`Iloc`) where the
// layout puts it.
//
// A .DS_Store record is `<name length u32><name UTF-16BE><code 4cc><type 4cc><data>`;
// a `blob` is `<length u32><bytes>`. The B-tree around the records is not walked: a
// record is found by its name, code and type, which is enough to read it.

import { existsSync, lstatSync, readFileSync, readlinkSync } from 'node:fs';
import { join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

/** The bytes of a record's head: the name's length and UTF-16BE text, the code and the type. */
function recordHead(name, code, type) {
  const text = Buffer.from(name, 'utf16le').swap16();
  const len = Buffer.alloc(4);
  len.writeUInt32BE(name.length);
  return Buffer.concat([len, text, Buffer.from(code + type, 'latin1')]);
}

/** The data of the `blob` record `code` for `name` in a .DS_Store, or null. */
export function blobRecord(dsStore, name, code) {
  const head = recordHead(name, code, 'blob');
  const at = dsStore.indexOf(head);
  if (at < 0) return null;
  const start = at + head.length;
  if (start + 4 > dsStore.length) return null;
  const length = dsStore.readUInt32BE(start);
  if (start + 4 + length > dsStore.length) return null;
  return dsStore.subarray(start + 4, start + 4 + length);
}

/** An icon's position (`Iloc`: x u32, y u32, then 8 bytes) for `name`, or null. */
export function iconLocation(dsStore, name) {
  const blob = blobRecord(dsStore, name, 'Iloc');
  if (!blob || blob.length < 8) return null;
  return { x: blob.readUInt32BE(0), y: blob.readUInt32BE(4) };
}

/** The problems with the .DS_Store `dsStore` against `settings`; empty when it is right. */
export function layoutProblems(dsStore, settings) {
  const problems = [];
  const icvp = blobRecord(dsStore, '.', 'icvp');
  if (!icvp) problems.push('.DS_Store has no icon view settings (icvp): Finder did not save the view');
  else {
    // A binary plist: the keys are ASCII, the alias names the picture's file.
    if (!icvp.includes('backgroundImageAlias')) problems.push('the icon view has no background picture (backgroundImageAlias)');
    if (!icvp.includes('background.tiff')) problems.push('the background alias does not name background.tiff');
  }
  if (!blobRecord(dsStore, '.', 'bwsp')) problems.push('.DS_Store has no window settings (bwsp)');
  for (const [name, want] of [
    [settings.app, settings.appPosition],
    ['Applications', settings.applicationsPosition],
  ]) {
    const got = iconLocation(dsStore, name);
    if (!got) problems.push(`.DS_Store has no position for "${name}"`);
    else if (got.x !== want.x || got.y !== want.y) problems.push(`"${name}" is at ${got.x},${got.y}, the layout says ${want.x},${want.y}`);
  }
  return problems;
}

/** The problems with the volume mounted at `mount` against `settings`. */
export function volumeProblems(mount, settings) {
  const problems = [];
  const at = (rel) => join(mount, rel);
  if (!existsSync(at(`${settings.app}/Contents/Info.plist`))) problems.push(`no app bundle ${settings.app}`);
  let link = null;
  try {
    link = lstatSync(at('Applications')).isSymbolicLink() ? readlinkSync(at('Applications')) : 'not a symlink';
  } catch {
    link = 'missing';
  }
  if (link !== '/Applications') problems.push(`Applications is ${link === 'missing' || link === 'not a symlink' ? link : `a link to ${link}`}, not a link to /Applications`);
  for (const rel of ['.background/background.tiff', '.VolumeIcon.icns']) if (!existsSync(at(rel))) problems.push(`no ${rel}`);
  if (!existsSync(at('.DS_Store'))) problems.push('no .DS_Store: Finder saved no layout');
  else problems.push(...layoutProblems(readFileSync(at('.DS_Store')), settings));
  return problems;
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const [mount, settingsFile] = process.argv.slice(2);
  if (!mount || !settingsFile) {
    console.log('::error::usage: dmg-verify.mjs <mount-point> <settings.json>');
    process.exit(2);
  }
  const settings = JSON.parse(readFileSync(settingsFile, 'utf8'));
  const problems = volumeProblems(mount, settings);
  if (problems.length) {
    for (const p of problems) console.log(`dmg layout: ${p}`);
    process.exitCode = 1;
  } else {
    console.log(`dmg layout: ${settings.app} at ${settings.appPosition.x},${settings.appPosition.y}; Applications -> /Applications at ${settings.applicationsPosition.x},${settings.applicationsPosition.y}; background.tiff; .VolumeIcon.icns`);
  }
}
