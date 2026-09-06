#!/usr/bin/env node
/**
 * check-stale-disclosure.mjs — fails the build when a disclosed gap has been
 * closed and its comment still says otherwise (issue 621).
 *
 * The rules and the reasoning live in scripts/lib/stale-disclosure-core.mjs.
 * This file gathers the facts: it reads the scope list, reads each scoped
 * file, and runs each probe over the repository.
 *
 * It lives under apps/elitea-web/scripts because that is where this repository
 * keeps its gate scripts and their coverage-measured `lib/*-core.mjs` rule
 * modules. It reads the whole repository, exactly as check-ci-dormancy.mjs and
 * check-playwright-image-tag.mjs do.
 *
 * SCOPE, AND WHY IT IS A LIST
 *
 * scripts/stale-disclosure-scope.txt names the files the gate holds to the
 * rule. It is not the whole repository: 146 lines across 40 files carry one of
 * these markers today, most of them in tests, and demanding a falsifier for
 * every one of them in a single change would produce 146 probes nobody had
 * thought about. The list may only GROW. Add a file, annotate its claims, and
 * the gate holds them from then on.
 *
 * Usage:
 *   node scripts/check-stale-disclosure.mjs [--scope <path>] [--json]
 */
import { readFileSync, readdirSync, statSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { evaluateFile, literalOccurs } from './lib/stale-disclosure-core.mjs';
import { checkFloors } from './lib/gate-floor.mjs';

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const appRoot = path.resolve(scriptDir, '..');
const repoRoot = path.resolve(appRoot, '../..');

const args = process.argv.slice(2);
const asJson = args.includes('--json');
const scopeArg = args.indexOf('--scope');
const scopePath =
  scopeArg >= 0 && args[scopeArg + 1]
    ? path.resolve(process.cwd(), args[scopeArg + 1])
    : path.join(scriptDir, 'stale-disclosure-scope.txt');

/** Read the scope list: one repo-relative path per line, `#` starts a comment. */
function readScope(file) {
  return readFileSync(file, 'utf8')
    .split('\n')
    .map((line) => line.replace(/#.*$/, '').trim())
    .filter((line) => line !== '');
}

/** Every file under `target`, or `target` itself when it is a file. */
function filesUnder(target) {
  let stats;
  try {
    stats = statSync(target);
  } catch {
    return [];
  }
  if (stats.isFile()) return [target];
  const out = [];
  for (const entry of readdirSync(target, { withFileTypes: true })) {
    if (entry.name === 'node_modules' || entry.name === '.git') continue;
    out.push(...filesUnder(path.join(target, entry.name)));
  }
  return out;
}

/**
 * Run one probe over the repository.
 *
 * The literal is compared as a SUBSTRING, not as a regular expression: a probe
 * is written by hand beside a claim, and a claim's author should not have to
 * escape anything to name the code that would falsify it. `literalOccurs`
 * carries the one rule that is not obvious — a probe line never counts as
 * evidence for itself.
 */
function runProbe(probe) {
  const files = filesUnder(path.resolve(repoRoot, probe.target));
  let matched = false;
  for (const file of files) {
    let text;
    try {
      text = readFileSync(file, 'utf8');
    } catch {
      continue;
    }
    if (literalOccurs(text, probe.literal)) {
      matched = true;
      break;
    }
  }
  return { matched, filesSearched: files.length };
}

const scope = readScope(scopePath);
const violations = [];
let claims = 0;
let probes = 0;

for (const entry of scope) {
  const absolute = path.resolve(repoRoot, entry);
  let text;
  try {
    text = readFileSync(absolute, 'utf8');
  } catch {
    violations.push({
      file: entry,
      line: 0,
      kind: 'blind',
      message: `named in ${path.relative(repoRoot, scopePath)} and not readable. A scope entry that does not resolve measures nothing.`,
    });
    continue;
  }
  const result = evaluateFile({ file: entry, text, runProbe });
  violations.push(...result.violations);
  claims += result.claims;
  probes += result.probes;
}

// The floors are the "absence reads as correctness" guard for this gate
// itself: an empty scope list, or a scope whose files carry no claim, exits 0
// while measuring nothing.
const floor = checkFloors('check-stale-disclosure', [
  { subject: `files in ${path.relative(repoRoot, scopePath)}`, observed: scope.length, floor: 2 },
  { subject: 'disclosure claims in those files', observed: claims, floor: 5 },
  { subject: 'DISCLOSURE-CHECK probes beside them', observed: probes, floor: 5 },
]);

if (asJson) {
  console.log(JSON.stringify({ ok: floor.ok && violations.length === 0, claims, probes, violations }, null, 2));
} else {
  for (const line of floor.lines) console.log(line);
  for (const violation of violations) {
    console.error(`${violation.kind.toUpperCase()}  ${violation.file}:${violation.line}  ${violation.message}`);
  }
}

if (!floor.ok) {
  if (!asJson) console.error(floor.error);
  process.exit(2);
}
if (violations.length > 0) {
  if (!asJson) console.error(`check-stale-disclosure: FAIL — ${violations.length} offence(s)`);
  process.exit(1);
}
if (!asJson) console.log('check-stale-disclosure: OK');
