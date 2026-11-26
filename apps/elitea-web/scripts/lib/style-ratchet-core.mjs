/**
 * theme-gate check 10 — the style ratchet (ADR-0062 decision 3). Pure logic:
 * scripts/theme-gate.mjs runs oxlint with scripts/style-ratchet.oxlintrc.json
 * over the PR head and over its base tree, and hands both JSON reports here.
 *
 * Counts are per folder (`src/<layer>/<slice>`) and per kind. The check fails
 * when a count in a folder is higher on the head than on the base; lower or
 * equal passes, and a new folder starts at 0. The base branch is the ceiling,
 * so there is no ceilings file to edit or to conflict on.
 *
 * scripts/style-ratchet-base.mjs builds the base tree. Its file list comes
 * from `planBaseTree` below: a file the PR renames is written under its NEW
 * path, so a move between two folders does not make the target folder grow.
 * Next to the tree it writes the files git did not pair (`unpairedFiles`),
 * and a failing folder names the ones that carry counted values.
 * scripts/style-ratchet-replay.mjs replays this core over merged PRs.
 */

/** Written by scripts/style-ratchet-base.mjs next to the base tree's src/: `{ added, removed }` from `unpairedFiles`. */
export const UNPAIRED_MANIFEST = 'style-ratchet-unpaired.json';

/**
 * Reads that manifest's text. Throws when it is not `{ added: [], removed: [] }`
 * of strings: a base tree without it would drop the names from the message.
 */
export function parseUnpairedManifest(text) {
  let manifest;
  try {
    manifest = JSON.parse(text);
  } catch {
    throw new Error(`${UNPAIRED_MANIFEST} is not JSON`);
  }
  const list = (value) => Array.isArray(value) && value.every((path) => typeof path === 'string');
  if (!manifest || !list(manifest.added) || !list(manifest.removed)) {
    throw new Error(`${UNPAIRED_MANIFEST} needs "added" and "removed" lists of paths`);
  }
  return { added: manifest.added, removed: manifest.removed };
}

/** oxlint's arguments for one tree (cwd = the tree), as theme-gate and the replay run it. */
export function oxlintArgs(config) {
  return ['-c', config, '-f', 'json', '--no-ignore', 'src'];
}

/** Kinds in stage 1 (ADR-0062 decision 3.4). A message names its kind as `[kind]`. */
export const KINDS = ['spacing-rem', 'spacing-px', 'spacing-off-scale', 'mui-dialog', 'mui-switch'];

const KIND_RE = /\[([a-z][a-z-]*)\]/;

/**
 * Files the ratchet does not count: tests, stories, mocks, type declarations
 * and the ported source of @eliteaai/elitea-assistant (decision 3.6). This
 * filter is here and not in the oxlint config because `ignorePatterns` there
 * resolve against the config's own folder and match nothing in a tree that
 * lives elsewhere (the base tree, the self-test's fixtures).
 */
const NOT_COUNTED = [
  /\.(test|spec)\.[cm]?[jt]sx?$/,
  /\.stories\.[cm]?[jt]sx?$/,
  /\.d\.ts$/,
  /(^|\/)(__tests__|__mocks__|test|smoke)\//,
  /(^|\/)widgets\/support-assistant\/vendor\//,
];

/** True when a path (relative to the tree root, `src/...`) is counted. */
export function isCounted(path) {
  return path.startsWith('src/') && !NOT_COUNTED.some((re) => re.test(path));
}

/** `src/<layer>/<slice>` for a file; a file right under a layer counts to the layer. */
export function folderOf(path) {
  const parts = path.split('/');
  if (parts.length <= 2) return 'src';
  if (parts.length === 3) return parts.slice(0, 2).join('/');
  return parts.slice(0, 3).join('/');
}

/** The ratchet kind of one oxlint diagnostic, or null when it carries none. */
export function kindOf(diagnostic) {
  const match = KIND_RE.exec(`${diagnostic.message ?? ''} ${diagnostic.help ?? ''}`);
  return match && KINDS.includes(match[1]) ? match[1] : null;
}

/**
 * Parses oxlint's `-f json` output. Throws when it is not a report: an oxlint
 * that crashed or refused its config must not read as "0 findings".
 */
export function parseOxlintJson(stdout) {
  let report;
  try {
    report = JSON.parse(stdout);
  } catch {
    throw new Error(`oxlint did not print a JSON report: ${String(stdout).slice(0, 200)}`);
  }
  if (!report || !Array.isArray(report.diagnostics)) {
    throw new Error('oxlint JSON report has no diagnostics array');
  }
  return report.diagnostics;
}

/**
 * Findings by folder, kind and file. A diagnostic with no kind is an error:
 * the ratchet config holds only ratchet rules, so an untagged message means
 * the config or a rule changed shape and the counts would be wrong.
 */
export function countFindings(diagnostics) {
  const folders = new Map();
  for (const diagnostic of diagnostics) {
    const path = String(diagnostic.filename).split('\\').join('/');
    if (!isCounted(path)) continue;
    const kind = kindOf(diagnostic);
    if (!kind) throw new Error(`untagged finding in ${path}: ${diagnostic.code} ${diagnostic.message}`);
    const folder = folderOf(path);
    if (!folders.has(folder)) folders.set(folder, new Map());
    const kinds = folders.get(folder);
    if (!kinds.has(kind)) kinds.set(kind, { count: 0, files: new Map() });
    const entry = kinds.get(kind);
    entry.count += 1;
    const line = diagnostic.labels?.[0]?.span?.line ?? 0;
    if (!entry.files.has(path)) entry.files.set(path, []);
    // oxlint puts a configured message (no-restricted-imports) in `help`; it names the fix.
    const message = diagnostic.help ? `${diagnostic.message} ${diagnostic.help}` : diagnostic.message;
    entry.files.get(path).push({ line, message });
  }
  return folders;
}

const countIn = (counts, folder, kind) => counts.get(folder)?.get(kind)?.count ?? 0;

/**
 * Every folder and kind whose count grew from base to head, with the files
 * whose own count grew (where the new values most likely are).
 */
export function compareCounts(base, head) {
  const growth = [];
  for (const [folder, kinds] of [...head].sort(([a], [b]) => a.localeCompare(b))) {
    for (const kind of KINDS) {
      const headEntry = kinds.get(kind);
      if (!headEntry) continue;
      const before = countIn(base, folder, kind);
      if (headEntry.count <= before) continue;
      const baseFiles = base.get(folder)?.get(kind)?.files ?? new Map();
      const files = [...headEntry.files]
        .filter(([path, hits]) => hits.length > (baseFiles.get(path)?.length ?? 0))
        .map(([path, hits]) => ({ path, before: baseFiles.get(path)?.length ?? 0, hits }));
      growth.push({ folder, kind, before, after: headEntry.count, files });
    }
  }
  return growth;
}

/** Totals per kind over all folders. */
export function totals(counts) {
  const sums = Object.fromEntries(KINDS.map((kind) => [kind, 0]));
  for (const kinds of counts.values()) {
    for (const [kind, entry] of kinds) sums[kind] += entry.count;
  }
  return sums;
}

/** The note on a grown file that git paired with no base file (decision 3.2). */
export const UNPAIRED_NOTE = 'new at the head: git paired it with no base file';

/** The two fixes for a grown folder that holds such files (decision 3.2). */
export const UNPAIRED_FIXES = [
  'A file marked "new at the head" counts as new in this folder (git pairs a move at 50 % similarity or more).',
  'If it was moved, split or merged from another file with edits, either:',
  '  1. land the move unchanged in its own PR first (a pure move pairs at 100 %), then edit it; or',
  '  2. move the values it carries onto the system in this PR.',
];

/**
 * Lines for a failing run: each grown folder, then the findings in its grown
 * files. `unpaired.added` (from `unpairedFiles`) marks the grown files git
 * paired with no base file, and the folder then gets the two fixes.
 */
export function formatGrowth(growth, unpaired = { added: [] }) {
  const added = new Set(unpaired.added);
  const lines = [];
  for (const { folder, kind, before, after, files } of growth) {
    lines.push(`${folder} ${kind}: ${before} → ${after} (the base branch is the ceiling; remove as many as you add)`);
    for (const { path, before: fileBefore, hits } of files) {
      const note = added.has(path) ? `; ${UNPAIRED_NOTE}` : '';
      lines.push(`  ${path} (${fileBefore} → ${hits.length} in this file${note})`);
      for (const hit of hits) lines.push(`    ${path}:${hit.line} ${hit.message}`);
    }
    if (files.some(({ path }) => added.has(path))) {
      for (const line of UNPAIRED_FIXES) lines.push(`  ${line}`);
    }
  }
  return lines;
}

/** The grown files of a failing run that git paired with no base file: the ones the message names. */
export function namedUnpaired(growth, unpaired) {
  const added = new Set(unpaired.added);
  return growth.flatMap(({ folder, kind, files }) =>
    files.filter(({ path }) => added.has(path)).map(({ path, hits }) => ({ folder, kind, path, count: hits.length })),
  );
}

/** Lines for a run with no base (nightly, dispatch): counts per folder, kinds in KINDS order. */
export function formatCounts(counts) {
  const lines = [];
  for (const [folder, kinds] of [...counts].sort(([a], [b]) => a.localeCompare(b))) {
    const parts = KINDS.filter((kind) => kinds.has(kind)).map((kind) => `${kind} ${kinds.get(kind).count}`);
    lines.push(`${folder}: ${parts.join(', ')}`);
  }
  const sums = totals(counts);
  lines.push(`total: ${KINDS.map((kind) => `${kind} ${sums[kind]}`).join(', ')}`);
  return lines;
}

/**
 * The base tree's file list, from `git diff --name-status -M <base> <head> -- <prefix>`
 * and `git ls-tree -r --name-only <base> -- <prefix>`: each base file and the
 * path it is written to. A renamed file goes to its head path (decision 3.2).
 * Only files under `<prefix>src/` are kept; paths are returned relative to `prefix`.
 */
export function planBaseTree(baseFiles, nameStatus, prefix) {
  const renames = new Map(
    parseNameStatus(nameStatus)
      .filter(({ status }) => status === 'R')
      .map(({ from, to }) => [from, to]),
  );
  const tree = `${prefix}src/`;
  return (
    baseFiles
      .filter((path) => path.startsWith(tree))
      .map((path) => ({ from: path, to: renames.get(path) ?? path }))
      // A file the PR moves out of src/ is not in the head's count either.
      .filter(({ to }) => to.startsWith(tree))
      .map(({ from, to }) => ({ from, to: to.slice(prefix.length) }))
  );
}

/**
 * `git diff --name-status -M` lines as { status, from, to }: status is the
 * letter (R087 → R), `from` the base path, `to` the head path (equal unless
 * git paired a rename or a copy).
 */
export function parseNameStatus(nameStatus) {
  return nameStatus
    .split('\n')
    .filter(Boolean)
    .map((line) => {
      const [code, from, to = from] = line.split('\t');
      return { status: code[0], from, to };
    });
}

/**
 * The files git did not pair between the base and the head (decision 3.2),
 * from the same `git diff --name-status -M <base> <head>` as `planBaseTree`.
 *
 * There is no pairing here but git's. Rename detection at git's default 50 %
 * similarity pairs each head file with at most one base file; such a pair is
 * an `R` line, and the base tree holds the file under its head path. A file
 * git lists as added (`A`) has no base file: it counts as new in its folder.
 * A file git lists as deleted (`D`) has no head file: it counts as removed
 * from its folder. A file git pairs across the edge of `src/` is new to the
 * counted tree on one side or removed from it on the other. Nothing else
 * reads names, sizes or contents to guess a move: a move with heavy edits, a
 * split or a merge is whatever git makes of it (a split pairs at most one
 * part, a merge at most one source), and the failure message names the new
 * files with the two fixes (`formatGrowth`).
 *
 * Returns paths relative to `prefix`, under `src/`, sorted.
 */
export function unpairedFiles(nameStatus, prefix) {
  const tree = `${prefix}src/`;
  const inTree = (path) => path.startsWith(tree);
  const added = [];
  const removed = [];
  for (const { status, from, to } of parseNameStatus(nameStatus)) {
    if (status === 'A' || (status === 'R' && inTree(to) && !inTree(from))) added.push(to);
    if (status === 'D' || (status === 'R' && inTree(from) && !inTree(to))) removed.push(from);
  }
  const rel = (paths) =>
    paths
      .filter(inTree)
      .map((path) => path.slice(prefix.length))
      .sort();
  return { added: rel(added), removed: rel(removed) };
}
