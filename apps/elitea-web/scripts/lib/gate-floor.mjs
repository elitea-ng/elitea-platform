/**
 * gate-floor.mjs — the shared "the subject set is not empty" rule (issue #528).
 *
 * A gate reads a subject set and reports the offences in it. When that set is
 * EMPTY, the gate finds no offence and exits 0. Absence then reads as
 * correctness: nobody can tell "the gate looked at 3164 modules and liked them
 * all" apart from "the gate looked at nothing". Both print the same tick.
 *
 * Eight gate scripts had that hole. Eight others already state a count and fail
 * below it — `check-handlers.mjs`,
 * `check-budgets.mjs`, `theme-gate.mjs`, `check-bundle-secrets.mjs`,
 * `check-partition.mjs`, `check-ci-dormancy.mjs`, `check-bundle-budget.mjs`
 * and `check-mutator-coverage.mjs`. This module is that same line, written
 * once, so every caller states its numbers in the same words.
 *
 * TWO RULES, and the second one matters as much as the first:
 *
 *   1. Fail below the floor. An empty or implausibly small subject set is a
 *      broken gate, not a clean tree.
 *   2. PRINT the count on the pass path too. A reader must see WHAT the gate
 *      measured, not only that it measured something. A floor nobody reads
 *      goes stale in silence.
 *
 * A floor is a plausibility bar, not a target. Set it well under today's
 * number, so ordinary work never trips it, and well over zero, so a subject
 * set that collapses does.
 *
 * Everything here is pure: counts in, decision out. The callers own the
 * printing and the exit code, because `process.exit` inside a rule module
 * cannot be tested.
 */

import path from 'node:path';

/**
 * Name a file the way a reader wants to read it.
 *
 * A floor message carries the path of the subject it counted. Inside the tree
 * the relative path is the clear form; a fixture passed on the command line
 * sits outside it, and `path.relative` then answers with a chain of `../`
 * nobody can follow back.
 *
 * @param {string} root the directory to measure against.
 * @param {string} target the absolute path of the subject.
 * @returns {string} the relative path, or the absolute one when the target
 *   sits outside `root`.
 */
export function subjectPath(root, target) {
  const relative = path.relative(root, target);
  return relative.startsWith('..') ? target : relative;
}

/**
 * @typedef {object} FloorCheck
 * @property {string} subject what the count counts, in words a reader
 *   understands without the source ("indexed shots in
 *   parity/screenshot-index.json").
 * @property {number} observed the count this run measured.
 * @property {number} floor the smallest count that still proves the gate ran.
 */

/**
 * Measure every floor of one gate.
 *
 * @param {string} gate the script name, for the message prefix.
 * @param {readonly FloorCheck[]} checks every subject set the gate reads.
 * @returns {{ok: boolean, lines: string[], error: string|null}} `lines` are
 *   the count statements to print on EVERY path. `error` is the failure text,
 *   or null.
 */
export function checkFloors(gate, checks) {
  // A gate that declares no floor is exactly the defect this module exists to
  // remove, so an empty list is a failure and not a silent pass.
  if (!Array.isArray(checks) || checks.length === 0) {
    return {
      ok: false,
      lines: [],
      error:
        `${gate}: FAIL — this gate states no floor, so it proves nothing about its subject set.\n` +
        'Give it at least one { subject, observed, floor } check.',
    };
  }

  const lines = [];
  const short = [];
  for (const check of checks) {
    lines.push(`${gate}: measured ${check.observed} ${check.subject} (floor ${check.floor}).`);
    if (check.observed < check.floor) short.push(check);
  }

  if (short.length === 0) return { ok: true, lines, error: null };

  const detail = short.map((c) => `  ${c.observed} ${c.subject} — floor ${c.floor}`).join('\n');
  return {
    ok: false,
    lines,
    error:
      `${gate}: FAIL — the subject set is empty or too small, so a clean result proves nothing:\n${detail}\n` +
      'Find what stopped feeding the gate: a renamed input, a moved directory, a key that\n' +
      'changed name, a glob that matches nothing. Lower a floor only together with the\n' +
      'measured number that replaces it.',
  };
}
