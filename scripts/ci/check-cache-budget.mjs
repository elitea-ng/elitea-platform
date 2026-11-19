#!/usr/bin/env node
// The Actions cache plan fits its cap, and every cache a pull request reads has a writer.
//
// MEASURED 2026-11-19: the cache held 11.2 GiB against a 10 GB cap, and 61 of 61
// sampled Rust jobs found no cache. Entries too big to keep (one Windows job
// held 2.8 GiB) evicted the ones every pull request restores.
//
// 1. BUDGET. scripts/ci/cache-budget.json lists the planned entries with their
//    measured size in GiB. Their sum must stay under BUDGET_GIB (8), so LRU never
//    has to evict the hot set. A new cache step is a new line there.
// 2. WRITER = READER. Pull requests only restore (check-workflow-cache-writes.mjs).
//    The WARM run is the same job on the nightly schedule of main: it computes
//    the same key from the same prefix-key, shared-key, workspaces and
//    toolchain, because it is the same step. So the check is that every
//    Swatinem/rust-cache step that saves on main sits in a workflow that has a
//    `schedule` trigger: a restore-only cache with no scheduled writer is cold
//    for ever (the ones that were never written).
// 3. NO TWO STEPS SHARE A KEY. Two steps with one (prefix-key, shared-key,
//    workspaces) restore and re-save each other's target directory.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { loadYaml, isEntrypoint } from "./ci-compat.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");
export const BUDGET_GIB = 8;

const onOf = (doc) => (Object.hasOwn(doc, "true") ? doc.true : doc.on);
const triggerNames = (on) => (typeof on === "string" ? [on] : Array.isArray(on) ? on : Object.keys(on ?? {}));

/** [{workflow, job, step, hasSchedule, key}] for every rust-cache step that saves on main. */
export function rustCacheSteps(workflows) {
  const out = [];
  for (const [file, doc] of Object.entries(workflows)) {
    const hasSchedule = triggerNames(onOf(doc) ?? {}).includes("schedule");
    for (const [job, def] of Object.entries(doc.jobs ?? {})) {
      for (const step of def.steps ?? []) {
        if (!String(step.uses ?? "").startsWith("Swatinem/rust-cache")) continue;
        const w = step.with ?? {};
        if (!String(w["save-if"] ?? "").includes("refs/heads/main")) continue;
        const key = JSON.stringify([w["prefix-key"] ?? "v0-rust", w["shared-key"] ?? `job:${file}:${job}`, String(w.workspaces ?? ".").split("\n").map((s) => s.trim()).filter(Boolean)]);
        out.push({ workflow: file, job, hasSchedule, key, shared: w["shared-key"] !== undefined });
      }
    }
  }
  return out;
}

export function check({ budget, workflows }) {
  const errors = [];
  const total = (budget.entries ?? []).reduce((s, e) => s + Number(e.gib), 0);
  if (!(budget.entries?.length >= 5)) errors.push("cache-budget.json lists fewer than 5 entries: the plan is empty");
  if (total > BUDGET_GIB) errors.push(`the planned cache entries sum to ${total.toFixed(2)} GiB, over the ${BUDGET_GIB} GiB budget`);
  const steps = rustCacheSteps(workflows);
  if (steps.length < 5) errors.push(`found ${steps.length} rust-cache step(s) that save on main, expected at least 5`);
  const seen = new Map();
  for (const s of steps) {
    if (!s.hasSchedule) errors.push(`${s.workflow} (${s.job}): the rust-cache step saves on main but the workflow has no schedule: nothing ever writes it, so every pull request is cold`);
    // Job-id keys are per job by construction; only explicit shared keys can collide.
    if (s.shared) {
      if (seen.has(s.key) && seen.get(s.key) !== `${s.workflow}:${s.job}`) errors.push(`${s.workflow} (${s.job}) and ${seen.get(s.key)} use the same rust-cache key ${s.key}`);
      seen.set(s.key, `${s.workflow}:${s.job}`);
    }
  }
  return errors;
}

export function main({ root = ROOT, out = console.log, err = console.error } = {}) {
  const dir = path.join(root, ".github", "workflows");
  const workflows = {};
  for (const n of fs.readdirSync(dir).filter((x) => x.endsWith(".yml"))) workflows[n] = loadYaml(fs.readFileSync(path.join(dir, n), "utf8")) ?? {};
  const budget = JSON.parse(fs.readFileSync(path.join(root, "scripts", "ci", "cache-budget.json"), "utf8"));
  const errors = check({ budget, workflows });
  for (const e of errors) err(`::error::${e}`);
  if (errors.length) return 1;
  out(`cache budget: ${budget.entries.reduce((s, e) => s + Number(e.gib), 0).toFixed(2)} GiB planned of ${BUDGET_GIB}; every saving rust-cache step has a scheduled writer`);
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
