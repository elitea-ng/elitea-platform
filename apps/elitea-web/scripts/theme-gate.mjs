#!/usr/bin/env node
/**
 * spec §4.6 — `npm run theme-gate` (unit F2). Each check is blocking.
 *
 *   1. no raw colours outside the token package    (elitea/no-raw-color via oxlint)
 *   2. no scheme branches                          (grep, §4.6 verbatim)
 *   3. no theme.palette outside shared/brand       (grep)
 *   4. no MUI internal selectors outside overrides (grep)
 *   5. no forked light/dark assets                 (filename scan)
 *   6. no external font/image origins              (grep, index.html + src)
 *   7. brand-pack round trip                       (T1's vitest contract test;
 *      skip-with-notice until src/shared/brand/__tests__/brandPack.contract.test.ts lands)
 *   8. no engine name / vendor docs origin in       (grep, ADR-0024 decision 8 / WP8)
 *      sub-application screens
 *   9. no fontFamily literals outside shared/brand (grep, ADR-0024 WP3)
 *  10. style ratchet: per folder, no more spacing   (oxlint with style-ratchet.oxlintrc.json
 *      in rem/px or off the scale, no more MUI      on the tree and on --base; ADR-0062
 *      Dialog/Switch past their wrappers, than the  decision 3). Without --base it prints
 *      base tree has                                 the counts per folder and does not fail
 *
 * Usage: node scripts/theme-gate.mjs [--root <dir>] [--base <dir>]
 *   --base: the PR's base tree, written by scripts/style-ratchet-base.mjs.
 * Checks 2–6 and 8 logic lives in scripts/lib/theme-gate-core.mjs (unit-tested);
 * this file walks files and orchestrates.
 */
import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

import {
  compareCounts,
  countFindings,
  formatCounts,
  formatGrowth,
  parseOxlintJson,
  totals,
} from './lib/style-ratchet-core.mjs';
import {
  checkExternalOrigins,
  checkFontFamilyLiterals,
  checkForkedAssets,
  checkModeBranches,
  checkMuiSelectors,
  checkSubAppStrings,
  checkThemePalette,
} from './lib/theme-gate-core.mjs';

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const APP_DIR = resolve(SCRIPT_DIR, '..');
const SKIP_DIRS = new Set(['node_modules', 'dist', 'coverage', '.git']);

// Floors on the walk itself (issue #426). Checks 2 to 6 each report PASS on an
// empty file list, and the walk used to skip a missing src/ without a word, so
// five of the seven checks passed on an empty tree. The floor is derived from
// the tree under test: the app tree holds about 3,700 scannable files, and the
// gate self-test points --root at fixture trees of five files each. Both
// floors reject zero.
const MIN_FILES_APP = 500;
const MIN_FILES_FIXTURE = 1;
const BRAND_PACK_CONTRACT_TEST = 'src/shared/brand/__tests__/brandPack.contract.test.ts';

function parseArgs(argv) {
  const args = { root: APP_DIR, base: null };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--root' || argv[i] === '--base') {
      const value = argv[i + 1];
      if (!value) throw new Error(`${argv[i]} requires a directory argument`);
      args[argv[i].slice(2)] = resolve(value);
      i++;
    } else {
      throw new Error(`unknown argument: ${argv[i]}`);
    }
  }
  return args;
}

function* allFiles(dir) {
  for (const entry of readdirSync(dir)) {
    if (SKIP_DIRS.has(entry)) continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      yield* allFiles(full);
    } else {
      yield full;
    }
  }
}

function loadTree(root) {
  const files = [];
  const assetPaths = [];
  const srcRoot = join(root, 'src');
  // A missing src/ used to be skipped in silence, which left checks 2 to 6
  // reporting PASS over an empty file list (issue #426). The caller applies
  // the file-count floor, so report the absence and let the floor speak.
  if (existsSync(srcRoot)) {
    for (const file of allFiles(srcRoot)) {
      const rel = relative(root, file).split('\\').join('/');
      assetPaths.push(rel);
      if (/\.(ts|tsx|js|jsx|html|css)$/.test(rel)) {
        files.push({ path: rel, text: readFileSync(file, 'utf8') });
      }
    }
  } else {
    console.error(`theme-gate: ${srcRoot} does not exist`);
  }
  const indexHtml = join(root, 'index.html');
  if (existsSync(indexHtml)) {
    files.push({ path: 'index.html', text: readFileSync(indexHtml, 'utf8') });
  }
  return { files, assetPaths };
}

function report(name, hits) {
  if (hits.length === 0) {
    console.log(`theme-gate ${name}: PASS`);
    return false;
  }
  console.error(`theme-gate ${name}: FAIL (${hits.length})`);
  for (const hit of hits) {
    console.error(`  ${hit.path}:${hit.line} ${hit.text}`);
  }
  return true;
}

// 10 — the style ratchet (ADR-0062 decision 3). Both trees are counted with
// THIS checkout's config and rule, so a PR that adds a kind counts it on its
// base too and does not fail on code it did not write (decision 3.2).
const STYLE_RATCHET_CONFIG = join(SCRIPT_DIR, 'style-ratchet.oxlintrc.json');

function styleRatchetCounts(oxlint, tree) {
  if (!existsSync(join(tree, 'src'))) {
    throw new Error(`${tree}/src does not exist — a missing tree would read as 0 findings`);
  }
  const run = spawnSync(oxlint, ['-c', STYLE_RATCHET_CONFIG, '-f', 'json', '--no-ignore', 'src'], {
    cwd: tree,
    encoding: 'utf8',
    maxBuffer: 1 << 28,
  });
  try {
    return countFindings(parseOxlintJson(run.stdout));
  } catch (error) {
    throw new Error(`${error.message}\n${run.stderr}`);
  }
}

function styleRatchet(oxlint, root, base) {
  const name = '10-style-ratchet';
  let head;
  let baseCounts;
  try {
    head = styleRatchetCounts(oxlint, root);
    baseCounts = base ? styleRatchetCounts(oxlint, base) : null;
  } catch (error) {
    console.error(`theme-gate ${name}: FAIL — ${error.message}`);
    return true;
  }
  if (!baseCounts) {
    console.log(`theme-gate ${name}: REPORT — no --base tree (nightly or dispatch run), counts per folder:`);
    for (const line of formatCounts(head)) console.log(`  ${line}`);
    return false;
  }
  const growth = compareCounts(baseCounts, head);
  const sum = (counts) => Object.entries(totals(counts)).map(([kind, n]) => `${kind} ${n}`).join(', ');
  if (growth.length === 0) {
    console.log(`theme-gate ${name}: PASS (base: ${sum(baseCounts)}; head: ${sum(head)})`);
    return false;
  }
  console.error(`theme-gate ${name}: FAIL (${growth.length} folder count(s) grew; ADR-0062 decision 3)`);
  for (const line of formatGrowth(growth)) console.error(`  ${line}`);
  return true;
}

function main() {
  const { root, base } = parseArgs(process.argv.slice(2));
  let failed = false;

  // 1 — raw colours, via the same rule implementation the main lint uses.
  const oxlint = join(APP_DIR, 'node_modules', '.bin', 'oxlint');
  const rawColor = spawnSync(oxlint, ['-c', join(SCRIPT_DIR, 'theme-gate.oxlintrc.json'), '--no-ignore', 'src'], {
    cwd: root,
    encoding: 'utf8',
  });
  const rawColorFindings = (rawColor.stdout + rawColor.stderr)
    .split('\n')
    .filter((line) => line.includes('no-raw-color'));
  if (rawColor.status === 0) {
    console.log('theme-gate 1-raw-colors: PASS');
  } else {
    failed = true;
    console.error('theme-gate 1-raw-colors: FAIL');
    for (const line of rawColorFindings) console.error(`  ${line.trim()}`);
    if (rawColorFindings.length === 0) {
      // oxlint failed for a reason other than findings — surface it.
      console.error(rawColor.stdout);
      console.error(rawColor.stderr);
    }
  }

  const { files, assetPaths } = loadTree(root);

  // Floor before any check reports (issue #426). report(name, []) prints PASS,
  // so checks 2 to 6 all passed on an empty tree.
  const minFiles = root === APP_DIR ? MIN_FILES_APP : MIN_FILES_FIXTURE;
  if (files.length < minFiles) {
    console.error(
      `theme-gate: FAIL — loaded ${files.length} scannable file(s) under ${root}, under the floor of ${minFiles}. ` +
        'The walk stopped matching, so a PASS from checks 2 to 6 would prove nothing. Check that the source tree ' +
        'is still at src/ and that its files still carry a .ts/.tsx/.js/.jsx/.html/.css suffix.',
    );
    process.exit(2);
  }

  failed = report('2-mode-branches', checkModeBranches(files)) || failed;
  failed = report('3-theme-palette', checkThemePalette(files)) || failed;
  failed = report('4-mui-selectors', checkMuiSelectors(files)) || failed;
  failed = report('5-forked-assets', checkForkedAssets(assetPaths)) || failed;
  failed = report('6-external-origins', checkExternalOrigins(files)) || failed;
  // 8 — runs before 7 because 7 spawns vitest; a text check has no reason to wait for it.
  failed = report('8-subapp-strings', checkSubAppStrings(files)) || failed;
  // 9 — fontFamily literals outside shared/brand (ADR-0024 WP3). Numbered
  // after check 7 below because 7 is the spec's own numbering; 9 is additive.
  failed = report('9-font-family-literals', checkFontFamilyLiterals(files)) || failed;
  failed = styleRatchet(oxlint, root, base) || failed;

  // 7 — brand-pack round trip (unit T1 owns the test; §4.6 check 7).
  //
  // The self-skip below used to apply to every tree, so a renamed or deleted
  // contract test turned a blocking check into a printed notice (issue #426).
  // The test landed long ago, so for the app tree its absence is now a
  // failure. A --root fixture tree has no brand package and never had this
  // test, so it still skips — and the skip cannot reach the production run,
  // which passes no --root at all.
  const contractTest = join(root, BRAND_PACK_CONTRACT_TEST);
  if (root === APP_DIR && !existsSync(contractTest)) {
    console.error(
      `theme-gate 7-brand-pack: FAIL — ${BRAND_PACK_CONTRACT_TEST} is not in the tree. ` +
        'The brand-pack round trip cannot run. Restore the test under its old name, or point this check at the new one.',
    );
    console.error('theme-gate: FAIL — see findings above (§4.6: every check is blocking)');
    process.exit(1);
  }
  if (existsSync(contractTest)) {
    const vitest = spawnSync(
      join(APP_DIR, 'node_modules', '.bin', 'vitest'),
      ['run', '--config', join(APP_DIR, 'vitest.config.ts'), 'src/shared/brand/__tests__/brandPack.contract.test.ts'],
      { cwd: APP_DIR, encoding: 'utf8', stdio: 'inherit' },
    );
    if (vitest.status === 0) {
      console.log('theme-gate 7-brand-pack: PASS');
    } else {
      failed = true;
      console.error('theme-gate 7-brand-pack: FAIL');
    }
  } else {
    console.log('theme-gate 7-brand-pack: SKIP — src/shared/brand/__tests__/brandPack.contract.test.ts not present yet (unit T1); this check arms itself when the file lands');
  }

  if (failed) {
    console.error('theme-gate: FAIL — see findings above (§4.6: every check is blocking)');
    process.exit(1);
  }
  console.log('theme-gate: OK');
}

main();
