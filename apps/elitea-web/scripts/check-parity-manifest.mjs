#!/usr/bin/env node
/**
 * Validates parity/manifest.json and its shards (spec §8.3).
 *
 * This is what is left of `tools/uictl parity-manifest --validate`, which
 * was deleted with the apps/elitea-ui baseline submodule. The manifest is
 * still read by live gates (endpoint manifest, contract coverage, visual
 * coverage), so its schema and its append-only rules still matter:
 *
 *   1. shard bookkeeping, unknown keys and the §8.3 item schema
 *      (scripts/lib/parity-manifest-core.mjs);
 *   2. against a BASE manifest read from git: no id may disappear, and an
 *      item's acceptance text may change only when it carries a waiver.
 *
 * The base. A pull request compares against its base branch as origin holds
 * it now: pass the branch in PARITY_BASE_REF (ci-web.yml passes
 * github.base_ref) or as `--base <branch>`. The ref is fetched at depth 1 if
 * the checkout lacks it, and a base that cannot be resolved FAILS the gate —
 * a check that reads only the pull request's own tree cannot see a deleted id.
 * (uictl compared against HEAD, which in a pull-request checkout is the merge
 * commit itself, so this half of it never saw a pull request's own change.)
 * With no base given (a push, or a local run) it compares against HEAD, which
 * catches uncommitted edits only; the pull request run is the real guard.
 *
 * Not ported: resolving each `source` anchor in the baseline checkout. The
 * anchors are checked for format only.
 */
import { execFileSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { checkImmutability, mergeManifest, validateManifest } from './lib/parity-manifest-core.mjs';

const APP_DIR = join(dirname(fileURLToPath(import.meta.url)), '..');
const PARITY_DIR = join(APP_DIR, 'parity');
const ROOT_PATH = join(PARITY_DIR, 'manifest.json');

function git(args) {
  return execFileSync('git', args, { cwd: APP_DIR, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'], maxBuffer: 64 * 1024 * 1024 });
}

function tryGit(args) {
  try {
    return git(args);
  } catch {
    return null;
  }
}

function parseArgs(argv) {
  let base = process.env.PARITY_BASE_REF || '';
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--base' && argv[i + 1]) {
      base = argv[++i];
    } else {
      console.error(`check-parity-manifest: unknown argument ${argv[i]} (usage: [--base <branch>])`);
      process.exit(2);
    }
  }
  return base;
}

/** Resolves a base branch to a commit-ish, fetching it at depth 1 if needed. */
function resolveBase(branch) {
  const remote = `refs/remotes/origin/${branch}`;
  const have = () => tryGit(['rev-parse', '--verify', '--quiet', `${remote}^{commit}`]);
  if (have() === null) {
    tryGit(['fetch', '--no-tags', '--quiet', '--depth=1', 'origin', `+refs/heads/${branch}:${remote}`]);
  }
  return have() === null ? null : remote;
}

function readJson(path) {
  return JSON.parse(readFileSync(path, 'utf8'));
}

function loadBaseItems(commitish) {
  const repoRoot = git(['rev-parse', '--show-toplevel']).trim();
  const rel = (path) => relative(repoRoot, path).split('\\').join('/');
  const rootBlob = tryGit(['show', `${commitish}:${rel(ROOT_PATH)}`]);
  if (rootBlob === null) return null;
  const root = JSON.parse(rootBlob);
  const items = [...(root.items ?? [])];
  for (const ref of root.shards ?? []) {
    const shardBlob = tryGit(['show', `${commitish}:${rel(join(PARITY_DIR, ref.path))}`]);
    // A shard the base does not hold is new: its items are all additions.
    if (shardBlob !== null) items.push(...(JSON.parse(shardBlob).items ?? []));
  }
  return items;
}

function main() {
  const baseBranch = parseArgs(process.argv.slice(2));
  const root = readJson(ROOT_PATH);
  const { items, problems } = mergeManifest(root, (path) => {
    const full = join(PARITY_DIR, path);
    return existsSync(full) ? readJson(full) : null;
  });
  problems.push(...validateManifest(root, items));

  let baseLabel = 'HEAD';
  let commitish = 'HEAD';
  if (baseBranch) {
    const resolved = resolveBase(baseBranch);
    if (resolved === null) {
      console.error(`check-parity-manifest: cannot resolve base branch "${baseBranch}" — refusing to report a pass without a base to compare against`);
      process.exit(1);
    }
    commitish = resolved;
    baseLabel = `${baseBranch} (${resolved})`;
  }
  const baseItems = loadBaseItems(commitish);
  if (baseItems === null) {
    console.log(`check-parity-manifest: ${baseLabel} holds no parity manifest; append-only rules have nothing to protect`);
  } else {
    problems.push(...checkImmutability(baseItems, items));
  }

  if (problems.length > 0) {
    for (const problem of problems) console.error(`INVALID: ${problem}`);
    console.error(`check-parity-manifest: ${problems.length} problem(s) in ${items.length} items`);
    process.exit(1);
  }
  const compared = baseItems === null ? 0 : baseItems.length;
  console.log(
    `check-parity-manifest: OK — ${items.length} items in ${(root.shards ?? []).length} shards; ` +
      `${compared} base items from ${baseLabel} kept their ids and acceptance text`,
  );
}

main();
