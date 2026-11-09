#!/usr/bin/env node
// The brand repository's view of ELITEA (ADR-0052 W5): read and validate
// upstream.json, pick the newest release a channel follows, and write the bump.
//
//   node scripts/upstream.mjs check                 validate upstream.json, print the tag
//   node scripts/upstream.mjs github-output         tag/repository/version to $GITHUB_OUTPUT
//
// The release watch (.github/workflows/release-watch.yml) calls `newestRelease`
// and `bumped` from actions/github-script. Node's standard library only, so the
// template needs no install before it can read its own pin.

import { appendFileSync, readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

export const UPSTREAM_FILE = resolve(dirname(fileURLToPath(import.meta.url)), '..', 'upstream.json');

/** A release tag: vX.Y.Z, or vX.Y.Z-<pre> (ELITEA's nightlies are vX.Y.Z-nightly.<date>.<n>). */
const TAG = /^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z][0-9A-Za-z.-]*))?$/;
const REPOSITORY = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const CHANNELS = ['stable', 'prerelease'];
const KEYS = ['$comment', 'repository', 'tag', 'channel'];

/** The problems with an upstream.json object (empty: valid). */
export function validate(upstream) {
  const problems = [];
  if (upstream === null || typeof upstream !== 'object' || Array.isArray(upstream)) return ['upstream.json must be an object'];
  for (const key of Object.keys(upstream)) if (!KEYS.includes(key)) problems.push(`unknown key "${key}"`);
  if (typeof upstream.repository !== 'string' || !REPOSITORY.test(upstream.repository)) problems.push('repository must be "owner/name"');
  if (typeof upstream.tag !== 'string' || !TAG.test(upstream.tag)) problems.push('tag must be a release tag, vX.Y.Z or vX.Y.Z-<pre>');
  if (!CHANNELS.includes(upstream.channel)) problems.push(`channel must be one of ${CHANNELS.join(', ')}`);
  if (upstream.channel === 'stable' && typeof upstream.tag === 'string' && TAG.exec(upstream.tag)?.[4]) {
    problems.push('a stable channel pins a stable tag (no -<pre> suffix)');
  }
  return problems;
}

/** upstream.json, parsed and validated; throws with every problem. */
export function readUpstream(file = UPSTREAM_FILE) {
  const upstream = JSON.parse(readFileSync(file, 'utf8'));
  const problems = validate(upstream);
  if (problems.length) throw new Error(`${file}:\n  - ${problems.join('\n  - ')}`);
  return upstream;
}

/** The parts of a tag for ordering, or null. */
export function parseTag(tag) {
  const m = TAG.exec(tag ?? '');
  return m ? { major: +m[1], minor: +m[2], patch: +m[3], pre: m[4] ?? '' } : null;
}

/** SemVer 2.0 prerelease order (§11): numeric ids numerically, others as text, more ids win. */
function comparePre(a, b) {
  if (a === b) return 0;
  if (a === '') return 1;
  if (b === '') return -1;
  const x = a.split('.');
  const y = b.split('.');
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    if (x[i] === undefined) return -1;
    if (y[i] === undefined) return 1;
    const nx = /^\d+$/.test(x[i]);
    const ny = /^\d+$/.test(y[i]);
    if (nx && ny && +x[i] !== +y[i]) return +x[i] - +y[i];
    if (nx !== ny) return nx ? -1 : 1;
    if (x[i] !== y[i]) return x[i] < y[i] ? -1 : 1;
  }
  return 0;
}

/** Negative when tag a is older than b, by SemVer order. */
export function compareTags(a, b) {
  const x = parseTag(a);
  const y = parseTag(b);
  if (!x || !y) throw new Error(`not a release tag: ${!x ? a : b}`);
  return x.major - y.major || x.minor - y.minor || x.patch - y.patch || comparePre(x.pre, y.pre);
}

/**
 * The newest release the channel follows, from GitHub's release list
 * (`{ tag_name, draft, prerelease }`), or null. Drafts and non-release tags never
 * count; `stable` skips prereleases.
 */
export function newestRelease(releases, channel) {
  const candidates = releases.filter(
    (r) => !r.draft && parseTag(r.tag_name) && (channel === 'prerelease' || (!r.prerelease && !parseTag(r.tag_name).pre)),
  );
  candidates.sort((a, b) => compareTags(b.tag_name, a.tag_name));
  return candidates[0]?.tag_name ?? null;
}

/** The new upstream.json text for `tag`, or null when `tag` is not newer. */
export function bumped(text, tag) {
  const upstream = JSON.parse(text);
  if (validate(upstream).length) throw new Error('upstream.json is invalid; fix it before the watch can bump it');
  if (compareTags(tag, upstream.tag) <= 0) return null;
  return `${JSON.stringify({ ...upstream, tag }, null, 2)}\n`;
}

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const [command] = process.argv.slice(2);
  try {
    const upstream = readUpstream();
    if (command === 'check') {
      console.log(`${upstream.repository}@${upstream.tag} (${upstream.channel})`);
    } else if (command === 'github-output') {
      const out = process.env.GITHUB_OUTPUT;
      if (!out) throw new Error('GITHUB_OUTPUT is not set');
      appendFileSync(out, `tag=${upstream.tag}\nrepository=${upstream.repository}\nversion=${upstream.tag.slice(1)}\n`);
    } else {
      console.error('usage: upstream.mjs check | github-output');
      process.exit(2);
    }
  } catch (error) {
    console.log(`::error::${error.message.replaceAll('\n', '%0A')}`);
    process.exit(1);
  }
}

