#!/usr/bin/env node
// Every dependency a chart declares is satisfied by what sits in its charts/.
//
//   node scripts/ci/check-chart-deps.mjs deploy/helm/elitea [more charts...]
//
// The elitea chart VENDORS its dependencies (charts/nats unpacked, and
// charts/elitea-sandbox-runtime a symlink), so neither CI nor a release ever
// runs `helm dependency build`. `helm dependency list` reports each declared
// dependency with a STATUS: `ok` (a packed .tgz matches) or `unpacked` (a
// directory matches) is satisfied; anything else (`missing`, `wrong version`,
// `misplaced`, a status a later Helm adds) is not. This reads the STATUS
// column of every row and passes only the two known-good values, instead of
// grepping for the bad words it knows today: an unknown status fails.
//
// A chart with no dependencies prints no table and passes. Used by
// helm-lint.yml (every PR) and publish.yml (before `helm package`). Needs
// only node and helm.

import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

export const SATISFIED = new Set(["ok", "unpacked"]);

/** Rows of a `helm dependency list` table: {name, version, repository, status}. */
export function parseListing(text) {
  const lines = text.split("\n").map((l) => l.trimEnd()).filter(Boolean);
  const header = lines.findIndex((l) => /^NAME\s+VERSION\s+REPOSITORY\s+STATUS$/.test(l.trim()));
  if (header < 0) return { table: false, rows: [] };
  const rows = [];
  for (const line of lines.slice(header + 1)) {
    // Columns are tab-separated and padded; STATUS itself may hold a space
    // ("wrong version"), so split on tabs, not on whitespace.
    const cols = line.split("\t").map((c) => c.trim());
    if (cols.length < 4) {
      rows.push({ name: cols[0] ?? line, version: "", repository: "", status: `(unparsed row: ${line})` });
      continue;
    }
    const [name, version, repository, ...rest] = cols;
    rows.push({ name, version, repository, status: rest.join(" ").trim() });
  }
  return { table: true, rows };
}

export function check(text) {
  const { table, rows } = parseListing(text);
  const problems = [];
  if (table && rows.length === 0) problems.push("the dependency table has a header and no rows");
  for (const r of rows) {
    if (!SATISFIED.has(r.status)) {
      problems.push(`${r.name} ${r.version}: status '${r.status}', want ok or unpacked (run the chart's vendor script)`);
    }
  }
  return { rows, problems };
}

function main() {
  const charts = process.argv.slice(2);
  if (!charts.length) {
    process.stderr.write("usage: check-chart-deps.mjs <chart dir>...\n");
    process.exit(2);
  }
  let failed = false;
  for (const chart of charts) {
    const helm = process.env.HELM || "helm";
    const out = execFileSync(helm, ["dependency", "list", chart], { encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] });
    process.stdout.write(out);
    const { rows, problems } = check(out);
    for (const p of problems) process.stdout.write(`::error::${chart}: ${p}\n`);
    if (problems.length) failed = true;
    else process.stdout.write(`${chart}: ${rows.length} dependency(ies), all satisfied by charts/\n`);
  }
  if (failed) process.exit(1);
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main();
