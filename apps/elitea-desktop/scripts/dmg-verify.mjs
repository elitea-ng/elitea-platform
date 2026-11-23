#!/usr/bin/env node
// Proves a mounted disk image is laid out as settings.json says (scripts/dmg-settings.mjs),
// instead of trusting that Finder applied it: macos-dmg.sh runs it after styling the
// writable image, and desktop-macos-package runs it again on the finished .dmg.
//
//   node scripts/dmg-verify.mjs <mount-point> <settings.json>
//
// Checks: the app bundle, `Applications` as a symlink to /Applications, the background
// (.background/background.tiff) and the volume icon (.VolumeIcon.icns) are there; and
// the .DS_Store Finder wrote holds, in its live B-tree (never a stale record left in a
// free block), the icon view (`icvp`: a picture background whose alias names
// background.tiff, the icon and text sizes, no arrangement), the window (`bwsp`: its
// size, and every bar off: toolbar, status bar, tab bar, path bar, sidebar) and each
// icon's position (`Iloc`). The view's background colour is white (Finder shows it
// before the picture loads; the picture is light, as the labels are drawn dark).
//
// The .DS_Store and property-list readers are ds-store.mjs. Node's standard library only.

import { existsSync, lstatSync, readFileSync, readlinkSync } from 'node:fs';
import { join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { dsStoreRecords, HIDDEN_CHROME, parseBplist } from './ds-store.mjs';

export { dsStoreRecords, parseBplist };

const record = (records, name, code) => records.find((r) => r.name === name && r.code === code);

/** An icon's position (`Iloc` blob: x u32, y u32, then 8 bytes) for `name`, or null. */
export function iconLocation(records, name) {
  const r = record(records, name, 'Iloc');
  if (!r || r.type !== 'blob' || r.value.length < 8) return null;
  return { x: r.value.readUInt32BE(0), y: r.value.readUInt32BE(4) };
}

/** The problems with the .DS_Store bytes `dsStore` against `settings`; empty when it is right. */
export function layoutProblems(dsStore, settings) {
  let records;
  try {
    records = dsStoreRecords(dsStore);
  } catch (error) {
    return [error.message];
  }
  const problems = [];
  const plist = (code, what) => {
    const r = record(records, '.', code);
    if (!r || r.type !== 'blob') {
      problems.push(`.DS_Store has no ${what} (${code}): Finder did not save it`);
      return null;
    }
    try {
      return parseBplist(r.value);
    } catch (error) {
      problems.push(`${code}: ${error.message}`);
      return null;
    }
  };
  const view = plist('icvp', 'icon view settings');
  if (view) {
    // backgroundType 2: a picture (0 is the default white, 1 a colour).
    if (view.backgroundType !== 2) problems.push(`the icon view's background is type ${view.backgroundType}, not a picture (2)`);
    const alias = view.backgroundImageAlias;
    if (!Buffer.isBuffer(alias) || !alias.includes('background.tiff')) problems.push('the icon view has no background alias naming background.tiff');
    if (view.iconSize !== settings.iconSize) problems.push(`the icons are ${view.iconSize} points, the layout says ${settings.iconSize}`);
    if (view.textSize !== settings.textSize) problems.push(`the labels are ${view.textSize} points, the layout says ${settings.textSize}`);
    if (view.arrangeBy !== 'none') problems.push(`the icons are arranged by ${view.arrangeBy}, so Finder would move them`);
    const colour = [view.backgroundColorRed, view.backgroundColorGreen, view.backgroundColorBlue];
    if (!colour.every((c) => typeof c === 'number' && Math.abs(c - 1) < 0.001)) problems.push(`the view's background colour is ${colour.join(',')}, not white (behind the picture until it loads)`);
  }
  const win = plist('bwsp', 'window settings');
  if (win) {
    const m = /^\{\{(-?\d+), (-?\d+)\}, \{(\d+), (\d+)\}\}$/.exec(String(win.WindowBounds ?? ''));
    const want = [settings.window.width, settings.window.height + settings.window.titleBar];
    if (!m) problems.push(`the window has no bounds (${win.WindowBounds})`);
    else if (Number(m[3]) !== want[0] || Number(m[4]) !== want[1]) problems.push(`the window is ${m[3]}x${m[4]}, the layout says ${want[0]}x${want[1]}`);
    const shown = Object.keys(HIDDEN_CHROME).filter((k) => win[k] !== false);
    if (shown.length) problems.push(`the window does not hide ${shown.join(', ')}: only the artwork may show`);
  }
  for (const [name, want] of [
    [settings.app, settings.appPosition],
    ['Applications', settings.applicationsPosition],
  ]) {
    const got = iconLocation(records, name);
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
  let link;
  try {
    link = lstatSync(at('Applications')).isSymbolicLink() ? readlinkSync(at('Applications')) : null;
  } catch {
    link = undefined;
  }
  if (link === undefined) problems.push('Applications is missing, not a link to /Applications');
  else if (link === null) problems.push('Applications is not a symlink, not a link to /Applications');
  else if (link !== '/Applications') problems.push(`Applications is a link to ${link}, not a link to /Applications`);
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
    const { window: w, iconSize, appPosition: a, applicationsPosition: f } = settings;
    console.log(`dmg layout: window ${w.width}x${w.height}, ${iconSize}-point icons, picture background.tiff; ${settings.app} at ${a.x},${a.y}; Applications -> /Applications at ${f.x},${f.y}; .VolumeIcon.icns`);
  }
}
