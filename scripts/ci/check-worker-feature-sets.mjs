#!/usr/bin/env node
// The Rust worker is linted and tested with the feature sets its images build, and with no other.
//
// services/elitea-worker-rust/Containerfile builds two binaries: the worker image with no feature flag (the
// default set, which is empty) and the code sandbox supervisor with `--features sandbox-supervisor`. A Clippy
// or test run with `--all-features` adds the two bench-only features, and a run with one set only hides what
// the other set compiles: dead code and lints that exist only in the default build (the image every
// deployment runs) passed CI for months behind `--all-features`.
//
// This check reads the image feature sets from the Containerfile's `cargo auditable build` commands and every
// `cargo build|check|clippy|doc|test|nextest|run|bench|rustc` a workflow runs against the worker (its
// directory from the step's `working-directory`, the job's or workflow's `defaults.run`, a `cd` in the
// script or `--manifest-path`; a job's `strategy.matrix.include` is expanded, `${{ matrix.<key> }}`
// substituted and a step skipped when its `if:` compares a matrix key the leg does not match), plus the
// sandbox ledger helper the worker's test job runs. It fails when:
//   * a run against the worker passes `--all-features`;
//   * a run against the worker passes a feature list that is not exactly one of the image sets;
//   * an image set has no Clippy run, or no test run;
//   * a Containerfile build passes `--all-features`, or the Containerfile names fewer than two sets, or the
//     workflows hold fewer worker runs than FLOOR ("found nothing, therefore clean").
//
// docs/contributing/ci.md, "Rust".

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { loadYaml, isEntrypoint } from "./ci-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SELF), "..", "..");

export const WORKER_DIR = "services/elitea-worker-rust";
const CONTAINERFILE = `${WORKER_DIR}/Containerfile`;
const WORKFLOWS = ".github/workflows";
/** Files outside the workflows that run cargo against the worker for a workflow, as a Python argv list. */
const HELPERS = ["scripts/runtime/test_sandbox_ledger.py"];
const SUBCOMMANDS = new Set(["build", "check", "clippy", "doc", "test", "nextest", "run", "bench", "rustc"]);
const LINT = new Set(["clippy"]);
const TEST = new Set(["test", "nextest"]);
/** Fewer worker cargo runs than this across the workflows means the scan stopped seeing them. */
export const FLOOR = 8;

/**
 * The shell words of a script: continuation lines joined, quotes removed, operators as their own words. A
 * `$( ... )` inside double quotes is split out too: the shell runs it.
 */
export function shellWords(script) {
  const text = script.replace(/\\\r?\n/g, " ");
  const words = [];
  let word = "";
  let quote = null;
  let started = false;
  const parens = [];
  const push = () => {
    if (started) words.push(word);
    word = "";
    started = false;
  };
  for (let i = 0; i < text.length; i += 1) {
    const c = text[i];
    if (quote === '"' && c === "$" && text[i + 1] === "(") {
      push();
      words.push("$(");
      parens.push('"');
      quote = null;
      i += 1;
      continue;
    }
    if (quote) {
      if (c === quote) quote = null;
      else if (c === "\\" && quote === '"' && i + 1 < text.length) word += text[++i];
      else word += c;
      continue;
    }
    if (c === "'" || c === '"') {
      quote = c;
      started = true;
    } else if (c === "#" && !started) {
      while (i < text.length && text[i] !== "\n") i += 1;
      push();
      words.push(";");
    } else if (c === "\n" || c === ";") {
      push();
      words.push(";");
    } else if (c === " " || c === "\t") {
      push();
    } else if (c === "$" && text[i + 1] === "(") {
      push();
      words.push("$(");
      parens.push(null);
      i += 1;
    } else if (c === "(") {
      push();
      words.push(c);
      parens.push(null);
    } else if (c === ")") {
      push();
      words.push(c);
      if (parens.pop() === '"') {
        quote = '"';
        started = true;
      }
    } else if ((c === "&" || c === "|") && text[i + 1] === c) {
      push();
      words.push(c + c);
      i += 1;
    } else if (c === "|" || c === "&") {
      push();
      words.push(c);
    } else {
      word += c;
      started = true;
    }
  }
  push();
  return words;
}

const BOUNDARY = new Set([";", "&&", "||", "|", "&", "(", ")", "$("]);
const ASSIGNMENT = /^[A-Za-z_][A-Za-z0-9_]*=/;

/** A feature list's canonical spelling: sorted, unique, comma-joined ("" for the default set). */
export function canonical(features) {
  return [...new Set(features.flatMap((f) => f.split(/[,\s]+/)).filter(Boolean))].sort().join(",");
}

/** The feature arguments of one cargo argv (the words after the subcommand, up to `--`). */
export function featureSet(args) {
  const features = [];
  let all = false;
  for (let i = 0; i < args.length; i += 1) {
    const a = args[i];
    if (a === "--") break;
    if (a === "--all-features") all = true;
    else if (a === "--features" || a === "-F") features.push(args[++i] ?? "");
    else if (a.startsWith("--features=")) features.push(a.slice("--features=".length));
    else if (a.startsWith("-F") && a.length > 2) features.push(a.slice(2));
  }
  return { all, set: canonical(features) };
}

function manifestDir(args) {
  for (let i = 0; i < args.length; i += 1) {
    if (args[i] === "--") break;
    if (args[i] === "--manifest-path") return path.posix.dirname(args[i + 1] ?? "");
    if (args[i].startsWith("--manifest-path=")) return path.posix.dirname(args[i].slice("--manifest-path=".length));
  }
  return null;
}

/**
 * Every cargo run in a script: { dir, subcommand, args } with `dir` the repository-relative directory it runs
 * in. `cd` moves the directory until the subshell (`(` or `$(`) it was typed in closes.
 */
export function cargoRuns(script, startDir) {
  const words = shellWords(script);
  const runs = [];
  const stack = [];
  let dir = path.posix.normalize(startDir || ".");
  let atStart = true;
  for (let i = 0; i < words.length; i += 1) {
    const w = words[i];
    if (w === "(" || w === "$(") {
      stack.push(dir);
      atStart = true;
      continue;
    }
    if (w === ")") {
      if (stack.length) dir = stack.pop();
      atStart = false;
      continue;
    }
    if (BOUNDARY.has(w)) {
      atStart = true;
      continue;
    }
    if (!atStart) continue;
    if (ASSIGNMENT.test(w) || w === "env" || w === "time" || w === "exec") continue;
    if (w === "rustup" && words[i + 1] === "run") {
      i += 2;
      continue;
    }
    atStart = false;
    if (w === "cd") {
      const to = words[i + 1];
      if (to !== undefined && !BOUNDARY.has(to)) {
        const target = to.replace(/^"?\$\{?GITHUB_WORKSPACE\}?\/?/, "/");
        dir = target.startsWith("/") ? path.posix.normalize(`.${target}`) : path.posix.join(dir, target);
        i += 1;
      }
      continue;
    }
    if (w !== "cargo") continue;
    const argv = [];
    for (let j = i + 1; j < words.length && !BOUNDARY.has(words[j]); j += 1) argv.push(words[j]);
    while (argv.length && argv[0].startsWith("+")) argv.shift();
    const subcommand = argv.shift();
    if (!SUBCOMMANDS.has(subcommand)) continue;
    const manifest = manifestDir(argv);
    runs.push({ dir: manifest ? path.posix.join(dir, manifest) : dir, subcommand, args: argv });
  }
  return runs;
}

/** The image feature sets: one per `cargo auditable build` (or `cargo build`) in the Containerfile. */
export function imageFeatureSets(containerfile) {
  const errors = [];
  const sets = new Set();
  for (const line of containerfile.replace(/\\\r?\n/g, " ").split("\n")) {
    if (line.trimStart().startsWith("#")) continue;
    for (const m of line.matchAll(/cargo (?:auditable )?build([^&;|]*)/g)) {
      const { all, set } = featureSet(shellWords(m[1]).filter((w) => !BOUNDARY.has(w)));
      if (all) errors.push(`${CONTAINERFILE}: an image build passes --all-features`);
      sets.add(set);
    }
  }
  return { sets, errors };
}

/** The matrix legs of a job: `include` entries (or the cartesian product of its axes), or one empty leg. */
export function matrixLegs(job) {
  const matrix = job?.strategy?.matrix;
  if (!matrix || typeof matrix !== "object") return [{}];
  const { include, exclude: _exclude, ...axes } = matrix;
  let legs = [{}];
  for (const [key, values] of Object.entries(axes)) {
    if (!Array.isArray(values)) continue;
    legs = legs.flatMap((leg) => values.map((v) => ({ ...leg, [key]: v })));
  }
  if (Array.isArray(include) && include.length) {
    legs = Object.keys(axes).length ? [...legs, ...include] : include;
  }
  return legs.length ? legs : [{}];
}

/** `${{ matrix.<key> }}` replaced by the leg's value ("" when the leg has none). */
export function substitute(text, leg) {
  return String(text ?? "").replace(/\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}/g, (_, key) =>
    leg[key] === undefined || leg[key] === null ? "" : String(leg[key]),
  );
}

/** `$NAME` and `${NAME}` replaced by the step's `env:` value, where it sets one. */
export function expandEnv(text, env) {
  return text.replace(/\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?/g, (whole, name) =>
    Object.hasOwn(env, name) ? String(env[name] ?? "") : whole,
  );
}

/**
 * False when a step's `if:` cannot hold for this leg: an `==`/`!=` against a matrix key the leg does not
 * match, or a bare `matrix.<key>` the leg leaves empty, in a condition with no `||`.
 */
export function legRuns(condition, leg) {
  if (condition === undefined || condition === null || condition === true) return true;
  const text = String(condition).replace(/^\$\{\{|\}\}$/g, "").trim();
  if (text.includes("||")) return true;
  for (const m of text.matchAll(/matrix\.([A-Za-z0-9_-]+)\s*(==|!=)\s*'([^']*)'/g)) {
    const value = leg[m[1]] === undefined ? "" : String(leg[m[1]]);
    if ((m[2] === "==") !== (value === m[3])) return false;
  }
  const bare = text.match(/^!?\s*matrix\.([A-Za-z0-9_-]+)$/);
  if (bare) {
    const truthy = Boolean(leg[bare[1]]);
    return text.startsWith("!") ? !truthy : truthy;
  }
  return true;
}

const isWorker = (dir) => dir === WORKER_DIR || dir.startsWith(`${WORKER_DIR}/`);

/** Every worker cargo run of one parsed workflow: { where, subcommand, all, set }. */
export function workerRuns(workflow, file) {
  const found = [];
  const workflowDir = workflow?.defaults?.run?.["working-directory"];
  for (const [jobId, job] of Object.entries(workflow?.jobs ?? {})) {
    const jobDir = job?.defaults?.run?.["working-directory"];
    for (const leg of matrixLegs(job)) {
      for (const [index, step] of (job?.steps ?? []).entries()) {
        if (typeof step?.run !== "string" || !legRuns(step.if, leg)) continue;
        const dir = substitute(step["working-directory"] ?? jobDir ?? workflowDir ?? ".", leg);
        const env = Object.fromEntries(
          Object.entries({ ...workflow?.env, ...job?.env, ...step.env }).map(([k, v]) => [k, substitute(v, leg)]),
        );
        for (const run of cargoRuns(expandEnv(substitute(step.run, leg), env), dir.replace(/^\$\{\{\s*github\.workspace\s*\}\}\/?/, "."))) {
          if (!isWorker(run.dir)) continue;
          const legName = Object.keys(leg).length ? ` [${leg.name ?? JSON.stringify(leg)}]` : "";
          found.push({
            where: `${file} ${jobId}${legName} step ${step.name ?? index + 1}`,
            subcommand: run.subcommand,
            ...featureSet(run.args),
          });
        }
      }
    }
  }
  return found;
}

/** The worker runs of a helper that builds a Python argv list (`["cargo", "test", ...]`). */
export function helperRuns(text, file) {
  const found = [];
  for (const m of text.matchAll(/\[\s*"cargo"\s*,([^\]]*)\]/g)) {
    const argv = [...m[1].matchAll(/"([^"]*)"/g)].map((x) => x[1]);
    const subcommand = argv.shift();
    if (!SUBCOMMANDS.has(subcommand)) continue;
    found.push({ where: file, subcommand, ...featureSet(argv) });
  }
  return found;
}

export function check(imageSets, runs) {
  const errors = [];
  const label = (set) => (set === "" ? "the default set (no features)" : `--features ${set}`);
  const allowed = [...imageSets].map(label).join(" or ");
  const covered = new Map([...imageSets].map((s) => [s, { lint: false, test: false }]));
  for (const run of runs) {
    if (run.all) {
      errors.push(`${run.where}: cargo ${run.subcommand} --all-features on the worker; use the image's set: ${allowed}`);
      continue;
    }
    if (!imageSets.has(run.set)) {
      errors.push(`${run.where}: cargo ${run.subcommand} ${label(run.set)} on the worker, which no image builds; use ${allowed}`);
      continue;
    }
    if (LINT.has(run.subcommand)) covered.get(run.set).lint = true;
    if (TEST.has(run.subcommand)) covered.get(run.set).test = true;
  }
  for (const [set, { lint, test }] of covered) {
    if (!lint) errors.push(`no workflow runs cargo clippy on the worker with ${label(set)}, which an image builds`);
    if (!test) errors.push(`no workflow runs cargo test on the worker with ${label(set)}, which an image builds`);
  }
  return errors;
}

function main() {
  const errors = [];
  const { sets, errors: imageErrors } = imageFeatureSets(fs.readFileSync(path.join(ROOT, CONTAINERFILE), "utf8"));
  errors.push(...imageErrors);
  if (sets.size < 2) errors.push(`${CONTAINERFILE}: found ${sets.size} image feature set(s); the worker and the supervisor build two`);
  const runs = [];
  for (const name of fs.readdirSync(path.join(ROOT, WORKFLOWS)).sort()) {
    if (!/\.ya?ml$/.test(name)) continue;
    const file = `${WORKFLOWS}/${name}`;
    runs.push(...workerRuns(loadYaml(fs.readFileSync(path.join(ROOT, file), "utf8")), file));
  }
  const workflowRuns = runs.length;
  for (const file of HELPERS) runs.push(...helperRuns(fs.readFileSync(path.join(ROOT, file), "utf8"), file));
  if (workflowRuns < FLOOR) errors.push(`found ${workflowRuns} worker cargo run(s) in ${WORKFLOWS}, fewer than ${FLOOR}: the scan stopped seeing them`);
  errors.push(...check(sets, runs));
  if (errors.length) {
    for (const e of errors) console.error(`::error::${e}`);
    process.exit(1);
  }
  const shown = [...sets].map((s) => s || "(default)").join(", ");
  console.log(`worker feature sets: ${shown}; ${runs.length} worker cargo run(s) use only them, and each is linted and tested`);
}

if (isEntrypoint(import.meta.url)) main();
