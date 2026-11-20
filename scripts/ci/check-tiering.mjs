#!/usr/bin/env node
// The CI tiers are complete and every job is in the verdict.
//
// .github/workflows/ci.yml runs the pull-request pipeline in two tiers (fast:
// every push; heavy: only a ready PR whose fast tier passed) and ends in
// `ci-gate`, the one job a required status check can point at. That only holds
// while three lists stay equal, and a list somebody has to keep complete is
// how a check goes green by absence. This fails when:
//
//   * a heavy job (one that needs `fast-gate`) has no `github.event.pull_request.draft == false`
//     in its `if:` — it would run for a draft, which is what the tiers exist to prevent;
//   * a fast job needs `fast-gate`, or `fast-gate` does not list every fast job;
//   * `ci-gate` or `fast-gate` does not run with `if: always()`, or `ci-gate`
//     does not list every other job of the workflow (a job missing from `needs`
//     cannot fail the verdict);
//   * a job that calls a workflow has no entry (tier and paths) in
//     scripts/ci/tier-paths.json, or an entry has no job, or the tier in the file
//     disagrees with the job's `needs`;
//   * a workflow ci.yml calls has no `workflow_call` arm or still starts on its
//     own `pull_request`, or a workflow outside the allowlist starts on a pull
//     request without being called by ci.yml (a new workflow would run untiered);
//   * a called workflow's concurrency group is built from `github.workflow` (in a
//     called workflow that is the CALLER's name, so every called workflow shares
//     one group and they cancel each other).
//
// Reads YAML with the `yaml` package (./ci-compat.mjs; `npm ci --prefix scripts/ci`)
// and runs with the runner's node. It fails when it finds fewer jobs than its
// floor: "found nothing, therefore clean" is how a guard stops guarding.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { loadYaml, isEntrypoint } from "./ci-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SELF), "..", "..");

export const MINIMUM_CALL_JOBS = 20;
/** Workflows that keep their own pull-request trigger, and why. */
export const OWN_PR_TRIGGER = new Map([
  ["ci.yml", "the orchestrator itself"],
  ["mirror-buildkit.yml", "starts only when its own file changes; its `packages: write` is not a gate"],
  ["cancel-merged-pr-runs.yml", "housekeeping on close, not a check"],
]);
export const DRAFT_CONDITION = "github.event.pull_request.draft == false";
// A heavy job's own if must not lean on the implicit success(): that is false when ANY
// ancestor was skipped, and fast-tier jobs a path filter (or a reused ready run) skipped
// are ancestors through fast-gate, so every heavy job would be skipped. It asks for
// fast-gate's result itself instead.
export const FAST_GATE_CONDITION = "!cancelled() && needs.fast-gate.result == 'success'";

const asList = (needs) => (needs === undefined ? [] : Array.isArray(needs) ? needs : [needs]);
const triggers = (workflow) => {
  const on = Object.hasOwn(workflow, "true") ? workflow.true : workflow.on;
  if (typeof on === "string") return { [on]: null };
  if (Array.isArray(on)) return Object.fromEntries(on.map((e) => [e, null]));
  return on ?? {};
};

/**
 * @param {{ci: any, tiers: any, workflows: Record<string, {parsed: any, text: string}>}} model
 * @returns {string[]} the problems, empty when the tiers are sound
 */
export function check({ ci, tiers, workflows }) {
  const errors = [];
  const jobs = ci.jobs ?? {};
  const callJobs = Object.entries(jobs).filter(([, j]) => typeof j.uses === "string");
  if (callJobs.length < MINIMUM_CALL_JOBS) {
    errors.push(`ci.yml has ${callJobs.length} job(s) that call a workflow, expected at least ${MINIMUM_CALL_JOBS} — the parser stopped finding them`);
  }
  const tierJobs = tiers.jobs ?? {};
  const fast = [];
  const heavy = [];

  for (const [id, job] of callJobs) {
    const entry = tierJobs[id];
    const needs = asList(job.needs);
    const isHeavy = needs.includes("fast-gate");
    (isHeavy ? heavy : fast).push(id);
    if (!entry) {
      errors.push(`${id}: a job that calls a workflow has no entry in scripts/ci/tier-paths.json`);
    } else if (entry.tier !== (isHeavy ? "heavy" : "fast")) {
      errors.push(`${id}: tier-paths.json says "${entry.tier}" but its needs make it ${isHeavy ? "heavy" : "fast"}`);
    }
    if (isHeavy) {
      if (!String(job.if ?? "").includes(DRAFT_CONDITION)) {
        errors.push(`${id}: a heavy-tier job has no \`${DRAFT_CONDITION}\` in its if — it would run for a draft`);
      }
      if (!String(job.if ?? "").includes(FAST_GATE_CONDITION)) {
        errors.push(`${id}: a heavy-tier job's if has no \`${FAST_GATE_CONDITION}\` — the implicit success() skips it whenever a fast job was skipped`);
      }
    }
    if (!needs.includes("changes")) errors.push(`${id}: needs does not include \`changes\` (its path filter)`);
    const file = job.uses.replace(/^\.\/\.github\/workflows\//, "");
    if (file !== `${id}.yml`) errors.push(`${id}: calls ${job.uses}; the job id must be the workflow's file name (${id}.yml)`);
    const called = workflows[file];
    if (!called) {
      errors.push(`${id}: calls ${job.uses}, which does not exist`);
      continue;
    }
    const on = triggers(called.parsed);
    if (!("workflow_call" in on)) errors.push(`${file}: called by ci.yml but has no \`workflow_call\` arm`);
    for (const event of ["pull_request", "pull_request_target"]) {
      if (event in on) errors.push(`${file}: called by ci.yml but still starts on its own \`${event}\` (it would run twice, untiered)`);
    }
    const group = /^concurrency:\s*\n(?:[ \t]+.*\n)*?[ \t]+group:\s*(.*)$/m.exec(called.text)?.[1] ?? "";
    if (group.includes("github.workflow")) {
      errors.push(`${file}: its concurrency group uses github.workflow, which is "CI" in a called workflow — every called workflow would share one group`);
    }
  }
  for (const id of Object.keys(tierJobs)) {
    if (!jobs[id]) errors.push(`${id}: in tier-paths.json but ci.yml has no such job`);
  }

  const gate = jobs["fast-gate"];
  const final = jobs["ci-gate"];
  if (!gate) errors.push("ci.yml has no `fast-gate` job");
  if (!final) errors.push("ci.yml has no `ci-gate` job");
  if (!jobs.changes) errors.push("ci.yml has no `changes` job");
  if (gate) {
    if (!/\balways\(\)/.test(String(gate.if ?? ""))) errors.push("fast-gate must run with `if: always()`");
    const needs = asList(gate.needs);
    for (const id of ["changes", ...fast]) if (!needs.includes(id)) errors.push(`fast-gate: needs does not list ${id}`);
    for (const id of needs) if (heavy.includes(id)) errors.push(`fast-gate: needs the heavy job ${id}, which needs fast-gate`);
  }
  if (final) {
    if (!/\balways\(\)/.test(String(final.if ?? ""))) errors.push("ci-gate must run with `if: always()`");
    const needs = asList(final.needs);
    for (const id of Object.keys(jobs)) {
      if (id !== "ci-gate" && !needs.includes(id)) errors.push(`ci-gate: needs does not list ${id}`);
    }
    for (const id of needs) if (!jobs[id]) errors.push(`ci-gate: needs ${id}, which is not a job`);
  }

  const called = new Set(callJobs.map(([, j]) => j.uses.replace(/^\.\/\.github\/workflows\//, "")));
  for (const [file, { parsed }] of Object.entries(workflows)) {
    const on = triggers(parsed);
    const pr = "pull_request" in on || "pull_request_target" in on;
    if (pr && !OWN_PR_TRIGGER.has(file) && !called.has(file)) {
      errors.push(`${file}: starts on a pull request but is not a job of ci.yml, so it runs outside the tiers and outside ci-gate`);
    }
  }
  return errors;
}

export function load(root = ROOT) {
  const directory = path.join(root, ".github", "workflows");
  const workflows = {};
  for (const name of fs.readdirSync(directory).filter((n) => /\.ya?ml$/.test(n)).sort()) {
    const text = fs.readFileSync(path.join(directory, name), "utf8");
    workflows[name] = { parsed: loadYaml(text) ?? {}, text };
  }
  const ci = workflows["ci.yml"]?.parsed;
  if (!ci) throw new Error("no .github/workflows/ci.yml");
  const tiers = JSON.parse(fs.readFileSync(path.join(root, "scripts", "ci", "tier-paths.json"), "utf8"));
  return { ci, tiers, workflows };
}

export function main({ root = ROOT, out = console.log, err = console.error } = {}) {
  let model;
  try {
    model = load(root);
  } catch (error) {
    err(`::error::${error.message}`);
    return 1;
  }
  const errors = check(model);
  if (errors.length) {
    for (const e of errors) err(`::error::${e}`);
    return 1;
  }
  out(`the tiers are sound: ${Object.keys(model.tiers.jobs).length} jobs, every one in ci-gate`);
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
