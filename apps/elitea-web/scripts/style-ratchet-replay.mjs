#!/usr/bin/env node
/**
 * The two-week replay of theme-gate check 10, the style ratchet (ADR-0062,
 * Rollout). For every PR merged into main in the window, it counts what
 * check 10 would have counted in CI and says whether a failure is a true
 * alarm (the PR wrote the values) or a false one.
 *
 *   node scripts/style-ratchet-replay.mjs --json <file> [--md <file>]
 *        [--days 14 | --since <date>] [--until origin/main]
 *
 * For each commit X on the first-parent line of --until that touches
 * apps/elitea-web/src: head = X, base = X^1, the base branch the PR landed on
 * (a squash merge's parent, or a merge commit's first parent), as CI takes
 * HEAD^1 of the merge commit GitHub builds. Everything that decides is the
 * check's own code: the tree is written by style-ratchet-base.mjs, linted
 * with style-ratchet.oxlintrc.json of this checkout, and counted, compared,
 * paired and printed by scripts/lib/style-ratchet-core.mjs.
 *
 * Each src/ tree is linted once, keyed by its git tree hash (the base of X is
 * usually the previous PR's head), the first one whole and each next one by
 * its changed files (`lintFrom`). The base reuses its parent's findings with
 * paths moved by `planBaseTree`, as the base tree CI writes holds them.
 *
 * True or false alarm, per grown folder and kind: the findings of the grown
 * files that sit on lines the PR added (`git diff -U0 -M X^1 X`, new side).
 * As many or more than the growth → true: the PR wrote them. Fewer → false:
 * the count grew by values the PR did not write.
 *
 * Unpaired moves met: each file the failure message names as new at the head
 * (decision 3.2). Only for this report, not for the check, git is asked
 * once more at a low similarity (-M20% -C20%) which base file the named
 * file is closest to, so a reader can tell a move with heavy edits from
 * new code.
 *
 * Read-only towards the repository; trees go to a temp folder.
 */
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import process from 'node:process';

import {
  KINDS,
  compareCounts,
  countFindings,
  formatGrowth,
  namedUnpaired,
  oxlintArgs,
  parseOxlintJson,
  planBaseTree,
  totals,
  unpairedFiles,
} from './lib/style-ratchet-core.mjs';

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const APP_DIR = resolve(SCRIPT_DIR, '..');
const CONFIG = join(SCRIPT_DIR, 'style-ratchet.oxlintrc.json');
const EXPORTER = join(SCRIPT_DIR, 'style-ratchet-base.mjs');
const OXLINT = join(APP_DIR, 'node_modules', '.bin', 'oxlint');
const LOW_SIMILARITY = '20%';

function parseArgs(argv) {
  const args = { days: '14', since: null, until: 'origin/main', json: null, md: null };
  for (let i = 0; i < argv.length; i += 2) {
    const name = argv[i]?.replace(/^--/, '');
    if (!(name in args) || !argv[i + 1]) {
      throw new Error(
        'usage: style-ratchet-replay.mjs --json <file> [--md <file>] [--days 14 | --since <date>] [--until <ref>]',
      );
    }
    args[name] = argv[i + 1];
  }
  if (!args.json) throw new Error('--json <file> is required');
  return args;
}

const { days, since: sinceArg, until, json, md } = parseArgs(process.argv.slice(2));
const startedAt = new Date();
const since =
  sinceArg ?? new Date(startedAt.getTime() - Number(days) * 86_400_000).toISOString().replace(/\.\d+Z$/, 'Z');

function run(cmd, args, options = {}) {
  const result = spawnSync(cmd, args, { cwd: APP_DIR, encoding: 'utf8', maxBuffer: 1 << 30, ...options });
  if (result.status !== 0 && !options.allowFail) {
    throw new Error(`${cmd} ${args.join(' ')} failed: ${String(result.stderr).trim()}`);
  }
  return result;
}
const TOP = run('git', ['rev-parse', '--show-toplevel']).stdout.trim();
// Paths in git calls are repository paths (apps/elitea-web/src), so git runs at the top.
// core.quotePath=false: non-ASCII paths come unquoted, as style-ratchet-base.mjs reads them.
const git = (args, options) => run('git', ['-c', 'core.quotePath=false', ...args], { cwd: TOP, ...options });
const PREFIX = `${relative(TOP, APP_DIR).split('\\').join('/')}/`;
const SRC = `${PREFIX}src`;

// A shallow clone drops the oldest PRs of the window without a word.
if (git(['rev-parse', '--is-shallow-repository']).stdout.trim() === 'true') {
  const dates = git(['log', '--first-parent', '--format=%cI', until]).stdout.trim().split('\n');
  const oldest = dates[dates.length - 1];
  if (new Date(oldest) >= new Date(since)) {
    throw new Error(
      `shallow clone: history starts ${oldest}, after ${since}; git fetch --shallow-since=<earlier> origin main`,
    );
  }
}

const tmp = mkdtempSync(join(tmpdir(), 'style-ratchet-replay-'));
const cache = new Map();
const stats = { fullLints: 0, filesLinted: 0 };
const treeKey = (sha) => git(['rev-parse', `${sha}:${SRC}`]).stdout.trim();
const lintDir = (dir) => parseOxlintJson(run(OXLINT, oxlintArgs(CONFIG), { cwd: dir, allowFail: true }).stdout);

/** src/ of one commit, written by the check's own style-ratchet-base.mjs, and linted. */
function fullLint(sha) {
  const dir = join(tmp, `full-${stats.fullLints}`);
  run(process.execPath, [EXPORTER, '--out', dir, '--base', sha, '--head', sha]);
  stats.fullLints += 1;
  const diags = lintDir(dir);
  rmSync(dir, { recursive: true, force: true });
  return diags;
}

/** Reads blobs `<sha>:<path>` in one `git cat-file --batch` call. */
function readBlobs(specs) {
  const out = spawnSync('git', ['cat-file', '--batch'], {
    cwd: TOP,
    input: specs.map((spec) => `${spec}\n`).join(''),
    maxBuffer: 1 << 30,
  }).stdout;
  const blobs = [];
  let at = 0;
  while (at < out.length) {
    const headerEnd = out.indexOf(0x0a, at);
    const size = Number(out.subarray(at, headerEnd).toString('utf8').split(' ')[2]);
    if (!Number.isFinite(size)) throw new Error(`git cat-file: ${out.subarray(at, headerEnd)}`);
    blobs.push(out.subarray(headerEnd + 1, headerEnd + 1 + size));
    at = headerEnd + 1 + size + 1;
  }
  return blobs;
}

/**
 * oxlint's diagnostics for src/ of `sha`, from those of `from`: the ratchet's
 * rules look at one file at a time, so only the files that differ are
 * linted again (in a tree that holds just them, at their paths). Writing
 * 8,000 files per commit is what made a full run slow; the result is
 * compared with a full lint of the last commit (`verified` in the output).
 */
function lintFrom(from, sha) {
  const changed = git(['diff', '--name-status', '--no-renames', from, sha, '--', SRC]).stdout;
  const gone = new Set();
  const write = [];
  for (const line of changed.split('\n').filter(Boolean)) {
    const [status, path] = line.split('\t');
    gone.add(path.slice(PREFIX.length));
    // Only files oxlint lints; a run over none prints no report (JSON, CSS and SVG have no findings).
    if (status !== 'D' && /\.[cm]?[jt]sx?$/.test(path)) write.push(path);
  }
  const kept = cache.get(treeKey(from)).filter((d) => !gone.has(String(d.filename)));
  if (write.length === 0) return kept;
  const dir = join(tmp, 'changed');
  rmSync(dir, { recursive: true, force: true });
  readBlobs(write.map((path) => `${sha}:${path}`)).forEach((blob, i) => {
    const target = join(dir, write[i].slice(PREFIX.length));
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, blob);
  });
  stats.filesLinted += write.length;
  return [...kept, ...lintDir(dir)];
}

/** Lints every commit in order: the first one whole, each next one from the one before. */
function lintAll(shas) {
  let previous = null;
  for (const sha of shas) {
    const key = treeKey(sha);
    if (!cache.has(key)) cache.set(key, previous ? lintFrom(previous, sha) : fullLint(sha));
    previous = sha;
  }
}

/** oxlint's diagnostics for src/ of one commit (lintAll ran first). */
const diagnostics = (sha) => cache.get(treeKey(sha));

/** A diagnostic as one comparable line. */
const asLine = (d) => `${d.filename}:${d.labels?.[0]?.span?.line ?? 0}:${d.code}:${d.message}`;

/** The PR's added lines per file (head path relative to the app), rename-aware. */
function addedLines(parent, sha) {
  const diff = git(['diff', '-U0', '-M', '--no-color', parent, sha, '--', SRC]).stdout;
  const lines = new Map();
  let current = null;
  for (const line of diff.split('\n')) {
    if (line.startsWith('+++ ')) {
      current = line.startsWith(`+++ b/${PREFIX}`) ? line.slice(`+++ b/${PREFIX}`.length) : null;
      if (current && !lines.has(current)) lines.set(current, new Set());
      continue;
    }
    const hunk = /^@@ -\S+ \+(\d+)(?:,(\d+))? @@/.exec(line);
    if (hunk && current) {
      const start = Number(hunk[1]);
      const count = hunk[2] === undefined ? 1 : Number(hunk[2]);
      for (let i = 0; i < count; i += 1) lines.get(current).add(start + i);
    }
  }
  return lines;
}

/** For the report only: the base file git finds closest to each named file at a low similarity. */
function closestSources(parent, sha, paths) {
  const status = git([
    'diff',
    '--name-status',
    `-M${LOW_SIMILARITY}`,
    `-C${LOW_SIMILARITY}`,
    parent,
    sha,
    '--',
    SRC,
  ]).stdout;
  const wanted = new Set(paths.map((path) => `${PREFIX}${path}`));
  const found = new Map();
  for (const line of status.split('\n').filter(Boolean)) {
    const [code, from, to] = line.split('\t');
    if (to && wanted.has(to) && (code[0] === 'R' || code[0] === 'C')) {
      found.set(to.slice(PREFIX.length), {
        source: from.slice(PREFIX.length),
        similarity: Number(code.slice(1)),
        as: code[0],
      });
    }
  }
  return found;
}

const merges = git(['log', '--first-parent', `--since=${since}`, '--format=%H%x09%cI%x09%s', until, '--', SRC])
  .stdout.trim()
  .split('\n')
  .filter(Boolean)
  .reverse();

const results = [];
let verified = null;
try {
  lintAll(
    merges.flatMap((line) => {
      const sha = line.split('\t')[0];
      return [`${sha}^1`, sha].map((ref) => git(['rev-parse', ref]).stdout.trim());
    }),
  );
  console.error(`linted ${stats.fullLints} tree(s) whole and ${stats.filesLinted} changed file(s)`);
  for (const [index, line] of merges.entries()) {
    const [sha, date, subject] = line.split('\t');
    const parent = git(['rev-parse', `${sha}^1`]).stdout.trim();
    const head = countFindings(diagnostics(sha));

    const diff = git(['diff', '--name-status', '-M', parent, sha, '--', SRC]);
    const nameStatus = diff.stdout;
    const baseFiles = git(['ls-tree', '-r', '--name-only', parent, '--', SRC]).stdout.split('\n').filter(Boolean);
    const moved = new Map(
      planBaseTree(baseFiles, nameStatus, PREFIX).map(({ from, to }) => [from.slice(PREFIX.length), to]),
    );
    const base = countFindings(
      diagnostics(parent)
        .filter((d) => moved.has(String(d.filename)))
        .map((d) => ({ ...d, filename: moved.get(String(d.filename)) })),
    );
    const unpaired = unpairedFiles(nameStatus, PREFIX);
    const growth = compareCounts(base, head);
    const before = totals(base);
    const after = totals(head);
    const delta = Object.fromEntries(KINDS.map((kind) => [kind, after[kind] - before[kind]]));

    const added = growth.length > 0 ? addedLines(parent, sha) : new Map();
    const rows = growth.map(({ folder, kind, before: b, after: a, files }) => {
      const onAdded = files.reduce(
        (sum, { path, hits }) => sum + hits.filter((hit) => added.get(path)?.has(hit.line)).length,
        0,
      );
      return {
        folder,
        kind,
        before: b,
        after: a,
        grew: a - b,
        onAdded,
        alarm: onAdded >= a - b ? 'true' : 'false',
        netInPr: delta[kind],
        files: files.map(({ path, before: fb, hits }) => ({ path, before: fb, after: hits.length, hits })),
      };
    });
    const named = namedUnpaired(growth, unpaired);
    const sources =
      named.length > 0
        ? closestSources(
            parent,
            sha,
            named.map(({ path }) => path),
          )
        : new Map();

    results.push({
      pr: (/\(#(\d+)\)\s*$/.exec(subject) ?? [])[1] ?? null,
      sha: sha.slice(0, 10),
      parent: parent.slice(0, 10),
      date,
      subject,
      fails: growth.length > 0,
      delta,
      unpaired: { added: unpaired.added.length, removed: unpaired.removed.length },
      renameWarning: diff.stderr.trim() || null,
      growth: rows,
      named: named.map((entry) => ({ ...entry, closest: sources.get(entry.path) ?? null })),
      message: formatGrowth(growth, unpaired),
    });
    const verdict = growth.length
      ? `FAIL ${rows.map((r) => `${r.folder} ${r.kind} ${r.before}→${r.after} ${r.alarm}`).join('; ')}`
      : 'pass';
    console.error(`${index + 1}/${merges.length} ${sha.slice(0, 8)} #${results.at(-1).pr} ${verdict}`);
  }
  // The incremental lint must give what a whole lint gives: compare on the last commit.
  if (merges.length > 0) {
    const last = merges.at(-1).split('\t')[0];
    const whole = fullLint(last).map(asLine).sort();
    const composed = diagnostics(last).map(asLine).sort();
    verified = {
      commit: last.slice(0, 10),
      equal: JSON.stringify(whole) === JSON.stringify(composed),
      findings: whole.length,
    };
    if (!verified.equal) console.error(`incremental lint differs from a whole lint on ${last.slice(0, 10)}`);
  }
} finally {
  rmSync(tmp, { recursive: true, force: true });
}

const runInfo = {
  commit: git(['rev-parse', 'HEAD']).stdout.trim(),
  branch: git(['rev-parse', '--abbrev-ref', 'HEAD']).stdout.trim(),
  dirty: git(['status', '--porcelain', '--', `${PREFIX}scripts`, `${PREFIX}tools/lint-rules`]).stdout.trim() !== '',
  startedAt: startedAt.toISOString(),
  finishedAt: new Date().toISOString(),
  since,
  until,
  untilSha: git(['rev-parse', until]).stdout.trim(),
  config: relative(APP_DIR, CONFIG),
  wholeTreeLints: stats.fullLints,
  changedFilesLinted: stats.filesLinted,
  verified,
};
const failing = results.filter((r) => r.fails);
const rows = failing.flatMap((r) => r.growth.map((g) => ({ ...g, pr: r.pr, sha: r.sha })));
const summary = {
  prs: results.length,
  failing: failing.length,
  passing: results.length - failing.length,
  rows: rows.length,
  trueAlarms: rows.filter((r) => r.alarm === 'true').length,
  falseAlarms: rows.filter((r) => r.alarm === 'false').length,
  prsWithUnpairedNamed: failing.filter((r) => r.named.length > 0).length,
  namedUnpairedFiles: failing.reduce((sum, r) => sum + r.named.length, 0),
  namedWithBaseFileAt20: failing.reduce((sum, r) => sum + r.named.filter((n) => n.closest).length, 0),
  renameWarnings: results.filter((r) => r.renameWarning).length,
  failingPrsByKind: Object.fromEntries(
    KINDS.map((k) => [k, failing.filter((r) => r.growth.some((g) => g.kind === k)).length]),
  ),
  netByKind: Object.fromEntries(KINDS.map((k) => [k, results.reduce((sum, r) => sum + r.delta[k], 0)])),
};
writeFileSync(json, `${JSON.stringify({ run: runInfo, summary, results }, null, 1)}\n`);

if (md) {
  const pr = (r) => (r.pr ? `#${r.pr}` : r.sha);
  const out = [
    `Run on \`${runInfo.branch}\` at \`${runInfo.commit.slice(0, 10)}\`${runInfo.dirty ? ' (with uncommitted changes)' : ''}, ${runInfo.startedAt}.`,
    `Lint: ${runInfo.wholeTreeLints} whole tree(s) and ${runInfo.changedFilesLinted} changed file(s); equal to a whole lint on \`${verified?.commit}\`: ${verified?.equal} (${verified?.findings} findings).`,
    `Window: merges into \`${until}\` since ${since}, up to \`${runInfo.untilSha.slice(0, 10)}\`; base = each merge's first parent.`,
    '',
    `**${summary.failing} of ${summary.prs} PRs would have failed; ${summary.falseAlarms} false alarm(s) in ${summary.rows} grown folder counts.**`,
    '',
    '| PR | Folder | Kind | Before → after | Alarm | New files the message names |',
    '| --- | --- | --- | --- | --- | --- |',
  ];
  for (const r of failing) {
    for (const g of r.growth) {
      const named = r.named.filter((n) => n.folder === g.folder && n.kind === g.kind);
      const cell = named.length
        ? named.map((n) => `\`${n.path.replace(`${g.folder}/`, '')}\` (${n.count})`).join(', ')
        : '—';
      out.push(`| ${pr(r)} | \`${g.folder}\` | ${g.kind} | ${g.before} → ${g.after} | ${g.alarm} | ${cell} |`);
    }
  }
  const named = failing.flatMap((r) => r.named.map((n) => ({ ...n, pr: pr(r) })));
  const met = named.filter((n) => n.closest);
  out.push(
    '',
    `**Unpaired moves met.** The message named ${named.length} new file(s) in failing folders; for ${named.length - met.length}` +
      ' of them git finds no base file at 20 % similarity (new code). The ones it does find, for this report only' +
      ' (-M20% -C20%; the check pairs at 50 % only):',
    '',
  );
  if (met.length === 0) out.push('None.');
  for (const n of met) {
    out.push(
      `- ${n.pr} \`${n.path}\` (${n.kind} ${n.count}): closest base file \`${n.closest.source}\` at ${n.closest.similarity} % (${n.closest.as === 'R' ? 'a deleted file' : 'a file the PR kept'})`,
    );
  }
  const warned = results.filter((r) => r.renameWarning);
  if (warned.length) out.push('', `git skipped inexact rename detection in: ${warned.map(pr).join(', ')}.`);
  writeFileSync(md, `${out.join('\n')}\n`);
}
console.error(
  `wrote ${json}: ${summary.prs} PRs, ${summary.failing} would fail, ${summary.falseAlarms} false alarm(s), ` +
    `${summary.namedUnpairedFiles} unpaired file(s) named; incremental lint equal to a whole lint: ${verified?.equal}`,
);
