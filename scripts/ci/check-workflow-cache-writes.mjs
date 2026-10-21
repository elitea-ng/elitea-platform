#!/usr/bin/env node
// Fail a workflow cache step that can WRITE from a pull request.
//
// THE RULE: pull requests read the Actions cache, only a push to main writes it.
//
// WHY (measured 2026-10-16). The repository's Actions cache is capped at 10 GB
// and evicted least-recently-used. It held 10.7 GB in 809 entries. The largest
// writers were pull-request refs: `setup-go` entries of 0.3-1.2 GB per pull
// request and ~2.9 GB of pull-request BuildKit blobs. A pull request can read
// main's entries, but its own writes are copies nothing else reads, and every
// one of them evicted something main had written. Main held no Go cache at all
// at the time, and a layer one run wrote was gone by the next; `ignore-error`
// on the BuildKit exports made every miss silent.
//
// WHAT IT CHECKS, in every .github/workflows/*.yml and .github/actions/*/action.yml:
//
//   actions/setup-go        `cache: false` (the cache is .github/actions/go-cache)
//   actions/setup-node      no `cache:` input, and `package-manager-cache: false`
//                           (the cache is .github/actions/npm-cache)
//   actions/setup-python    no `cache:` input
//   actions/cache           (the full action, which saves in a post step) and
//   actions/cache/save      the step's `if:` holds BOTH
//                           `github.ref == 'refs/heads/main'` and
//                           `github.event_name == 'push'`
//   Swatinem/rust-cache     `save-if:` names `github.ref == 'refs/heads/main'`,
//                           or is a literal false (restore only)
//   golangci-lint-action    `skip-cache: true`, or `skip-save-cache:` gated on
//                           a push to main
//   trivy-action            `cache: "false"` (its DB cache is a full actions/cache)
//   cache-to (BuildKit)     on docker/build-push-action, as a bake-action `set`
//                           line, or in a `docker buildx bake` run line: empty
//                           unless a push to main, i.e. an expression holding the
//                           main ref, `github.event_name == 'push'` and an
//                           `|| ''` fallback. A `docker buildx bake` run that
//                           builds (not `--print`) must clear `*.cache-to=`,
//                           because docker-bake.hcl declares mode=max exports.
//   build-stack-images      its `cache-write` default is a push to main, and no
//                           caller passes anything but "false"; `mount-cache-writer`
//                           is "true" or "false", "true" on at most ONE job per
//                           mount-cache-suffix across ALL workflows (the key is
//                           repository-wide), and never on every leg of a
//                           matrix job (one job, not N legs, saves the mounts:
//                           N writers raced for one key, measured 2026-10-20).
//                           A matrix job names its writer legs with
//                           `${{ matrix.<key> }}` over an include-only matrix
//                           whose entries give the literal; each "true" leg
//                           counts as one writer of that leg's suffix.
//   deploy/ci/*.hcl         declares no cache-to (it is merged into every bake)
//
// A workflow whose ONLY trigger is a push to main/next (publish.yml, the release)
// is exempt from the cache-to, actions/cache and trivy rules: it cannot run for
// a pull request. Every other rule still applies to it.
//
// This reads YAML with the `yaml` package the way PyYAML read it before the
// port (./ci-compat.mjs; `npm ci --prefix scripts/ci`), and runs with the
// runner's node. It fails, rather than passes, when it finds fewer workflows
// or cache steps than its floors: "found nothing, therefore clean" is how a
// guard stops guarding in silence.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";

import { loadYaml } from "./ci-compat.mjs";
import { pyRepr, pyStr, pyTruthy, splitlines } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const REPOSITORY_ROOT = path.resolve(path.dirname(SELF), "..", "..");

export const MINIMUM_WORKFLOWS = 10;
export const MINIMUM_CACHE_STEPS = 20;

const MAIN_REF = "github.ref == 'refs/heads/main'";
const PUSH_EVENT = "github.event_name == 'push'";
const RELEASE_BRANCHES = new Set(["main", "next"]);

const isDict = (v) => v !== null && typeof v === "object" && !Array.isArray(v) && !(v instanceof Date);
// dict.get(key, default)
const get = (obj, key, dflt = null) => (isDict(obj) && Object.hasOwn(obj, key) ? obj[key] : dflt);
// `x or {}`
const orDict = (v) => (pyTruthy(v) ? v : {});

/** Collapse whitespace so `github.ref=='x'` and spacing variants compare. */
function norm(value) {
  let text = value === null || value === undefined ? "" : pyStr(value);
  text = text.replace(/\s+/g, " ");
  text = text.replace(/\s*==\s*/g, " == ");
  text = text.replace(/\s*!=\s*/g, " != ");
  return text.replaceAll('"', "'");
}

const isFalse = (value) => value === false || pyStr(value).trim().toLowerCase() === "false";

function gatedOnMainPush(expr) {
  const text = norm(expr);
  return text.includes(MAIN_REF) && text.includes(PUSH_EVENT);
}

/** `!(main && push)` — the skip-side spelling of the same gate. */
function negatedMainPush(expr) {
  const text = norm(expr).replaceAll(" ", "");
  return gatedOnMainPush(expr) && text.includes("!(");
}

const MATRIX_REF = /^\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}$/;

/**
 * The legs of a matrix made of `include:` entries alone, else null.
 *
 * With any other axis, an include entry can merge into several legs, so a
 * per-entry value would not be a per-leg one.
 */
function includeOnlyLegs(job) {
  const matrix = get(orDict(get(job, "strategy")), "matrix");
  if (!isDict(matrix) || Object.keys(matrix).length !== 1 || !Object.hasOwn(matrix, "include")) return null;
  const include = matrix.include;
  if (!Array.isArray(include) || !include.every(isDict)) return null;
  return include;
}

/** A cache-to that is empty unless a push to main. */
function cacheToGated(value) {
  const text = norm(value).trim();
  if (text === "") return true;
  if (!text.includes("${{")) return false;
  return gatedOnMainPush(text) && /\|\|\s*''/.test(text);
}

function triggers(doc) {
  // PyYAML reads the unquoted `on:` key as True ("true" here).
  const on = Object.hasOwn(doc, "true") ? doc.true : get(doc, "on");
  if (typeof on === "string") return { [on]: null };
  if (Array.isArray(on)) return Object.fromEntries(on.map((k) => [pyStr(k), null]));
  return orDict(on);
}

function releaseOnly(doc) {
  const on = triggers(doc);
  const keys = Object.keys(on);
  if (keys.length !== 1 || keys[0] !== "push") return false;
  const push = orDict(on.push);
  const branches = new Set(pyTruthy(get(push, "branches")) ? get(push, "branches") : []);
  if (!branches.size && !pyTruthy(get(push, "tags"))) return false; // every branch
  return [...branches].every((b) => RELEASE_BRANCHES.has(b));
}

/** [job id, step] for a workflow; ["<action>", step] for a composite. */
function* steps(doc) {
  if (Object.hasOwn(doc, "runs")) {
    const list = get(orDict(doc.runs), "steps");
    for (const step of pyTruthy(list) ? list : []) yield ["<action>", step];
    return;
  }
  for (const [jobId, job] of Object.entries(orDict(get(doc, "jobs")))) {
    const list = get(orDict(job), "steps");
    for (const step of pyTruthy(list) ? list : []) yield [jobId, step];
  }
}

const uses = (step) => (pyTruthy(get(step, "uses")) ? pyStr(step.uses) : "");
const actionOf = (step) => uses(step).split("@", 1)[0];

export class Report {
  constructor() {
    this.errors = [];
    this.cacheSteps = 0;
    // mount-cache-suffix -> ["<file> [<job>]", ...], across every workflow.
    this.mountWriters = new Map();
    this.workflows = 0;
  }

  fail(where, message) {
    this.errors.push(`${where}: ${message}`);
  }

  writer(suffix, label) {
    if (!this.mountWriters.has(suffix)) this.mountWriters.set(suffix, []);
    this.mountWriters.get(suffix).push(label);
  }
}

export function checkDocument(name, doc, report) {
  const isAction = Object.hasOwn(doc, "runs");
  const release = isAction ? false : releaseOnly(doc);
  for (const [jobId, step] of steps(doc)) {
    if (!isDict(step)) continue;
    const action = actionOf(step);
    const with_ = orDict(get(step, "with"));
    const has = (key) => isDict(with_) && Object.hasOwn(with_, key);
    const label = pyTruthy(get(step, "name")) ? step.name : uses(step) || "run step";
    const where = `${name} [${jobId}] ${pyRepr(label)}`;
    const cond = get(step, "if");

    if (action === "actions/setup-go") {
      report.cacheSteps += 1;
      if (!isFalse(get(with_, "cache", true))) {
        report.fail(where, "actions/setup-go without `cache: false` saves its Go cache from every ref, pull requests included; use .github/actions/go-cache after it");
      }
    } else if (action === "actions/setup-node") {
      report.cacheSteps += 1;
      if (has("cache")) report.fail(where, "actions/setup-node `cache:` saves from every ref; drop it and use .github/actions/npm-cache");
      if (!isFalse(get(with_, "package-manager-cache", true))) {
        report.fail(where, "actions/setup-node needs `package-manager-cache: false`, or its automatic cache saves from every ref");
      }
    } else if (action === "actions/setup-python") {
      report.cacheSteps += 1;
      if (has("cache")) report.fail(where, "actions/setup-python `cache:` saves from every ref, pull requests included");
    } else if (action === "actions/cache" || action === "actions/cache/save") {
      report.cacheSteps += 1;
      if (!release && !gatedOnMainPush(cond)) {
        report.fail(where, `${action} saves the cache; its \`if:\` must require ${MAIN_REF} && ${PUSH_EVENT} (use actions/cache/restore to read)`);
      }
    } else if (action === "actions/cache/restore") {
      report.cacheSteps += 1;
    } else if (action === "Swatinem/rust-cache") {
      report.cacheSteps += 1;
      // A literal `false` (restore only) is the strongest form.
      if (!isFalse(get(with_, "save-if")) && !norm(get(with_, "save-if")).includes(MAIN_REF)) {
        report.fail(where, `Swatinem/rust-cache needs \`save-if: \${{ ${MAIN_REF} }}\`; without it every pull request saves its target directory`);
      }
    } else if (action === "golangci/golangci-lint-action") {
      report.cacheSteps += 1;
      const skipAll = pyStr(get(with_, "skip-cache", "")).trim().toLowerCase() === "true";
      if (!skipAll && !negatedMainPush(get(with_, "skip-save-cache"))) {
        report.fail(where, "golangci-lint-action saves its cache from every ref; set `skip-save-cache: ${{ !(github.ref == 'refs/heads/main' && github.event_name == 'push') }}`");
      }
    } else if (action === "aquasecurity/trivy-action") {
      report.cacheSteps += 1;
      if (!release && !isFalse(get(with_, "cache", true))) {
        report.fail(where, 'trivy-action caches its DB with a full actions/cache from every ref; set `cache: "false"` and restore it through .github/actions/main-write-cache');
      }
    } else if (action.endsWith("/build-stack-images")) {
      report.cacheSteps += 1;
      if (has("cache-write") && !isFalse(with_["cache-write"])) {
        report.fail(where, 'build-stack-images `cache-write` may only be omitted (a push to main writes) or "false"');
      }
      const writer = pyStr(get(with_, "mount-cache-writer", "false")).trim().toLowerCase();
      const job = orDict(get(orDict(get(doc, "jobs")), jobId));
      const legs = includeOnlyLegs(job);
      const writerRef = MATRIX_REF.exec(writer);
      if (writerRef && legs !== null) {
        // One writer per LEG, named in an include-only matrix: every
        // leg is exactly one include entry, so the literal each entry
        // gives is the whole truth about that leg. The suffix is read
        // per leg the same way, and each "true" leg counts as one
        // writer below, so two legs of one suffix are still refused.
        const writerKey = writerRef[1];
        const suffixRaw = pyStr(get(with_, "mount-cache-suffix", ""));
        const suffixKey = MATRIX_REF.exec(suffixRaw.trim());
        legs.forEach((leg, index) => {
          const value = pyStr(get(leg, writerKey, "")).trim().toLowerCase();
          if (value !== "true" && value !== "false") {
            report.fail(where, `matrix leg ${index} gives \`${writerKey}\` = ${pyRepr(get(leg, writerKey))}; each include entry must give the literal "true" or "false"`);
          } else if (value === "true") {
            const suffix = suffixKey ? pyStr(get(leg, suffixKey[1], "")) : suffixRaw;
            report.writer(suffix, `${name} [${jobId} leg ${index}]`);
          }
        });
      } else if (writer !== "true" && writer !== "false") {
        report.fail(where, `build-stack-images \`mount-cache-writer\` must be the literal "true" or "false", or \`\${{ matrix.<key> }}\` of an include-only matrix whose entries give the literal; got ${pyRepr(get(with_, "mount-cache-writer"))}; an expression can make every leg a writer`);
      } else if (writer === "true") {
        if (pyTruthy(get(orDict(get(job, "strategy")), "matrix"))) {
          report.fail(where, 'build-stack-images `mount-cache-writer: "true"` on a matrix job makes every leg a writer racing for one cache key; designate one non-matrix job, or one leg through an include-only matrix');
        }
        const suffix = pyStr(get(with_, "mount-cache-suffix", ""));
        report.writer(suffix, `${name} [${jobId}]`);
      }
    }

    // BuildKit layer exports.
    if (!release) {
      if (has("cache-to")) {
        report.cacheSteps += 1;
        if (!cacheToGated(with_["cache-to"])) {
          report.fail(where, `cache-to must be empty unless a push to main (\`\${{ ${MAIN_REF} && ${PUSH_EVENT} && '<spec>' || '' }}\`); got ${pyRepr(with_["cache-to"])}`);
        }
      }
      if (action.startsWith("docker/bake-action")) {
        const files = pyTruthy(get(with_, "files")) ? pyStr(with_.files) : "";
        const sets = splitlines(pyTruthy(get(with_, "set")) ? pyStr(with_.set) : "").map((l) => l.trim()).filter(Boolean);
        const cacheSets = sets.filter((s) => /^[^=\s]+\.cache-to=/.test(s));
        for (const s of cacheSets) {
          report.cacheSteps += 1;
          if (!cacheToGated(s.slice(s.indexOf("=") + 1))) {
            report.fail(where, `bake \`set\` ${pyRepr(s)} writes the layer cache from every run; make it empty unless a push to main`);
          }
        }
        if (files.includes("docker-bake.hcl")) {
          const targets = (pyTruthy(get(with_, "targets")) ? pyStr(with_.targets) : "").split(/\s+/).filter(Boolean);
          const coveredTargets = new Set(cacheSets.map((s) => s.split(".cache-to=", 1)[0]));
          for (const t of targets) {
            if (!coveredTargets.has(t) && !coveredTargets.has("*")) {
              report.fail(where, `bake target ${pyRepr(t)} from docker-bake.hcl keeps its mode=max cache-to; override \`${t}.cache-to=\` gated on a push to main`);
            }
          }
        }
      }
      const run = pyTruthy(get(step, "run")) ? pyStr(step.run) : "";
      for (const line of splitlines(run)) {
        if (/\bbuildx\s+bake\b/.test(line) && !line.includes("--print")) {
          report.cacheSteps += 1;
          if (!/--set[= ]['"]?\*\.cache-to=['"]?(\s|$)/.test(line)) {
            report.fail(where, "a `docker buildx bake` run inherits docker-bake.hcl's mode=max cache-to; add `--set '*.cache-to='`");
          }
          break;
        }
      }
    }
  }

  // The composite action's own write default.
  if (isAction && name.endsWith("build-stack-images/action.yml")) {
    const dflt = get(orDict(get(orDict(get(doc, "inputs")), "cache-write")), "default");
    if (!gatedOnMainPush(dflt)) {
      report.fail(name, `the \`cache-write\` default must be ${MAIN_REF} && ${PUSH_EVENT}; got ${pyRepr(dflt)}`);
    }
  }
}

export function checkHcl(name, text, report) {
  splitlines(text).forEach((line, index) => {
    if (/^\s*cache-to\s*=/.test(line)) {
      report.fail(`${name}:${index + 1}`, "deploy/ci HCL is merged into every CI bake, so a cache-to here writes from pull requests; the write side belongs to build-stack-images");
    }
  });
}

const byName = (a, b) => (a < b ? -1 : a > b ? 1 : 0);

function listFiles(dir, suffix) {
  try {
    return fs
      .readdirSync(dir)
      .filter((n) => n.endsWith(suffix))
      .sort(byName)
      .map((n) => path.join(dir, n));
  } catch {
    return [];
  }
}

function listActions(dir, file) {
  try {
    return fs
      .readdirSync(dir)
      .sort(byName)
      .map((n) => path.join(dir, n, file))
      .filter((p) => fs.existsSync(p));
  } catch {
    return [];
  }
}

export function run(root) {
  const report = new Report();
  const workflowsDir = path.join(root, ".github", "workflows");
  const actionsDir = path.join(root, ".github", "actions");
  const workflows = [...listFiles(workflowsDir, ".yml"), ...listFiles(workflowsDir, ".yaml")];
  const actions = [...listActions(actionsDir, "action.yml"), ...listActions(actionsDir, "action.yaml")];
  for (const file of [...workflows, ...actions]) {
    const rel = path.relative(root, file);
    const doc = loadYaml(fs.readFileSync(file, "utf8"));
    checkDocument(rel, pyTruthy(doc) ? doc : {}, report);
  }
  // The mount-cache key is buildkit-mounts-v3-<os>-<suffix>-…: one key space
  // per suffix for the whole REPOSITORY, so two writers in two different
  // workflows race exactly as two in one workflow did.
  for (const suffix of [...report.mountWriters.keys()].sort(byName)) {
    const jobs = report.mountWriters.get(suffix);
    if (jobs.length > 1) {
      report.fail("build-stack-images", `${jobs.length} jobs (${jobs.join(", ")}) pass mount-cache-writer: "true" for mount-cache-suffix ${pyRepr(suffix)}; exactly one job in the repository may save those mounts`);
    }
  }
  for (const file of listFiles(path.join(root, "deploy", "ci"), ".hcl")) {
    checkHcl(path.relative(root, file), fs.readFileSync(file, "utf8"), report);
  }
  report.workflows = workflows.length;
  return report;
}

const PROG = "check-workflow-cache-writes.mjs";
const USAGE = `usage: ${PROG} [-h] [--root ROOT] [--min-workflows MIN_WORKFLOWS] [--min-cache-steps MIN_CACHE_STEPS]`;
class UsageError extends Error {}

/** An option's integer value, as argparse's `type=int` read it. */
function intOption(name, value) {
  if (!/^\s*[-+]?\d+\s*$/.test(value)) throw new UsageError(`argument ${name}: invalid int value: ${pyRepr(value)}`);
  return Number.parseInt(value, 10);
}

/** The command line, read by node:util parseArgs: `--x v` or `--x=v`. */
export function parseArguments(argv) {
  let values;
  try {
    ({ values } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: false,
      options: {
        help: { type: "boolean", short: "h" },
        root: { type: "string" },
        "min-workflows": { type: "string" },
        "min-cache-steps": { type: "string" },
      },
    }));
  } catch (error) {
    if (typeof error?.code === "string" && error.code.startsWith("ERR_PARSE_ARGS_")) throw new UsageError(error.message);
    throw error;
  }
  return {
    root: values.root ?? REPOSITORY_ROOT,
    minWorkflows: values["min-workflows"] === undefined ? MINIMUM_WORKFLOWS : intOption("--min-workflows", values["min-workflows"]),
    minCacheSteps: values["min-cache-steps"] === undefined ? MINIMUM_CACHE_STEPS : intOption("--min-cache-steps", values["min-cache-steps"]),
    help: values.help === true,
  };
}

export function main(argv = process.argv.slice(2), log = console.log, err = console.error) {
  let args;
  try {
    args = parseArguments(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    err(`${USAGE}\n${PROG}: error: ${error.message}`);
    return 2;
  }
  if (args.help) {
    log(`${USAGE}\n\nFail a workflow cache step that can WRITE from a pull request.`);
    return 0;
  }
  const report = run(path.resolve(args.root));
  if (report.workflows < args.minWorkflows) {
    log(`::error::found ${report.workflows} workflow file(s), expected at least ${args.minWorkflows} — the glob stopped matching, so this check read nothing`);
    return 1;
  }
  if (report.cacheSteps < args.minCacheSteps) {
    log(`::error::found ${report.cacheSteps} cache step(s), expected at least ${args.minCacheSteps} — the step matching stopped working, so this check measured nothing`);
    return 1;
  }
  for (const error of report.errors) log(`::error::${error}`);
  if (report.errors.length) {
    log(`${report.errors.length} cache step(s) can write from a pull request`);
    return 1;
  }
  log(`workflow caches: ${report.cacheSteps} cache step(s) in ${report.workflows} workflow(s) and the composite actions; none writes from a pull request`);
  return 0;
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === SELF) {
  process.exitCode = main();
}
