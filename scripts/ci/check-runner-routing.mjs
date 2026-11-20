#!/usr/bin/env node
// Only our own jobs can be routed to the self-hosted pool, and only jobs that can run there.
//
// `eng-runner` is the org's ARC runner scale set (deploy/ci-runners/README.md). It runs the job directly in
// the plain `actions-runner` pod: no Docker daemon (so no `container:`, no `services:`, no docker, podman or
// kind), no root (no sudo, no apt), and no C toolchain. The repository is public. A job may reach the pool only
// through ROUTED_RUNS_ON below, which sends it there only when ALL of these hold:
//
//   * the repository variable ENG_RUNNER is `on` (unset by default: the kill switch);
//   * the actor, and the actor of a re-run, is not Dependabot;
//   * the event is schedule, workflow_dispatch or merge_group (only writers start them, and their head is in
//     this repository), or a pull_request whose head repository IS this repository and whose author is not
//     Dependabot. Never an "everything but pull_request" condition: pull_request_target, workflow_run and
//     issue_comment would pass it.
//
// Everything else runs on ubuntu-latest. This is routing, not security: a fork's pull request runs the fork's
// own YAML. The guards for that are the runner group's repository list, fork-run approval, and the job-started
// hook (deploy/ci-runners/job-started.sh).
//
// This check fails when:
//   * a job's runs-on can resolve to eng-runner (a literal, a matrix value, an expression naming ENG_RUNNER)
//     and is not exactly ROUTED_RUNS_ON, or runs-on is an expression this check cannot see through;
//   * a routed job has `container:` or `services:`, or a step (its own, or one inside a local composite
//     action it uses) runs docker, podman, buildah, kind, sudo, apt or systemctl, or uses a docker/* or kind
//     action or a Docker container action — unless the step's `if:` starts with HOSTED_ONLY;
//   * a routed job is in a workflow that can start on any event other than pull_request, merge_group,
//     schedule, workflow_dispatch (and workflow_call from such a caller): pull_request_target and
//     workflow_run are refused outright;
//   * it finds fewer workflows or routed jobs than its floors: "found nothing, therefore clean" is how a
//     guard stops guarding.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { loadYaml, isEntrypoint } from "./ci-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SELF), "..", "..");

export const RUNNER = "eng-runner";
/** The ONE routing condition, as a bare expression (without `${{ }}`). */
export const ROUTING_CONDITION =
  "vars.ENG_RUNNER == 'on'" +
  " && github.actor != 'dependabot[bot]' && github.triggering_actor != 'dependabot[bot]'" +
  " && (github.event_name == 'schedule' || github.event_name == 'workflow_dispatch' || github.event_name == 'merge_group'" +
  " || (github.event_name == 'pull_request' && github.event.pull_request.head.repo.full_name == github.repository" +
  " && github.event.pull_request.user.login != 'dependabot[bot]'))";
/** The only runs-on a job may use to reach the pool. */
export const ROUTED_RUNS_ON = `\${{ (${ROUTING_CONDITION}) && 'eng-runner' || 'ubuntu-latest' }}`;
/** A step whose `if:` starts with this runs on GitHub-hosted runners only. */
export const HOSTED_ONLY = "runner.environment == 'github-hosted'";
/** Events a workflow with a routed job may start on. */
export const ALLOWED_EVENTS = new Set(["pull_request", "merge_group", "schedule", "workflow_dispatch", "workflow_call"]);
export const MINIMUM_WORKFLOWS = 40;
export const MINIMUM_ROUTED_JOBS = 20;

// What cannot run in the pod. Read per line of a step's `run:`, comments dropped.
const FORBIDDEN_RUN = [
  [/(?:^|[\s;&|(`$])docker(?:-compose)?\s/, "docker (no daemon)"],
  [/\bpodman\b/, "podman"],
  [/\bbuildah\b/, "buildah"],
  [/\bkind\s+(?:create|delete|load|get|export|build)\b/, "kind"],
  [/\bsudo\b/, "sudo (no root)"],
  [/\bapt(?:-get)?\s+(?:install|update|upgrade|remove|purge)\b/, "apt (no root)"],
  [/\bdpkg\s+-i\b/, "dpkg (no root)"],
  [/\bsystemctl\b/, "systemctl"],
  [/\brunsc\b/, "gVisor"],
];
const FORBIDDEN_USES = [
  [/^docker\//, "a docker/* action (no daemon)"],
  [/^docker:\/\//, "a container action (no daemon)"],
  [/^helm\/kind-action\b/, "kind"],
];

const strip = (expression) => String(expression ?? "").trim().replace(/^\$\{\{\s*([\s\S]*?)\s*\}\}$/, "$1").trim();
const triggers = (workflow) => {
  const on = Object.hasOwn(workflow, "true") ? workflow.true : workflow.on;
  if (typeof on === "string") return [on];
  if (Array.isArray(on)) return on.map(String);
  return Object.keys(on ?? {});
};
const mentionsRunner = (value) => JSON.stringify(value ?? null).toLowerCase().includes(RUNNER);

/** How a job's runs-on can resolve: "routed", "hosted" (cannot reach the pool) or a problem. */
export function classifyRunsOn(job) {
  const runsOn = job["runs-on"];
  if (runsOn === ROUTED_RUNS_ON) return { kind: "routed" };
  if (typeof runsOn === "string" && !runsOn.includes("${{")) {
    return runsOn.toLowerCase().includes(RUNNER) ? { kind: "problem", why: `runs-on names ${RUNNER} without the routing condition` } : { kind: "hosted" };
  }
  if (typeof runsOn !== "string") {
    // A list of labels or a {group, labels} map.
    if (mentionsRunner(runsOn) || /\$\{\{/.test(JSON.stringify(runsOn ?? null))) {
      return { kind: "problem", why: `runs-on ${JSON.stringify(runsOn)} can reach ${RUNNER} without the routing condition` };
    }
    return { kind: "hosted" };
  }
  const expression = strip(runsOn);
  if (/eng[-_]?runner/i.test(expression)) {
    return { kind: "problem", why: `runs-on is not exactly the routing condition (scripts/ci/check-runner-routing.mjs ROUTED_RUNS_ON): ${runsOn}` };
  }
  // A matrix value is visible: every value the matrix can give must be a hosted label.
  if (/^matrix(?:\.[A-Za-z_][\w-]*)+$/.test(expression)) {
    return mentionsRunner(job.strategy?.matrix) ? { kind: "problem", why: `runs-on ${runsOn} takes ${RUNNER} from the matrix without the routing condition` } : { kind: "hosted" };
  }
  return { kind: "problem", why: `runs-on ${runsOn} is an expression this check cannot see through; use a label, a matrix value or ROUTED_RUNS_ON` };
}

/** The problems with one step on the pool, recursing into local composite actions. */
function stepProblems(step, actions, seen = new Set()) {
  if (strip(step.if).startsWith(HOSTED_ONLY)) return [];
  const problems = [];
  const lines = String(step.run ?? "").split("\n").map((l) => l.replace(/(^|\s)#.*$/, ""));
  for (const [pattern, what] of FORBIDDEN_RUN) {
    if (lines.some((l) => pattern.test(l))) problems.push(`runs ${what}`);
  }
  const uses = String(step.uses ?? "");
  for (const [pattern, what] of FORBIDDEN_USES) if (pattern.test(uses)) problems.push(`uses ${what}`);
  if (uses.startsWith("./")) {
    const name = uses.replace(/^\.\//, "").replace(/\/$/, "");
    const action = actions[name];
    if (!action) {
      problems.push(`uses ${uses}, which has no action.yml`);
    } else if (action.runs?.using === "docker") {
      problems.push(`uses ${uses}, a Docker container action (no daemon)`);
    } else if (action.runs?.using === "composite" && !seen.has(name)) {
      seen.add(name);
      for (const inner of action.runs.steps ?? []) {
        for (const p of stepProblems(inner, actions, seen)) problems.push(`${p} (in ${uses}: ${inner.name ?? inner.uses ?? "a step"})`);
      }
    }
  }
  return problems;
}

/** The events a workflow can effectively start on: its own, plus its callers' for workflow_call. */
function effectiveEvents(workflows) {
  const callers = {};
  for (const [file, { parsed }] of Object.entries(workflows)) {
    for (const job of Object.values(parsed.jobs ?? {})) {
      const m = /^\.\/\.github\/workflows\/([^@]+)$/.exec(String(job.uses ?? ""));
      if (m) (callers[m[1]] ??= new Set()).add(file);
    }
  }
  const memo = {};
  const events = (file, stack = new Set()) => {
    if (memo[file]) return memo[file];
    const out = new Set();
    for (const e of triggers(workflows[file]?.parsed ?? {})) if (e !== "workflow_call") out.add(e);
    if (!stack.has(file)) {
      stack.add(file);
      for (const caller of callers[file] ?? []) for (const e of events(caller, stack)) out.add(e);
      stack.delete(file);
    }
    return (memo[file] = out);
  };
  return Object.fromEntries(Object.keys(workflows).map((f) => [f, events(f)]));
}

/**
 * @param {{workflows: Record<string, {parsed: any}>, actions: Record<string, any>}} model
 * @returns {{errors: string[], routed: string[]}}
 */
export function check({ workflows, actions }) {
  const errors = [];
  const routed = [];
  const events = effectiveEvents(workflows);
  for (const [file, { parsed }] of Object.entries(workflows)) {
    for (const [id, job] of Object.entries(parsed.jobs ?? {})) {
      if (typeof job.uses === "string") continue; // a call: its jobs are checked in their own file
      const where = `${file} ${id}`;
      const runsOn = classifyRunsOn(job);
      if (runsOn.kind === "problem") {
        errors.push(`${where}: ${runsOn.why}`);
        continue;
      }
      if (runsOn.kind !== "routed") continue;
      routed.push(where);
      if (job.container !== undefined) errors.push(`${where}: routed to ${RUNNER} but has \`container:\` (the pool has no Docker daemon)`);
      if (job.services !== undefined) errors.push(`${where}: routed to ${RUNNER} but has \`services:\` (the pool has no Docker daemon)`);
      const bad = [...events[file]].filter((e) => !ALLOWED_EVENTS.has(e));
      if (bad.length) errors.push(`${where}: routed to ${RUNNER} in a workflow that can start on ${bad.join(", ")}; only ${[...ALLOWED_EVENTS].join(", ")} may reach the pool`);
      (job.steps ?? []).forEach((step, i) => {
        for (const p of stepProblems(step, actions)) {
          errors.push(`${where}: step ${i + 1} (${step.name ?? step.uses ?? "unnamed"}) ${p}; the pool has no Docker daemon and no root. Keep the job hosted, or give the step \`if: ${HOSTED_ONLY}\``);
        }
      });
    }
  }
  return { errors, routed };
}

export function load(root = ROOT) {
  const read = (dir, filter) => fs.readdirSync(dir).filter(filter).sort();
  const wfDir = path.join(root, ".github", "workflows");
  const workflows = {};
  for (const name of read(wfDir, (n) => /\.ya?ml$/.test(n))) {
    workflows[name] = { parsed: loadYaml(fs.readFileSync(path.join(wfDir, name), "utf8")) ?? {} };
  }
  const actions = {};
  const acDir = path.join(root, ".github", "actions");
  if (fs.existsSync(acDir)) {
    for (const name of read(acDir, () => true)) {
      for (const file of ["action.yml", "action.yaml"]) {
        const p = path.join(acDir, name, file);
        if (fs.existsSync(p)) actions[`.github/actions/${name}`] = loadYaml(fs.readFileSync(p, "utf8")) ?? {};
      }
    }
  }
  return { workflows, actions };
}

export function main({ root = ROOT, out = console.log, err = console.error, minimumWorkflows = MINIMUM_WORKFLOWS, minimumRouted = MINIMUM_ROUTED_JOBS } = {}) {
  const model = load(root);
  const count = Object.keys(model.workflows).length;
  const { errors, routed } = check(model);
  if (count < minimumWorkflows) errors.push(`read ${count} workflow(s), expected at least ${minimumWorkflows} — the reader stopped finding them`);
  if (routed.length < minimumRouted) errors.push(`found ${routed.length} job(s) routed to ${RUNNER}, expected at least ${minimumRouted} — the routing expression changed, or this check stopped recognising it`);
  if (errors.length) {
    for (const e of errors) err(`::error::${e}`);
    return 1;
  }
  out(`runner routing holds: ${routed.length} job(s) routable to ${RUNNER} through the one condition, ${count} workflow(s) read`);
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
