#!/usr/bin/env node
/**
 * Writes the base tree for theme-gate check 10, the style ratchet (ADR-0062
 * decision 3.2): `src/` of the base commit, into a folder theme-gate reads
 * with `--base <dir>`.
 *
 *   node scripts/style-ratchet-base.mjs --out <dir> [--base <ref>] [--head <ref>]
 *
 * --base defaults to HEAD^1. On a pull request the checkout is the merge
 * commit GitHub builds (refs/pull/N/merge), so its first parent is the base
 * branch the PR lands on, and HEAD^1..HEAD is exactly what the PR adds. The
 * gates job checks out with depth 1, so the parent is fetched when missing.
 *
 * A file the PR renames is written under its HEAD path (git rename
 * detection), so a move between two folders does not make the target folder
 * grow. The file plan is `planBaseTree` in scripts/lib/style-ratchet-core.mjs.
 *
 * Next to `src/` it writes `style-ratchet-unpaired.json`: the files git did
 * not pair (`unpairedFiles`, decision 3.2). theme-gate needs it with --base,
 * so that a failing folder names its new files and the two fixes.
 */
import { spawnSync } from 'node:child_process';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

import { UNPAIRED_MANIFEST, planBaseTree, unpairedFiles } from './lib/style-ratchet-core.mjs';

const APP_DIR = resolve(dirname(fileURLToPath(import.meta.url)), '..');

function git(args, options = {}) {
  const run = spawnSync('git', args, { cwd: APP_DIR, maxBuffer: 1 << 30, ...options });
  if (run.status !== 0) {
    throw new Error(`git ${args.join(' ')} failed: ${String(run.stderr).trim()}`);
  }
  return run.stdout;
}

const hasCommit = (ref) =>
  spawnSync('git', ['rev-parse', '--verify', '--quiet', `${ref}^{commit}`], { cwd: APP_DIR }).status === 0;

function parseArgs(argv) {
  const args = { base: 'HEAD^1', head: 'HEAD', out: null };
  for (let i = 0; i < argv.length; i += 2) {
    const name = argv[i];
    const value = argv[i + 1];
    if (!['--out', '--base', '--head'].includes(name) || !value) {
      throw new Error(`usage: style-ratchet-base.mjs --out <dir> [--base <ref>] [--head <ref>] (bad: ${name})`);
    }
    args[name.slice(2)] = value;
  }
  if (!args.out) throw new Error('--out <dir> is required');
  return args;
}

/** Reads many blobs in one `git cat-file --batch` call. */
function readBlobs(specs) {
  const out = git(['cat-file', '--batch'], { input: specs.map((s) => `${s}\n`).join('') });
  const blobs = [];
  let at = 0;
  while (at < out.length) {
    const headerEnd = out.indexOf(0x0a, at);
    const header = out.subarray(at, headerEnd).toString('utf8');
    const size = Number(header.split(' ')[2]);
    if (!Number.isFinite(size)) throw new Error(`git cat-file: ${header}`);
    blobs.push(out.subarray(headerEnd + 1, headerEnd + 1 + size));
    at = headerEnd + 1 + size + 1;
  }
  return blobs;
}

function main() {
  const { base, head, out } = parseArgs(process.argv.slice(2));
  const headSha = String(git(['rev-parse', head])).trim();
  if (!hasCommit(base)) {
    // Depth-1 checkout: fetch the head with its parent.
    git(['fetch', '--no-tags', '--depth=2', 'origin', headSha]);
  }
  const baseSha = String(git(['rev-parse', base])).trim();
  const top = String(git(['rev-parse', '--show-toplevel'])).trim();
  const prefix = `${relative(top, APP_DIR).split('\\').join('/')}/`;
  const src = `${prefix}src`;

  const baseFiles = String(git(['ls-tree', '-r', '--name-only', baseSha, '--', src], { cwd: top }))
    .split('\n')
    .filter(Boolean);
  const diff = spawnSync('git', ['diff', '--name-status', '-M', baseSha, headSha, '--', src], {
    cwd: top,
    encoding: 'utf8',
    maxBuffer: 1 << 30,
  });
  if (diff.status !== 0) throw new Error(`git diff --name-status failed: ${diff.stderr.trim()}`);
  // Past diff.renameLimit git skips inexact rename detection and says so on
  // stderr; every move it did not pair then counts as new (decision 3.2).
  if (diff.stderr.trim()) console.warn(`style-ratchet-base: git: ${diff.stderr.trim()}`);
  const nameStatus = diff.stdout;
  const plan = planBaseTree(baseFiles, nameStatus, prefix);
  const unpaired = unpairedFiles(nameStatus, prefix);
  const blobs = readBlobs(plan.map(({ from }) => `${baseSha}:${from}`));

  const outDir = resolve(out);
  rmSync(join(outDir, 'src'), { recursive: true, force: true });
  plan.forEach(({ to }, i) => {
    const target = join(outDir, to);
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, blobs[i]);
  });
  mkdirSync(outDir, { recursive: true });
  writeFileSync(
    join(outDir, UNPAIRED_MANIFEST),
    `${JSON.stringify({ base: baseSha, head: headSha, ...unpaired }, null, 2)}\n`,
  );
  const renamed = plan.filter(({ from, to }) => from !== `${prefix}${to}`).length;
  console.log(
    `style-ratchet-base: ${plan.length} files of ${baseSha.slice(0, 10)} in ${outDir} (${renamed} under their new path;` +
      ` git paired no base file with ${unpaired.added.length} head file(s) and no head file with ${unpaired.removed.length} base file(s))`,
  );
}

main();
