#!/usr/bin/env node
// i18n-backfill.mjs — the en.json sync backfill + CI gate (issue #44).
//
// Walks src/**/*.{ts,tsx} (excluding test/story files), extracts every
// `t(key, fallback)` call site bound to `t` imported from `@/shared/i18n`
// (the pre-S8 `@/shared/ui/lib/t` stub was removed by issue #45), and either:
//
//   - default mode: merges every safely-addable key into en.json (2-space
//     indent, existing key order preserved, new keys appended sorted).
//   - --check mode: adds nothing; exits 1 if there's anything that WOULD be
//     added, or any unresolved conflict — this is the CI enforcement gate
//     (wired into ci-web.yml's gate-i18n-sync job), sharing the exact same
//     extraction logic as the writer so the two can never define "a valid
//     call site" differently from each other.
//
// Never silently resolves a conflict — a new key with disagreeing call-site
// fallbacks, an existing key whose shipped text has drifted from its call
// site(s), and a call site needing a hand-written i18next-interpolation
// fallback (a template literal with `${}` expressions) are all reported and
// left for a human. Decision logic lives in scripts/lib/i18n-backfill-core.mjs.
//
// Usage:
//   node scripts/i18n-backfill.mjs [--check]
//
// Two catalogues (ADR-0029): src/shared/i18n/en.json, which every entry
// ships, and src/entries/desktop/i18n/en.desktop.json, which only the desktop
// entry loads. A key whose every call site is in a desktop-only module
// (`isDesktopOnlyPath`) belongs to the desktop one. --check fails on a key in
// the wrong catalogue or in both; the default mode adds new keys to the right
// one and moves misplaced keys across (their text unchanged).
import { readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { checkFloors } from './lib/gate-floor.mjs';
import { extractCallSites, planBackfill, planCatalogueSplit, routeNewKeys } from './lib/i18n-backfill-core.mjs';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const appRoot = resolve(scriptDir, '..');

/*
 * Floors on the scan (issue #528).
 *
 * `--check` decides on `addedKeys`, `hasUnresolvedConflicts` and
 * `parseErrors`. All three are empty when the EXTRACTION finds nothing, and
 * the gate then reports OK over an empty plan. `fileCount` and
 * `allEntries.length` were printed and gated nothing.
 *
 * The vector is cheap to hit. Extraction binds to `t` imported from
 * `@/shared/i18n`; rename that module and every call site stops matching at
 * once. A skipped directory name, or a change to the source-file filter, does
 * the same to `fileCount`.
 *
 * Measured on 2026-08-28: 2604 source files, 2812 `t()` call sites, 2702 keys
 * in en.json.
 */
const MIN_SOURCE_FILES = 1500;
const MIN_CALL_SITES = 1500;
const MIN_EN_KEYS = 1500;

const SOURCE_RE = /\.tsx?$/;
const EXCLUDE_RE = /\.(test|spec|stories)\.tsx?$/;
const SKIP_DIRS = new Set(['node_modules', 'dist', 'coverage']);

function* sourceFiles(dir) {
  for (const entry of readdirSync(dir)) {
    if (SKIP_DIRS.has(entry)) continue;
    const full = join(dir, entry);
    const info = statSync(full);
    if (info.isDirectory()) {
      yield* sourceFiles(full);
    } else if (SOURCE_RE.test(entry) && !EXCLUDE_RE.test(entry)) {
      yield full;
    }
  }
}

function parseArgs(argv) {
  const opts = { check: false };
  for (const arg of argv) {
    if (arg === '--check') opts.check = true;
    else if (arg === '--help' || arg === '-h') {
      console.log('usage: i18n-backfill.mjs [--check]');
      process.exit(0);
    } else {
      console.error(`unknown argument: ${arg}`);
      process.exit(2);
    }
  }
  return opts;
}

/** Acceptance-bar self-check: every key this run would add must be
 * byte-identical to a single agreed call-site fallback — never trust
 * planBackfill's own bookkeeping without re-deriving it independently. */
function assertToAddIsByteIdentical(toAdd, allEntries) {
  const variantsByKey = new Map();
  for (const entry of allEntries) {
    let variants = variantsByKey.get(entry.key);
    if (!variants) {
      variants = new Set();
      variantsByKey.set(entry.key, variants);
    }
    variants.add(entry.fallback);
  }
  for (const [key, fallback] of Object.entries(toAdd)) {
    const variants = variantsByKey.get(key);
    if (!variants || variants.size !== 1 || !variants.has(fallback)) {
      throw new Error(
        `i18n-backfill: internal invariant violated — toAdd["${key}"] is not byte-identical to a single agreed call-site fallback`,
      );
    }
  }
}

function formatSites(sites) {
  return sites.map((s) => `${s.filename}:${s.line}`).join(', ');
}

function reportConflicts(conflicts) {
  for (const { key, variants } of conflicts) {
    console.log(`CONFLICT  "${key}" — call sites disagree on fallback text, never auto-resolved:`);
    for (const { fallback, sites } of variants) {
      console.log(`          ${JSON.stringify(fallback)}  <- ${formatSites(sites)}`);
    }
  }
}

function reportDrifted(drifted) {
  for (const { key, shipped, variants } of drifted) {
    console.log(`DRIFT     "${key}" — shipped en.json text no longer matches its call site(s):`);
    console.log(`          shipped:   ${JSON.stringify(shipped)}`);
    for (const { fallback, sites } of variants) {
      console.log(`          call site: ${JSON.stringify(fallback)}  <- ${formatSites(sites)}`);
    }
  }
}

/** Splits flagged interpolation call sites into already-hand-resolved (key
 * already shipped in en.json) vs unresolved (key missing — this IS a
 * missing key, the tool just can't safely author its fallback text). */
function reportFlagged(flagged, existingEn) {
  const parseErrors = flagged.filter((f) => f.reason === 'parse-error');
  const dynamicKeys = flagged.filter((f) => f.reason === 'dynamic-key');
  const interpolated = flagged.filter((f) => f.reason === 'interpolated-fallback');
  const unresolvedInterpolated = interpolated.filter((f) => !Object.hasOwn(existingEn, f.key));

  for (const f of parseErrors) {
    console.log(`PARSE-ERROR  ${f.filename}:${f.line} — ${f.detail}`);
  }
  for (const f of dynamicKeys) {
    console.log(`FLAG dynamic-key          ${f.filename}:${f.line}  ${f.detail}  (non-literal key — can't check against en.json)`);
  }
  for (const f of interpolated) {
    const resolved = Object.hasOwn(existingEn, f.key) ? 'hand-resolved' : 'UNRESOLVED';
    console.log(`FLAG interpolated-fallback ${f.filename}:${f.line}  key="${f.key}"  ${f.detail}  [${resolved} — needs a hand-written i18next-interpolation fallback]`);
  }

  return { parseErrors, dynamicKeys, interpolated, unresolvedInterpolated };
}

function main() {
  const opts = parseArgs(process.argv.slice(2));
  const enPath = join(appRoot, 'src/shared/i18n/en.json');
  const desktopPath = join(appRoot, 'src/entries/desktop/i18n/en.desktop.json');
  const sharedEn = JSON.parse(readFileSync(enPath, 'utf8'));
  const desktopEn = JSON.parse(readFileSync(desktopPath, 'utf8'));
  // Every call site resolves against both: which one a key sits in is the split check's question.
  const existingEn = { ...sharedEn, ...desktopEn };

  const allEntries = [];
  const allFlagged = [];
  let fileCount = 0;

  for (const file of sourceFiles(join(appRoot, 'src'))) {
    fileCount++;
    const rel = relative(appRoot, file);
    const { entries, flagged } = extractCallSites(rel, readFileSync(file, 'utf8'));
    allEntries.push(...entries);
    allFlagged.push(...flagged);
  }

  const plan = planBackfill(existingEn, allEntries);
  assertToAddIsByteIdentical(plan.toAdd, allEntries);

  const addedKeys = Object.keys(plan.toAdd).sort();
  console.log(`i18n-backfill: scanned ${fileCount} files, ${allEntries.length} t() call site(s), ${allFlagged.length} flagged`);

  // The floors run before the verdict, in both modes. An extraction that
  // matches nothing produces an empty plan, and an empty plan reads as a
  // synchronised en.json.
  const floors = checkFloors('i18n-backfill', [
    { subject: 'source files scanned under src/', observed: fileCount, floor: MIN_SOURCE_FILES },
    { subject: 't() call sites bound to @/shared/i18n', observed: allEntries.length, floor: MIN_CALL_SITES },
    { subject: 'keys shipped in src/shared/i18n/en.json', observed: Object.keys(sharedEn).length, floor: MIN_EN_KEYS },
  ]);
  for (const line of floors.lines) console.log(line);
  if (!floors.ok) {
    console.error(floors.error);
    process.exit(2);
  }

  reportConflicts(plan.conflicts);
  reportDrifted(plan.drifted);
  const { unresolvedInterpolated, parseErrors } = reportFlagged(allFlagged, existingEn);

  const hasUnresolvedConflicts = plan.conflicts.length > 0 || plan.drifted.length > 0 || unresolvedInterpolated.length > 0;
  const split = planCatalogueSplit(sharedEn, desktopEn, allEntries);
  for (const { key, sites } of split.toDesktop) {
    console.log(`MISPLACED "${key}" — in en.json but called only from desktop-only modules; belongs in en.desktop.json  <- ${formatSites(sites)}`);
  }
  for (const { key, sites } of split.toShared) {
    console.log(`MISPLACED "${key}" — in en.desktop.json but called from a module the web build ships; belongs in en.json  <- ${formatSites(sites)}`);
  }
  for (const key of split.duplicated) {
    console.log(`DUPLICATE "${key}" — in both en.json and en.desktop.json`);
  }
  const misplaced = split.toDesktop.length + split.toShared.length + split.duplicated.length;

  if (opts.check) {
    if (addedKeys.length > 0) {
      console.log(`i18n-backfill --check: ${addedKeys.length} missing key(s) would be added: ${addedKeys.join(', ')}`);
    }
    const fail = addedKeys.length > 0 || hasUnresolvedConflicts || parseErrors.length > 0 || misplaced > 0;
    console.log(fail ? 'i18n-backfill --check: FAIL' : 'i18n-backfill --check: OK');
    process.exit(fail ? 1 : 0);
  }

  const routed = routeNewKeys(plan.toAdd, allEntries);
  const nextShared = { ...sharedEn };
  const nextDesktop = { ...desktopEn };
  for (const { key } of split.toDesktop) {
    nextDesktop[key] = sharedEn[key];
    delete nextShared[key];
  }
  for (const { key } of split.toShared) {
    nextShared[key] = desktopEn[key];
    delete nextDesktop[key];
  }
  for (const key of split.duplicated) delete nextDesktop[key];
  for (const key of Object.keys(routed.shared).sort()) nextShared[key] = routed.shared[key];
  for (const key of Object.keys(routed.desktop).sort()) nextDesktop[key] = routed.desktop[key];

  if (addedKeys.length > 0 || misplaced > 0) {
    writeFileSync(enPath, `${JSON.stringify(nextShared, null, 2)}\n`);
    writeFileSync(desktopPath, `${JSON.stringify(nextDesktop, null, 2)}\n`);
    console.log(`i18n-backfill: wrote ${addedKeys.length} new key(s) (${Object.keys(routed.desktop).length} to en.desktop.json), moved ${misplaced} misplaced key(s): ${addedKeys.join(', ')}`);
  } else {
    console.log('i18n-backfill: no missing keys to add');
  }
  if (hasUnresolvedConflicts) {
    console.log('i18n-backfill: unresolved conflicts/drift/interpolation above need hand resolution (never auto-picked)');
  }
}

main();
