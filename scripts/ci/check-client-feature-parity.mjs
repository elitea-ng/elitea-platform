#!/usr/bin/env node
// The client feature-parity table (docs/client-feature-parity.md) is well formed.
//
// The table is how new work lands on web, desktop and mobile together: each
// feature row states, for each client, `shipped`, `planned (#<issue>)` or
// `n-a: <reason>`. This gate holds the shape, not the truth of each cell:
//
//   1. The `## Features` section has a table whose header is exactly
//      Feature | Web | Desktop | Mobile | Notes.
//   2. Every row has a status for all three clients, in one of the three forms.
//      `planned` names an issue; `n-a` gives a reason.
//   3. No feature name repeats.
//   4. The table has at least MINIMUM_ROWS rows. A table that parses to nothing
//      must fail: "no row found, therefore no bad row" is not a pass.
//
// Standard library only, so the job installs nothing for it.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SELF = fileURLToPath(import.meta.url);
const REPOSITORY_ROOT = path.resolve(path.dirname(SELF), "..", "..");
export const TABLE_PATH = "docs/client-feature-parity.md";

export const HEADER = ["Feature", "Web", "Desktop", "Mobile", "Notes"];
export const PLATFORMS = ["Web", "Desktop", "Mobile"];
export const MINIMUM_ROWS = 10;

const STATUS = [/^shipped$/, /^planned \(#[1-9]\d*\)$/, /^n-a: \S.*$/];

/** The length of the run of backticks that starts at `at`. */
function backtickRun(text, at) {
  let end = at;
  while (text[end] === "`") end++;
  return end - at;
}

/** Does a run of exactly `length` backticks start somewhere after `from`? */
function hasClosingRun(text, from, length) {
  for (let i = from; i < text.length; ) {
    if (text[i] !== "`") {
      i += text[i] === "\\" ? 2 : 1;
      continue;
    }
    const run = backtickRun(text, i);
    if (run === length) return true;
    i += run;
  }
  return false;
}

/**
 * Splits one markdown table line into its trimmed cells.
 *
 * A pipe separates cells unless it is escaped (`\|`) or inside a code span
 * (`` `a|b` ``): both are cell text, kept as written. A backtick run with no
 * closing run of the same length opens no span, as in CommonMark.
 */
export function cells(line) {
  const text = line.trim();
  const out = [];
  let cell = "";
  let span = 0; // the length of the open code span's backtick run; 0 outside one
  let endedOnPipe = false;
  for (let i = 0; i < text.length; ) {
    const ch = text[i];
    endedOnPipe = false;
    if (ch === "\\" && i + 1 < text.length) {
      cell += text.slice(i, i + 2);
      i += 2;
    } else if (ch === "`") {
      const run = backtickRun(text, i);
      if (span === 0 ? hasClosingRun(text, i + run, run) : run === span) span = span === 0 ? run : 0;
      cell += text.slice(i, i + run);
      i += run;
    } else if (ch === "|" && span === 0) {
      out.push(cell);
      cell = "";
      endedOnPipe = true;
      i++;
    } else {
      cell += ch;
      i++;
    }
  }
  if (!endedOnPipe) out.push(cell);
  if (text.startsWith("|")) out.shift();
  return out.map((value) => value.trim());
}

/** Is `value` a valid status cell? */
export function isStatus(value) {
  return STATUS.some((re) => re.test(value));
}

/**
 * Checks the table text.
 *
 * @param {string} markdown the whole document
 * @returns {{ok: boolean, rows: number, failures: string[]}}
 */
export function checkParityTable(markdown) {
  const failures = [];
  const lines = markdown.split(/\r?\n/);
  const start = lines.findIndex((line) => line.trim() === "## Features");
  if (start === -1) {
    return { ok: false, rows: 0, failures: ["no `## Features` section"] };
  }
  let i = start + 1;
  while (i < lines.length && !lines[i].trim().startsWith("|")) {
    if (lines[i].startsWith("## ")) break;
    i++;
  }
  if (i >= lines.length || !lines[i].trim().startsWith("|")) {
    return { ok: false, rows: 0, failures: ["the `## Features` section has no table"] };
  }
  const header = cells(lines[i]);
  if (header.join("|") !== HEADER.join("|")) {
    failures.push(`the header must be | ${HEADER.join(" | ")} |, found | ${header.join(" | ")} |`);
  }
  const separator = lines[i + 1] === undefined ? [] : cells(lines[i + 1]);
  if (separator.length !== HEADER.length || !separator.every((cell) => /^:?-{3,}:?$/.test(cell))) {
    failures.push("the header must be followed by a separator row with one cell per column");
  }
  const seen = new Set();
  let rows = 0;
  for (let n = i + 2; n < lines.length && lines[n].trim().startsWith("|"); n++) {
    rows++;
    const lineNo = n + 1;
    const row = cells(lines[n]);
    if (row.length !== HEADER.length) {
      failures.push(`line ${lineNo}: ${row.length} cells, expected ${HEADER.length}`);
      continue;
    }
    const [feature] = row;
    if (feature === "") failures.push(`line ${lineNo}: the feature name is empty`);
    else if (seen.has(feature)) failures.push(`line ${lineNo}: "${feature}" repeats`);
    seen.add(feature);
    PLATFORMS.forEach((platform, index) => {
      const value = row[index + 1];
      if (isStatus(value)) return;
      let why = "use shipped, planned (#<issue>) or n-a: <reason>";
      if (value === "") why = "the status is missing";
      else if (/^planned\b/.test(value)) why = "planned must name its issue: planned (#<issue>)";
      else if (/^n-?a\b/i.test(value)) why = "n-a must give a reason: n-a: <reason>";
      failures.push(`line ${lineNo}: "${feature}" ${platform} is "${value}": ${why}`);
    });
  }
  if (rows < MINIMUM_ROWS) {
    failures.push(`the table has ${rows} rows, fewer than the floor of ${MINIMUM_ROWS}: did the table stop parsing?`);
  }
  return { ok: failures.length === 0, rows, failures };
}

function main() {
  const file = path.join(REPOSITORY_ROOT, TABLE_PATH);
  let markdown;
  try {
    markdown = fs.readFileSync(file, "utf8");
  } catch (error) {
    console.error(`check-client-feature-parity: cannot read ${TABLE_PATH}: ${error.message}`);
    return 1;
  }
  const result = checkParityTable(markdown);
  if (!result.ok) {
    console.error(`check-client-feature-parity: FAIL (${TABLE_PATH})`);
    for (const failure of result.failures) console.error(`  ${failure}`);
    return 1;
  }
  console.log(`check-client-feature-parity: OK: ${result.rows} features, each with a status for ${PLATFORMS.join(", ")}.`);
  return 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === SELF) process.exitCode = main();
