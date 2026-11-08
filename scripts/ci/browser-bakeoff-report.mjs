// The ADR-0049 decision 9 bake-off report (#1599): reads the JSON lines of
// libs/rust/browser-session/tests/bakeoff.rs (one per engine and task) and
// applies the admission rule.
//
// Moli (`lite`) is admitted only when the run is comparable (below) and all
// three hold:
//
//   1. it succeeds on at least 80% of the read and extract tasks;
//   2. its overall success rate is no more than 10 points below `full`'s;
//   3. no task crashed or hung it. A crash or hang the session agent
//      recovered from is not counted here: the agent never restarts an engine
//      in place, so every crash or hang in a line is unrecoverable for its
//      session.
//
// The run must also be a fair comparison: `full` (Chrome, the engine every
// session runs today) has to succeed on at least half the corpus. Below
// that the run measured the harness or the corpus (sites that moved, a
// start refused), not Moli, and the verdict is "no comparison" whatever
// lite scored (the first real run, 42434760930, had full at 0/40).
//
// `auto` is reported beside them, and is admitted with `lite`: it runs Moli
// first and falls back to `full`, which already passed.
//
// The report is Markdown for the ADR-0049 appendix and the job summary, and
// the verdict a JSON object. Nothing here flips the admission: the change
// that records a passing appendix sets platformconfig.BrowserLiteAdmitted.
//
//   node scripts/ci/browser-bakeoff-report.mjs results.jsonl [--json verdict.json]
//
// Exit 0 whatever the verdict (the report is the output); exit 2 on input
// that does not hold a full and a lite run of the same tasks.

import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

export const READ_EXTRACT_FLOOR = 0.8;
export const MAX_POINTS_BEHIND = 10;
export const FULL_BASELINE_FLOOR = 0.5;

export function parse(text) {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line, index) => {
      try {
        return JSON.parse(line);
      } catch {
        throw new Error(`line ${index + 1} is not JSON`);
      }
    });
}

function quantile(sorted, q) {
  if (sorted.length === 0) return null;
  // Nearest rank: the smallest value with at least q of the sample at or below it.
  const rank = Math.max(1, Math.ceil(q * sorted.length));
  return sorted[rank - 1];
}

const rate = (hits, of) => (of === 0 ? null : hits / of);

export function summarise(lines, engine) {
  const runs = lines.filter((line) => line.engine === engine);
  const readExtract = runs.filter((line) => line.kind === "read" || line.kind === "extract");
  const memory = runs.map((line) => line.peak_memory_bytes).filter((bytes) => typeof bytes === "number").sort((a, b) => a - b);
  const firstSnapshot = runs.map((line) => line.first_snapshot_ms).filter((ms) => typeof ms === "number").sort((a, b) => a - b);
  const refs = runs.map((line) => line.refs).filter((count) => typeof count === "number").sort((a, b) => a - b);
  return {
    engine,
    tasks: runs.length,
    ok: runs.filter((line) => line.ok).length,
    success: rate(runs.filter((line) => line.ok).length, runs.length),
    read_extract: readExtract.length,
    read_extract_success: rate(readExtract.filter((line) => line.ok).length, readExtract.length),
    crashed: runs.filter((line) => line.crashed).map((line) => line.task),
    hung: runs.filter((line) => line.hung).map((line) => line.task),
    memory_median_bytes: quantile(memory, 0.5),
    memory_p95_bytes: quantile(memory, 0.95),
    memory_samples: memory.length,
    first_snapshot_median_ms: quantile(firstSnapshot, 0.5),
    first_snapshot_p95_ms: quantile(firstSnapshot, 0.95),
    refs_median: quantile(refs, 0.5),
    zero_ref_snapshots: runs.filter((line) => line.refs === 0).length,
    switched: runs.filter((line) => line.switched).map((line) => `${line.task}:${line.switched}`),
    version: runs.find((line) => line.version)?.version ?? null,
  };
}

export function check(result, id) {
  return result.checks.find((entry) => entry.id === id);
}

export function verdict(lines) {
  const full = summarise(lines, "full");
  const lite = summarise(lines, "lite");
  if (full.tasks === 0 || lite.tasks === 0) {
    throw new Error("the results hold no full or no lite run");
  }
  const fullTasks = new Set(lines.filter((l) => l.engine === "full").map((l) => l.task));
  const liteTasks = new Set(lines.filter((l) => l.engine === "lite").map((l) => l.task));
  if (fullTasks.size !== liteTasks.size || [...fullTasks].some((task) => !liteTasks.has(task))) {
    throw new Error("full and lite ran different tasks: the comparison would not be like for like");
  }
  const behind = (full.success - lite.success) * 100;
  const comparable = full.success !== null && full.success >= FULL_BASELINE_FLOOR;
  const checks = [
    {
      id: "baseline",
      rule: `full (the baseline) succeeds on at least ${FULL_BASELINE_FLOOR * 100}% of the corpus`,
      pass: comparable,
      measured: pct(full.success),
    },
    {
      id: "read_extract",
      rule: `lite succeeds on at least ${READ_EXTRACT_FLOOR * 100}% of read and extract tasks`,
      pass: lite.read_extract_success !== null && lite.read_extract_success >= READ_EXTRACT_FLOOR,
      measured: lite.read_extract_success === null ? "no read or extract task" : pct(lite.read_extract_success),
    },
    {
      id: "behind",
      rule: `lite is no more than ${MAX_POINTS_BEHIND} points behind full overall`,
      pass: behind <= MAX_POINTS_BEHIND,
      measured: `${pct(lite.success)} vs ${pct(full.success)} (${behind.toFixed(1)} points behind)`,
    },
    {
      id: "stable",
      rule: "no crash or hang of lite",
      pass: lite.crashed.length === 0 && lite.hung.length === 0,
      measured: `${lite.crashed.length} crashed, ${lite.hung.length} hung`,
    },
  ];
  // With no working baseline, the lite rows say nothing about Moli: they are
  // shown as not measured, never as a pass.
  if (!comparable) {
    for (const check of checks) {
      if (check.id === "baseline") continue;
      check.pass = null;
      check.measured = `n/a (no comparison; measured ${check.measured})`;
    }
  }
  const auto = summarise(lines, "auto");
  return {
    comparable,
    admitted: comparable && checks.every((check) => check.pass === true),
    checks,
    engines: [full, lite, ...(auto.tasks ? [auto] : [])],
  };
}

function pct(value) {
  return value === null ? "n/a" : `${(value * 100).toFixed(1)}%`;
}

function mib(bytes) {
  return bytes === null ? "n/a" : `${(bytes / 1048576).toFixed(0)} MiB`;
}

function msOr(value) {
  return value === null ? "n/a" : `${value} ms`;
}

export function markdown(result, lines) {
  const out = [];
  const said = result.admitted
    ? "Moli is admitted"
    : result.comparable
      ? "Moli is NOT admitted"
      : "no comparison: the full baseline failed, so this run measured the harness or the corpus, not Moli. Moli is NOT admitted";
  out.push(`**Verdict: ${said}.**`, "");
  out.push("| Rule | Measured | Pass |", "|---|---|---|");
  for (const check of result.checks) out.push(`| ${check.rule} | ${check.measured} | ${check.pass === null ? "n/a" : check.pass ? "yes" : "no"} |`);
  out.push("", "| | " + result.engines.map((e) => e.engine).join(" | ") + " |", "|---|" + result.engines.map(() => "---").join("|") + "|");
  const row = (label, fn) => out.push(`| ${label} | ${result.engines.map(fn).join(" | ")} |`);
  row("engine version", (e) => e.version ?? "n/a");
  row("tasks", (e) => String(e.tasks));
  row("success", (e) => `${e.ok}/${e.tasks} (${pct(e.success)})`);
  row("read/extract success", (e) => pct(e.read_extract_success));
  row("median actionable refs (first snapshot)", (e) => (e.refs_median === null ? "n/a" : String(e.refs_median)));
  row("snapshots with no ref", (e) => String(e.zero_ref_snapshots));
  row("peak memory, median", (e) => mib(e.memory_median_bytes));
  row("peak memory, p95", (e) => mib(e.memory_p95_bytes));
  row("time to first snapshot, median", (e) => msOr(e.first_snapshot_median_ms));
  row("time to first snapshot, p95", (e) => msOr(e.first_snapshot_p95_ms));
  row("crashed", (e) => (e.crashed.length ? e.crashed.join(", ") : "none"));
  row("hung", (e) => (e.hung.length ? e.hung.join(", ") : "none"));
  row("auto switches", (e) => (e.switched.length ? e.switched.join(", ") : "none"));
  out.push("", "<details><summary>Every task</summary>", "", "| Task | Class | Kind | Engine | OK | Refs | First snapshot | Peak memory | Failure |", "|---|---|---|---|---|---|---|---|---|");
  for (const line of lines) {
    const failure = String(line.failure ?? "").replaceAll("|", "\\|");
    out.push(`| ${line.task} | ${line.class} | ${line.kind} | ${line.engine} | ${line.ok ? "yes" : "no"} | ${line.refs ?? "n/a"} | ${msOr(line.first_snapshot_ms ?? null)} | ${mib(line.peak_memory_bytes ?? null)} | ${failure} |`);
  }
  out.push("", "</details>", "");
  return out.join("\n");
}

function main(argv) {
  const [input, ...rest] = argv;
  if (!input) {
    console.error("usage: browser-bakeoff-report.mjs results.jsonl [--json verdict.json]");
    return 2;
  }
  let lines;
  let result;
  try {
    lines = parse(readFileSync(input, "utf8"));
    result = verdict(lines);
  } catch (error) {
    console.error(`::error::${error.message}`);
    return 2;
  }
  process.stdout.write(markdown(result, lines));
  const at = rest.indexOf("--json");
  if (at !== -1 && rest[at + 1]) writeFileSync(rest[at + 1], JSON.stringify(result, null, 2) + "\n");
  return 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  process.exitCode = main(process.argv.slice(2));
}
