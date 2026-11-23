#!/usr/bin/env node
// The Actions cache plan fits its cap, and every cache a pull request reads has a writer.
//
// MEASURED 2026-11-19: the cache held 11.2 GiB against a 10 GB cap, and 61 of 61
// sampled Rust jobs found no cache. Entries too big to keep (one Windows job
// held 2.8 GiB) evicted the ones every pull request restores. MEASURED 2026-11-20:
// a registry-only rust-cache entry is 0.3-0.6 GiB, not the 0.1 planned, so 24
// of them alone were ~9 GiB; they are gone, and the sizes in cache-budget.json
// are measured (the nightly janitor warns when the plan drifts from the cache).
//
// 1. BUDGET. The planned entries are DERIVED from the workflows (every rust-cache,
//    go-cache and npm-cache step that saves on main, times its matrix legs) with
//    the size estimates of scripts/ci/cache-budget.json, plus its fixed entries.
//    Their sum must stay under BUDGET_GIB (8: the cap is 10 GB and the janitor trims at 9 GiB), so LRU never has to evict the hot
//    set. A new cache step cannot escape: it is counted the day it is written.
// 2. WRITER = READER. Pull requests only restore (check-workflow-cache-writes.mjs).
//    The WARM run is the same job on the nightly schedule of main: it computes
//    the same key from the same prefix-key, shared-key, workspaces and
//    toolchain, because it is the same step. So the check is that every
//    Swatinem/rust-cache step that saves on main sits in a workflow that has a
//    `schedule` trigger: a restore-only cache with no scheduled writer is cold
//    for ever (the ones that were never written).
// 3. NO TWO STEPS SHARE A KEY. Two steps with one (prefix-key, shared-key,
//    workspaces) restore and re-save each other's target directory.
// 4. A READER HAS A WRITER. A restore-only rust-cache step with a shared-key (it
//    reads another job's entry: desktop-package.yml's pull-request dry run reads
//    ci-desktop.yml's macOS host) must compute the key a saving step in a
//    scheduled workflow computes: the same prefix-key, shared-key, workspaces and
//    cache-targets, and the same CARGO_*/RUST* environment (rust-cache hashes it
//    into the key). Anything else is a cache that is never there.
// 5. ONE WRITER PER CACHE KEY (rust-cache and go-cache; npm-cache is exempt, its
//    entry is the lockfile's tarballs whoever saves it). An exact hit never
//    re-saves, so the first writer to finish decides what every pull request
//    restores: on 2026-11-20 a small job saved main's Go key at 0.39 GiB before
//    ci-go's 1.3 GiB build ran. Every go-cache reader has that one writer.
// 6. NO REGISTRY-ONLY RUST CACHE. A target directory or nothing.
// 7. A WORKFLOW'S OWN actions/cache IS COUNTED TOO (2026-11-23: cargo's git
//    database, ci-rust-shared.yml). Every actions/cache or actions/cache/save
//    step of a workflow that saves on main is an entry of its key family (the
//    literal start of `key:`), sized from cache-budget.json, and a family no
//    size covers is an error, not a zero. A key that is all expression
//    (ci-go.yml's save of the go-cache key) is the cache action's entry,
//    counted there. Every actions/cache/restore of a workflow reads a family
//    some saving step of a scheduled workflow writes.
// 8. THE PLAN IS THE CACHE. The janitor (ci-image-cleanup.yml) compares main's
//    entries with this plan every night, after its prune: a drift of more than
//    1 GiB warns (update the measured sizes), and main's entries over
//    BUDGET_GIB FAIL the job. That is an overrun, not a state: on 2026-11-23
//    main held 8.33 GiB against a plan of 7.71, and nothing had said so.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { loadYaml, isEntrypoint } from "./ci-compat.mjs";
import { familyOf } from "./prune-actions-caches.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
// 8, not the janitor's 9: one gibibyte of room for what the plan cannot see (a
// key family that changed and is not pruned yet, a pull request's entries before
// the janitor removes them).
export const BUDGET_GIB = 8;

const onOf = (doc) => (Object.hasOwn(doc, "true") ? doc.true : doc.on);
const triggerNames = (on) => (typeof on === "string" ? [on] : Array.isArray(on) ? on : Object.keys(on ?? {}));

/** The environment rust-cache hashes into its key (by these prefixes), from the workflow and the job. */
const KEYED_ENV = /^(CARGO|RUST|CC|CFLAGS|CXX|CMAKE)/;
const keyedEnv = (doc, def) => Object.entries({ ...(doc.env ?? {}), ...(def.env ?? {}) }).filter(([k]) => KEYED_ENV.test(k)).map(([k, v]) => `${k}=${v}`).sort();
const cachesTargets = (w) => !(w["cache-targets"] === false || w["cache-targets"] === "false");

/** Every rust-cache step: {workflow, job, hasSchedule, saves, restoreOnly, key, full, shared}. */
function allRustCacheSteps(workflows) {
  const out = [];
  for (const [file, doc] of Object.entries(workflows)) {
    const hasSchedule = triggerNames(onOf(doc) ?? {}).includes("schedule");
    for (const [job, def] of Object.entries(doc.jobs ?? {})) {
      for (const step of def.steps ?? []) {
        if (!String(step.uses ?? "").startsWith("Swatinem/rust-cache")) continue;
        const w = step.with ?? {};
        const workspaces = String(w.workspaces ?? ".").split("\n").map((s) => s.trim()).filter(Boolean);
        const key = JSON.stringify([w["prefix-key"] ?? "v0-rust", w["shared-key"] ?? `job:${file}:${job}`, workspaces]);
        out.push({
          workflow: file,
          job,
          legs: Array.isArray(def.strategy?.matrix?.include) ? def.strategy.matrix.include.length : 1,
          targets: cachesTargets(w),
          hasSchedule,
          saves: String(w["save-if"] ?? "").includes("refs/heads/main"),
          restoreOnly: w["save-if"] === false || w["save-if"] === "false",
          key,
          // What a reader must match for its restore to hit: the key, the cached paths, the hashed environment.
          full: JSON.stringify([key, cachesTargets(w), keyedEnv(doc, def)]),
          shared: w["shared-key"] !== undefined,
        });
      }
    }
  }
  return out;
}

/** [{workflow, job, hasSchedule, key, ...}] for every rust-cache step that saves on main. */
export function rustCacheSteps(workflows) {
  return allRustCacheSteps(workflows).filter((s) => s.saves);
}

/** Restore-only rust-cache steps with a shared-key whose entry no scheduled writer saves. */
export function orphanReaders(workflows) {
  const steps = allRustCacheSteps(workflows);
  const written = new Set(steps.filter((s) => s.saves && s.hasSchedule).map((s) => s.full));
  return steps.filter((s) => s.restoreOnly && s.shared && !written.has(s.full));
}

const isGoCache = (uses) => uses.endsWith("/go-cache") || uses.includes("actions/go-cache");
const legsOf = (def) => (Array.isArray(def.strategy?.matrix?.include) ? def.strategy.matrix.include.length : 1);
const goVersionOf = (def) => {
  const setup = (def.steps ?? []).find((s) => String(s.uses ?? "").startsWith("actions/setup-go"));
  const w = setup?.with ?? {};
  return w["go-version-file"] ? `file:${w["go-version-file"]}` : `version:${w["go-version"] ?? "?"}`;
};

/**
 * Every go-cache step: {workflow, job, legs, key, writer, hasSchedule}. The key is
 * what .github/actions/go-cache keys on and the workflow can name: the go.sum list
 * and where setup-go read the Go version. A step writes when it does not say
 * `save: "false"`, or when a later `actions/cache/save` of its job saves its
 * `steps.<id>.outputs.key` (ci-go.yml test-checks saves after a red gate too).
 */
export function goCacheSteps(workflows) {
  const out = [];
  for (const [file, doc] of Object.entries(workflows)) {
    const hasSchedule = triggerNames(onOf(doc) ?? {}).includes("schedule");
    for (const [job, def] of Object.entries(doc.jobs ?? {})) {
      const steps = def.steps ?? [];
      const savedIds = new Set(
        steps
          .filter((s) => String(s.uses ?? "").startsWith("actions/cache/save"))
          .map((s) => /steps\.([A-Za-z0-9_-]+)\.outputs\.key/.exec(String(s.with?.key ?? ""))?.[1])
          .filter(Boolean),
      );
      for (const step of steps) {
        if (!isGoCache(String(step.uses ?? ""))) continue;
        const w = step.with ?? {};
        const deps = String(w["cache-dependency-path"] ?? "");
        if (deps.includes("${{")) continue; // an expression (codeql's matrix): its key is not knowable here
        const key = `go-cache ${deps.replace(/\s+/g, " ").trim()} [${goVersionOf(def)}]`;
        const writer = String(w.save ?? "true") !== "false" || (step.id !== undefined && savedIds.has(step.id));
        out.push({ workflow: file, job, legs: legsOf(def), key, writer, hasSchedule });
      }
    }
  }
  return out;
}

/** The planned cache entries, DERIVED from the workflows: every rust-cache, go-cache and npm-cache step that saves on main. */
/**
 * Every actions/cache, actions/cache/save and actions/cache/restore step of a
 * workflow whose key starts with a literal: {workflow, job, family, saves,
 * restores, hasSchedule}. The family is that literal start (what a
 * restore-keys prefix matches on).
 */
export function actionsCacheSteps(workflows) {
  const out = [];
  for (const [file, doc] of Object.entries(workflows)) {
    const hasSchedule = triggerNames(onOf(doc) ?? {}).includes("schedule");
    for (const [job, def] of Object.entries(doc.jobs ?? {})) {
      for (const step of def.steps ?? []) {
        const uses = String(step.uses ?? "");
        const m = /^actions\/cache(\/save|\/restore)?@/.exec(uses);
        if (!m) continue;
        const family = String(step.with?.key ?? "").split("${{")[0].trim();
        if (!family) continue; // all expression: the cache action whose output it is counts it
        const iff = String(step.if ?? "");
        const saves = m[1] !== "/restore" && iff.includes("refs/heads/main");
        out.push({ workflow: file, job, family, saves, restores: m[1] !== "/save", hasSchedule });
      }
    }
  }
  return out;
}

/** The planned size of one entry: projected (changed, not measured yet), then measured, then the estimate. */
const sizeOf = (budget, key, estimate) => budget.projected?.[key] ?? budget.measured?.[key] ?? estimate;

export function plannedEntries(workflows, budget) {
  const est = budget.estimates;
  const entries = [];
  for (const s of rustCacheSteps(workflows)) {
    const each = sizeOf(budget, `${s.workflow}:${s.job}`, s.targets ? est.rustWithTargets : est.rustRegistryOnly);
    entries.push({ what: `${s.workflow}:${s.job} (rust-cache x${s.legs})`, gib: each * s.legs });
  }
  const goKeys = new Set();
  for (const s of goCacheSteps(workflows)) {
    if (!s.writer || goKeys.has(s.key)) continue;
    goKeys.add(s.key);
    entries.push({ what: s.key, gib: sizeOf(budget, s.key, est.go) });
  }
  const families = new Set();
  for (const s of actionsCacheSteps(workflows)) {
    if (!s.saves || families.has(s.family)) continue;
    families.add(s.family);
    // Undefined when unsized: check() names it rather than counting it as nothing.
    entries.push({ what: `actions/cache ${s.family}`, gib: sizeOf(budget, `actions/cache ${s.family}`, undefined) });
  }
  const npmPaths = new Set();
  for (const doc of Object.values(workflows)) {
    for (const def of Object.values(doc.jobs ?? {})) {
      for (const step of def.steps ?? []) {
        const uses = String(step.uses ?? "");
        if (!(uses.endsWith("/npm-cache") || uses.includes("actions/npm-cache"))) continue;
        const key = String(step.with?.["cache-dependency-path"] ?? "");
        if (!npmPaths.has(key)) { npmPaths.add(key); entries.push({ what: `npm-cache ${key}`, gib: sizeOf(budget, `npm-cache ${key}`, est.npm) }); }
      }
    }
  }
  for (const f of budget.fixed ?? []) entries.push({ what: f.family, gib: Number(f.gib) });
  return entries;
}

export function check({ budget, workflows }) {
  const errors = [];
  const entries = plannedEntries(workflows, budget);
  for (const e of entries.filter((x) => typeof x.gib !== "number" || Number.isNaN(x.gib))) {
    errors.push(`${e.what}: no size in scripts/ci/cache-budget.json (add it under "projected" until a nightly measures it, then under "measured")`);
    e.gib = 0;
  }
  const total = entries.reduce((s, e) => s + e.gib, 0);
  if (entries.length < 8) errors.push(`only ${entries.length} planned cache entries derived: the reader stopped finding steps`);
  const ac = actionsCacheSteps(workflows);
  const scheduledFamilies = new Set(ac.filter((s) => s.saves && s.hasSchedule).map((s) => s.family));
  for (const s of ac) {
    if (s.saves && !s.hasSchedule) errors.push(`${s.workflow} (${s.job}): an actions/cache step saves ${s.family}… on main in a workflow with no schedule, so nothing ever writes it`);
    if (s.restores && !s.saves && !scheduledFamilies.has(s.family)) errors.push(`${s.workflow} (${s.job}): restores ${s.family}…, which no saving step of a scheduled workflow writes, so it never restores`);
  }
  if (total > BUDGET_GIB) {
    const top = [...entries].sort((a, b) => b.gib - a.gib).slice(0, 5).map((e) => `${e.what} ${e.gib.toFixed(1)}`).join("; ");
    errors.push(`the planned cache entries sum to ${total.toFixed(2)} GiB, over the ${BUDGET_GIB} GiB budget (largest: ${top})`);
  }

  const rust = rustCacheSteps(workflows);
  if (rust.length < 2) errors.push(`found ${rust.length} rust-cache step(s) that save on main, expected at least 2 (the worker tests and the macOS desktop host)`);
  for (const s of rust) {
    if (!s.hasSchedule) errors.push(`${s.workflow} (${s.job}): the rust-cache step saves on main but the workflow has no schedule: nothing ever writes it, so every pull request is cold`);
    if (!s.targets) errors.push(`${s.workflow} (${s.job}): a registry-only rust-cache entry (cache-targets: false) is 0.3-0.6 GiB of the cap for the seconds cargo takes to fetch the crates; cache the target or nothing`);
  }
  for (const r of orphanReaders(workflows)) {
    errors.push(`${r.workflow} (${r.job}): a restore-only rust-cache step reads ${r.key}, but no saving step of a scheduled workflow has that key with the same cache-targets and CARGO_*/RUST* environment, so it never restores`);
  }

  // ONE WRITER PER CACHE KEY. An exact hit never re-saves, so with two writers
  // the first to finish decides what every pull request restores, and a small
  // job wins (main's Go key, 2026-11-20). Matrix legs count one each.
  const writers = new Map();
  const note = (key, who, legs) => writers.set(key, [...(writers.get(key) ?? []), ...Array.from({ length: legs }, () => who)]);
  for (const s of rust) note(`rust-cache ${s.key}`, `${s.workflow} (${s.job})`, s.legs);
  const go = goCacheSteps(workflows);
  for (const s of go) if (s.writer) note(s.key, `${s.workflow} (${s.job})`, s.legs);
  for (const [key, who] of writers) {
    if (who.length > 1) errors.push(`${key}: ${who.length} writers (${who.join(", ")}); exactly one job saves a key, the others restore with \`save: "false"\` (or \`save-if: false\`)`);
  }
  for (const s of go) {
    const w = go.filter((x) => x.key === s.key && x.writer);
    if (w.length === 0) errors.push(`${s.workflow} (${s.job}): reads ${s.key}, which no job saves, so it never restores`);
    else if (s.writer && !s.hasSchedule) errors.push(`${s.workflow} (${s.job}): the one writer of ${s.key} is in a workflow with no schedule, so main never writes it`);
  }
  return { errors, total, entries };
}

/**
 * Drift between the plan and the cache as it is (the nightly janitor runs this
 * after its prune, with `gh api actions/caches`). A plan that drifts more than
 * the tolerance from the cache is a warning (the measured sizes need
 * updating); main's entries over the budget are an `overrun`, which fails
 * the janitor: the one real failure here.
 * @param {{key:string, ref:string, size_in_bytes:number}[]} caches
 */
export function drift({ caches, entries, budgetGib = BUDGET_GIB, toleranceGib = 1 }) {
  const gibOf = (b) => b / 1024 ** 3;
  const main = caches.filter((c) => c.ref === "refs/heads/main");
  const actual = gibOf(main.reduce((s, c) => s + c.size_in_bytes, 0));
  const planned = entries.reduce((s, e) => s + e.gib, 0);
  const warnings = [];
  const overrun = actual > budgetGib ? `main's cache entries hold ${actual.toFixed(2)} GiB, over the ${budgetGib} GiB budget (planned ${planned.toFixed(2)}): cut an entry (docs/contributing/ci.md, "The cache plan")` : null;
  if (Math.abs(actual - planned) > toleranceGib) warnings.push(`main's cache entries hold ${actual.toFixed(2)} GiB but scripts/ci/cache-budget.json plans ${planned.toFixed(2)}: update its measured sizes`);
  const largest = [...main].sort((a, b) => b.size_in_bytes - a.size_in_bytes).slice(0, 12).map((c) => `${gibOf(c.size_in_bytes).toFixed(2)} ${c.key}`);
  const byFamily = new Map();
  for (const c of main) {
    const f = familyOf(c.key);
    const [n, b] = byFamily.get(f) ?? [0, 0];
    byFamily.set(f, [n + 1, b + c.size_in_bytes]);
  }
  const families = [...byFamily].sort((a, b) => b[1][1] - a[1][1]).map(([f, [n, b]]) => `${gibOf(b).toFixed(3)} ${f} (${n})`);
  return { actual, planned, warnings, overrun, largest, families };
}

function loadWorkflows(root) {
  const dir = path.join(root, ".github", "workflows");
  const workflows = {};
  for (const n of fs.readdirSync(dir).filter((x) => x.endsWith(".yml"))) workflows[n] = loadYaml(fs.readFileSync(path.join(dir, n), "utf8")) ?? {};
  const budget = JSON.parse(fs.readFileSync(path.join(root, "scripts", "ci", "cache-budget.json"), "utf8"));
  return { workflows, budget };
}

/** For the janitor (ci-image-cleanup.yml, actions/github-script): the plan against the cache list. */
export async function reportDrift({ github, owner, repo, root = ROOT, log = console.log, warn = console.warn }) {
  const caches = await github.paginate(github.rest.actions.getActionsCacheList, { owner, repo, per_page: 100 });
  const { workflows, budget } = loadWorkflows(root);
  const { entries } = check({ budget, workflows });
  const r = drift({ caches, entries });
  log(`cache plan ${r.planned.toFixed(2)} GiB, main holds ${r.actual.toFixed(2)} GiB; by key family (GiB, entries):`);
  for (const l of r.families) log(`  ${l}`);
  log("largest entries:");
  for (const l of r.largest) log(`  ${l}`);
  for (const w of r.warnings) warn(w);
  return r;
}

export function main({ root = ROOT, out = console.log, err = console.error } = {}) {
  const { workflows, budget } = loadWorkflows(root);
  const { errors, total } = check({ budget, workflows });
  for (const e of errors) err(`::error::${e}`);
  if (errors.length) return 1;
  out(`cache budget: ${total.toFixed(2)} GiB planned of ${BUDGET_GIB}, derived from the workflows; one writer per key, each on a schedule`);
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
