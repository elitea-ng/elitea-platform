#!/usr/bin/env node
// The sunset check of ADR-0059 decision 2 (story R0.14b, #2590).
//
// The legacy administration items end on a DATE, not at a version: releases
// are cut by semantic-release, so a major can arrive any week. A date binds
// only when CI enforces it, in both directions: the removal must not merge
// early, and it must not be forgotten.
//
// It reads, all from the commit under test (never the working tree, never the
// wall clock, so a rebuild of an old tag gives the answer it gave the day the
// tag was cut):
//
//   * the two constants ReplacementShippedOn and SunsetAfter of
//     services/elitea-main/internal/adminaccess/registry.go;
//   * whether each legacy item and each replacement is in the tree (the
//     detection tables below);
//   * the commit's COMMITTER date (UTC day);
//   * the stable release tags (vX.Y.Z, no pre-release) reachable from it.
//
// It fails in four cases:
//
//   1. ReplacementShippedOn is set and SunsetAfter is empty or less than
//      MIN_NOTICE_DAYS later (or SunsetAfter is set alone).
//   2. Both replacements are in the code and ReplacementShippedOn is empty.
//   3. The legacy items are gone, and the commit is dated before SunsetAfter
//      (or no date is set), or fewer than MIN_STABLE_MINORS stable minor
//      releases follow the release that shipped the replacements.
//   4. A legacy item is still there and the commit is dated more than
//      GRACE_DAYS after SunsetAfter. From then on the removal is the only
//      change that passes; .github/workflows/sunset-reminder.yml opens an
//      issue 30 days before the date so nobody meets this by surprise.
//
// A commit whose tree has no registry.go predates the rule (story R0.1a) and
// passes with nothing checked.
//
//   node scripts/ci/check-sunset.mjs [--sha <rev>] [--root <repo>]

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

export const REGISTRY = 'services/elitea-main/internal/adminaccess/registry.go';
const ADMINACCESS = 'services/elitea-main/internal/adminaccess';
const LEGACYRBAC = 'services/elitea-main/internal/infra/legacyrbac';

export const MIN_NOTICE_DAYS = 90;
export const GRACE_DAYS = 14;
export const MIN_STABLE_MINORS = 2;

// ── Detection tables ─────────────────────────────────────────────────────────
// Each row is found by a regular expression over the non-test Go files of one
// package at the commit under test. An item is "there" when its pattern
// matches. A pattern names the declaration (the constant and its value), so a
// comment that still quotes a removed name does not keep it "there".
//
// `landed` says the owning story has merged: a landed legacy item that is not
// found has been REMOVED, and the removal is held to the date (case 3). A row
// whose story has not landed yet finds nothing, and that absence means
// nothing: it neither removes a legacy item nor counts as a replacement. The
// story that adds the marker sets `landed: true` in the same pull request.
// While SunsetAfter is empty the test requires every landed row to match the
// real tree (check-sunset.test.mjs), so a rename cannot read as a removal.
//
// The removal is ONE pull request (R4.2, R4.7, R4.8 on one branch, ADR-0059):
// removing one item early fails case 3, and keeping one past the grace fails
// case 4.
//
// Patterns are POSIX extended regular expressions (git grep -E): write
// [[:space:]], not \s, which the macOS regex library does not know.

/** The legacy items: all end on SunsetAfter. */
export const LEGACY_ITEMS = [
  { id: 'runtime.plugins', story: 'R0.1a (#2490)', dir: ADMINACCESS, pattern: String.raw`RuntimePlugins[[:space:]]*=[[:space:]]*"runtime\.plugins"`, landed: true },
  { id: 'configuration.governance', story: 'R0.1a (#2490)', dir: ADMINACCESS, pattern: String.raw`ConfigurationGovernance[[:space:]]*=[[:space:]]*"configuration\.governance"`, landed: true },
  { id: 'inherit token administration access', story: 'R1.1 (#2505)', dir: LEGACYRBAC, pattern: String.raw`TokenAdminAccessInherit[[:space:]]*=[[:space:]]*"inherit"`, landed: true },
  // R3.8a (#2539), internal/adminaccess/reason.go: true while a request without a reason
  // is served with Elitea-Reason-Missing.
  { id: 'reason warning period', story: 'R3.8 (#2539)', dir: ADMINACCESS, pattern: String.raw`ReasonOptionalUntilSunset[[:space:]]*=[[:space:]]*true`, landed: true },
];

/** The replacements: when both are in the code, the date must be set. */
export const REPLACEMENTS = [
  // R2.7 (#2521) owes this constant: the pattern of the sealed sign-in route.
  { id: 'sealed sign-in route', story: 'R2.7 (#2521)', dir: ADMINACCESS, pattern: String.raw`SealedSignInRoute[[:space:]]*=[[:space:]]*"`, landed: false },
  // R3.8a (#2539), internal/adminaccess/reason.go: the header a request without a reason gets.
  { id: 'reason handling', story: 'R3.8 (#2539)', dir: ADMINACCESS, pattern: String.raw`ReasonMissingHeader[[:space:]]*=[[:space:]]*"Elitea-Reason-Missing"`, landed: true },
];

// ── Dates (no clock) ─────────────────────────────────────────────────────────

const DAY_MS = 86_400_000;
const ISO_DAY = /^(\d{4})-(\d{2})-(\d{2})$/;

/** Days since the epoch of a YYYY-MM-DD, or NaN when it is not a real date. */
export function dayNumber(text) {
  const m = ISO_DAY.exec(text);
  if (!m) return Number.NaN;
  const ms = Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
  const back = new Date(ms).toISOString().slice(0, 10);
  return back === text ? ms / DAY_MS : Number.NaN;
}

/** The UTC day of an ISO 8601 timestamp with offset (git %cI). */
export function utcDay(isoTimestamp) {
  const ms = Date.parse(isoTimestamp);
  if (Number.isNaN(ms)) throw new Error(`not a timestamp: ${isoTimestamp}`);
  return new Date(ms).toISOString().slice(0, 10);
}

const STABLE = /^v(\d+)\.(\d+)\.(\d+)$/;

/** [major, minor, patch] of a stable tag, or null for anything else. */
export function stableVersion(tag) {
  const m = STABLE.exec(tag);
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
}

const byVersion = (a, b) => a[0] - b[0] || a[1] - b[1] || a[2] - b[2];

/** The constants of registry.go; throws when the file lost them. */
export function parseConstants(source) {
  const read = (name) => {
    const m = new RegExp(String.raw`\b${name}\s*=\s*"([^"]*)"`).exec(source);
    if (!m) throw new Error(`${REGISTRY} does not declare ${name} = "..."; the sunset check reads it`);
    return m[1];
  };
  return { replacementShippedOn: read('ReplacementShippedOn'), sunsetAfter: read('SunsetAfter') };
}

/**
 * The verdict. `state` is what the commit holds:
 *   { replacementShippedOn, sunsetAfter, commitDay,
 *     legacyPresent: [ids found], legacyRemoved: [landed ids not found],
 *     replacementsPresent: [ids],
 *     minorsAfterReplacement: number | null (null: no stable tag ships both) }
 * It answers the list of failures; empty passes.
 */
export function decide(state) {
  const problems = [];
  const { replacementShippedOn: shipped, sunsetAfter: sunset, commitDay } = state;
  const shippedDay = shipped ? dayNumber(shipped) : null;
  const sunsetDay = sunset ? dayNumber(sunset) : null;
  const today = dayNumber(commitDay);
  if (Number.isNaN(shippedDay)) problems.push(`ReplacementShippedOn "${shipped}" is not a YYYY-MM-DD date`);
  if (Number.isNaN(sunsetDay)) problems.push(`SunsetAfter "${sunset}" is not a YYYY-MM-DD date`);
  if (problems.length) return problems;

  // Case 1.
  if (shipped && !sunset) problems.push(`case 1: ReplacementShippedOn is ${shipped} and SunsetAfter is empty; set it at least ${MIN_NOTICE_DAYS} days later`);
  if (shipped && sunset && sunsetDay - shippedDay < MIN_NOTICE_DAYS) {
    problems.push(`case 1: SunsetAfter ${sunset} is ${sunsetDay - shippedDay} days after ReplacementShippedOn ${shipped}; the notice is at least ${MIN_NOTICE_DAYS} days`);
  }
  if (!shipped && sunset) problems.push(`case 1: SunsetAfter is ${sunset} and ReplacementShippedOn is empty; story R3.15 sets both in one commit`);

  // Case 2.
  if (!shipped && state.replacementsPresent.length === REPLACEMENTS.length) {
    problems.push(`case 2: both replacements are in the code (${state.replacementsPresent.join(', ')}) and ReplacementShippedOn is empty; story R3.15 sets the two dates`);
  }

  // Case 3: any landed item removed is a removal, held to the date.
  const removed = state.legacyRemoved;
  if (removed.length > 0) {
    const what = `the legacy item(s) ${removed.join(', ')} are gone`;
    if (!sunset) {
      problems.push(`case 3: ${what} and SunsetAfter is empty; the removal waits for the date`);
    } else if (today < sunsetDay) {
      problems.push(`case 3: ${what} and the commit is dated ${commitDay}, before SunsetAfter ${sunset}`);
    }
    if (state.minorsAfterReplacement === null) {
      problems.push(`case 3: ${what} and no stable release reachable from this commit ships both replacements`);
    } else if (state.minorsAfterReplacement < MIN_STABLE_MINORS) {
      problems.push(`case 3: ${what} and ${state.minorsAfterReplacement} stable minor release(s) follow the release that shipped the replacements; at least ${MIN_STABLE_MINORS} must`);
    }
  }

  // Case 4.
  if (state.legacyPresent.length > 0 && sunset && today - sunsetDay > GRACE_DAYS) {
    problems.push(`case 4: the commit is dated ${commitDay}, ${today - sunsetDay} days after SunsetAfter ${sunset}, and still holds: ${state.legacyPresent.join(', ')}; from day ${GRACE_DAYS + 1} only the removal merges, one pull request that removes every legacy item (R4.2, R4.7, R4.8)`);
  }
  return problems;
}

/**
 * Stable minor releases after the release that shipped the replacements: the
 * distinct (major, minor) above the lowest stable tag whose tree holds every
 * replacement. `tags` are [{ tag, hasReplacements }]; null when none ships them.
 */
export function minorsAfter(tags) {
  const stable = tags
    .map((t) => ({ ...t, version: stableVersion(t.tag) }))
    .filter((t) => t.version)
    .sort((a, b) => byVersion(a.version, b.version));
  const first = stable.find((t) => t.hasReplacements);
  if (!first) return null;
  const minors = new Set();
  for (const t of stable) {
    const [major, minor] = t.version;
    if (major > first.version[0] || (major === first.version[0] && minor > first.version[1])) minors.add(`${major}.${minor}`);
  }
  return minors.size;
}

// ── Reading a commit ─────────────────────────────────────────────────────────

function git(root, args, { allowFail = false } = {}) {
  try {
    return execFileSync('git', args, { cwd: root, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  } catch (err) {
    if (allowFail) return null;
    throw new Error(`git ${args.join(' ')}: ${(err.stderr || err.message).trim()}`);
  }
}

/** Whether `pattern` matches a non-test Go file directly in `dir` at `rev`. */
export function found(root, rev, { dir, pattern }) {
  try {
    execFileSync('git', ['grep', '-q', '-E', '-e', pattern, rev, '--', `:(glob)${dir}/*.go`, ':(glob,exclude)**/*_test.go'], {
      cwd: root,
      stdio: 'ignore',
    });
    return true;
  } catch (err) {
    if (err.status === 1) return false;
    throw new Error(`git grep at ${rev} failed (exit ${err.status})`);
  }
}

/** Everything decide() needs, read from `rev`; null when the rule predates it. */
export function inspect(root, rev = 'HEAD') {
  const sha = git(root, ['rev-parse', '--verify', `${rev}^{commit}`]).trim();
  const source = git(root, ['show', `${sha}:${REGISTRY}`], { allowFail: true });
  if (source === null) return null;
  const constants = parseConstants(source);
  const legacyFound = new Set(LEGACY_ITEMS.filter((item) => found(root, sha, item)).map((item) => item.id));
  const legacyPresent = LEGACY_ITEMS.filter((item) => legacyFound.has(item.id)).map((item) => item.id);
  const legacyRemoved = LEGACY_ITEMS.filter((item) => item.landed && !legacyFound.has(item.id)).map((item) => item.id);
  const replacementsPresent = REPLACEMENTS.filter((item) => found(root, sha, item)).map((item) => item.id);
  const commitDay = utcDay(git(root, ['log', '-1', '--format=%cI', sha]).trim());
  let minorsAfterReplacement = null;
  if (legacyRemoved.length > 0) {
    // Only the release floor reads history; refuse a short walk rather
    // than count the tags of a shallow clone.
    if (git(root, ['rev-parse', '--is-shallow-repository']).trim() === 'true') {
      throw new Error('the clone is shallow: the release tags reachable from the commit would be incomplete (fetch-depth: 0)');
    }
    const tags = git(root, ['tag', '--merged', sha])
      .split('\n')
      .filter((tag) => stableVersion(tag))
      .map((tag) => ({ tag, hasReplacements: REPLACEMENTS.every((item) => found(root, `refs/tags/${tag}^{commit}`, item)) }));
    minorsAfterReplacement = minorsAfter(tags);
  }
  return { sha, ...constants, commitDay, legacyPresent, legacyRemoved, replacementsPresent, minorsAfterReplacement };
}

/**
 * Whether `pattern` matches a non-test Go file directly in `dir` of a checked
 * out tree (the reminder reads its sparse checkout; it has no history).
 */
export function foundOnDisk(root, { dir, pattern }) {
  const re = new RegExp(pattern.replaceAll('[[:space:]]', String.raw`\s`));
  const full = path.join(root, dir);
  if (!fs.existsSync(full)) return false;
  return fs
    .readdirSync(full)
    .filter((name) => name.endsWith('.go') && !name.endsWith('_test.go'))
    .some((name) => re.test(fs.readFileSync(path.join(full, name), 'utf8')));
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const args = process.argv.slice(2);
  const flag = (name, fallback) => {
    const i = args.indexOf(name);
    return i >= 0 && args[i + 1] ? args[i + 1] : fallback;
  };
  const root = flag('--root', path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..'));
  const rev = flag('--sha', 'HEAD');
  let state;
  try {
    state = inspect(root, rev);
  } catch (err) {
    console.log(`::error::sunset check: ${err.message}`);
    process.exit(1);
  }
  if (state === null) {
    console.log(`sunset check: ${rev} has no ${REGISTRY}; it predates the rule, nothing to check`);
    process.exit(0);
  }
  const problems = decide(state);
  for (const p of problems) console.log(`::error::sunset check: ${p}`);
  if (problems.length) process.exit(1);
  console.log(
    `sunset check: ${state.sha.slice(0, 12)} dated ${state.commitDay}; ReplacementShippedOn "${state.replacementShippedOn}", ` +
      `SunsetAfter "${state.sunsetAfter}"; legacy items present: ${state.legacyPresent.join(', ') || 'none'}; ` +
      `replacements present: ${state.replacementsPresent.join(', ') || 'none'}`,
  );
}
