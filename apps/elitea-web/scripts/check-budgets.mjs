#!/usr/bin/env node
/**
 * spec §3.5 — complexity budgets gate (unit F2). A breach fails the build;
 * there is no warning tier.
 *
 * Usage: node scripts/check-budgets.mjs [--root <dir>]
 *   --root defaults to the app directory (parent of scripts/); the gate
 *   self-test points it at fixture trees.
 *
 * Decision logic lives in scripts/lib/budgets-core.mjs (100% covered by
 * scripts/lib/budgets-core.test.mjs). This file only walks the tree and
 * reports.
 */
import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

import { DEFAULT_LIMITS, checkFile } from './lib/budgets-core.mjs';

const SOURCE_RE = /\.(ts|tsx|js|jsx|mts|cts)$/;
const SKIP_DIRS = new Set(['node_modules', 'dist', 'coverage', '.git']);
const APP_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');

// Floor on the walk itself (issue #426). This gate printed the file count and
// asserted nothing about it, so an empty src/ produced "OK — 0 files within
// §3.5 budgets". Its sibling gates (check-handlers.mjs,
// check-bundle-secrets.mjs) all refuse to pass on
// a count of zero.
//
// The floor is derived from the tree under test, not hardcoded to one number:
// the app tree holds about 3,600 source files, and the gate self-test points
// --root at fixture trees of five files each. Both floors reject zero.
const MIN_FILES_APP = 500;
const MIN_FILES_FIXTURE = 1;

function parseArgs(argv) {
  const args = { root: APP_DIR };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--root') {
      const value = argv[i + 1];
      if (!value) throw new Error('--root requires a directory argument');
      args.root = resolve(value);
      i++;
    } else {
      throw new Error(`unknown argument: ${argv[i]}`);
    }
  }
  return args;
}

function* sourceFiles(dir) {
  // An absent directory yields nothing rather than throwing, so the file-count
  // floor in main() reports it with a message the reader can act on.
  if (!existsSync(dir)) return;
  for (const entry of readdirSync(dir)) {
    if (SKIP_DIRS.has(entry)) continue;
    const full = join(dir, entry);
    const info = statSync(full);
    if (info.isDirectory()) {
      yield* sourceFiles(full);
    } else if (SOURCE_RE.test(entry)) {
      yield full;
    }
  }
}

function main() {
  const { root } = parseArgs(process.argv.slice(2));
  const srcRoot = join(root, 'src');
  let files = 0;
  const findings = [];

  for (const file of sourceFiles(srcRoot)) {
    files++;
    const rel = relative(root, file);
    findings.push(...checkFile(rel, readFileSync(file, 'utf8'), DEFAULT_LIMITS));
  }

  const minFiles = root === APP_DIR ? MIN_FILES_APP : MIN_FILES_FIXTURE;
  if (files < minFiles) {
    console.error(
      `check-budgets: FAIL — walked ${files} source files under ${srcRoot}, under the floor of ${minFiles}. ` +
        'The walk stopped matching, so a clean result here proves nothing. Check that the source tree is still ' +
        'at src/ and that its files still carry a .ts/.tsx/.js/.jsx/.mts/.cts suffix.',
    );
    process.exit(2);
  }

  for (const f of findings) {
    console.error(`BUDGET ${f.rule}: ${f.file}:${f.line} — ${f.message}`);
  }
  if (findings.length > 0) {
    console.error(`check-budgets: ${findings.length} budget breach(es) in ${files} files — failing (§3.5 has no warning tier)`);
    process.exit(1);
  }
  console.log(`check-budgets: OK — ${files} files (floor ${minFiles}) within §3.5 budgets (file-length/component-props/use-effects/hook-deps/slice-public-api)`);
}

main();
