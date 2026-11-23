#!/usr/bin/env node
// The verdicts of ci.yml: `fast-gate` (stage `fast`) and `ci-gate` (`final`).
//
// Both run with `if: always()` and read what their `needs` did, so a job that
// the path filter skipped counts as a success and a job that FAILED or was
// CANCELLED never does. What a skip means depends on why it happened, which is
// why the gate also reads the `changes` output:
//
//   run[job] false      the change does not reach the job: skipped is correct.
//   run[job] true       the job must have succeeded. A fast job that was skipped
//                       anyway, or a heavy job skipped because the fast tier
//                       failed or the pull request is a draft, is MISSING: the
//                       gate names it and fails, so a check list that went green
//                       by absence cannot reach a required check.
//
// On a draft the ci-gate job does not run at all (ci.yml skips it, grey not red;
// check-tiering.mjs holds that), and fast-gate is the draft's signal. `final`
// still fails if it is ever evaluated for a draft: the heavy tier has not run,
// so a draft has no verdict, and a skipped gate must not become a passing one.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { isEntrypoint } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
export const TIERS_FILE = path.join(path.dirname(SELF), "tier-paths.json");

// ci-web-e2e's lanes each carry their own lane-filter `if:`
// (scripts/ci/e2e-lane-filter.sh, run by ci-build-once's model-path-changes
// job). A change the path filter sends to ci-web-e2e (deploy/** holds the
// compose files) can still reach no lane: a chart-only pull request. Every
// lane then skips, and GitHub reports the calling job as skipped. That is a
// verdict, not an absence, when ci-build-once SUCCEEDED and its outputs say no
// lane is reached; anything else (a failed or skipped build-once, an output
// that is missing or true) stays missing.
export const E2E_LANE_OUTPUTS = ["reaches-stack", "reaches-model", "reaches-handoff", "reaches-sandbox"];

/** True when ci-build-once's lane filter decided that no ci-web-e2e lane runs. */
export function e2eReachesNoLane(needs) {
  const build = needs["ci-build-once"];
  if (build?.result !== "success") return false;
  const outputs = build.outputs ?? {};
  return E2E_LANE_OUTPUTS.every((key) => outputs[key] === "false");
}

/**
 * @param {{stage: "fast"|"final", needs: Record<string,{result:string}>, run: Record<string,boolean>, draft: boolean, tiers: {jobs: Record<string,{tier:string}>}}} input
 * @returns {{ok: boolean, problems: string[]}}
 */
export function evaluate({ stage, needs, run, draft, tiers }) {
  const problems = [];
  const changes = needs.changes?.result;
  if (changes !== "success") {
    problems.push(`changes: ${changes ?? "missing"} — the path filter did not produce a verdict`);
    return { ok: false, problems };
  }
  for (const [id, job] of Object.entries(tiers.jobs)) {
    if (stage === "fast" && job.tier !== "fast") continue;
    const result = needs[id]?.result;
    if (result === undefined) {
      problems.push(`${id}: not in the gate's needs`);
    } else if (result === "failure" || result === "cancelled") {
      problems.push(`${id}: ${result}`);
    } else if (run[id] === true && result !== "success") {
      // A draft never runs the heavy tier: the one `draft` problem below says so.
      if (draft && job.tier === "heavy") continue;
      if (id === "ci-web-e2e" && result === "skipped" && e2eReachesNoLane(needs)) continue;
      const why = job.tier === "heavy" ? "the fast tier did not pass" : "it did not run";
      problems.push(`${id}: missing (${result}; ${why})`);
    }
  }
  if (stage === "fast") {
    return { ok: problems.length === 0, problems };
  }
  if (needs["fast-gate"]?.result !== "success") problems.push(`fast-gate: ${needs["fast-gate"]?.result ?? "missing"}`);
  if (draft) problems.unshift("draft: the pull request is a draft, so the heavy tier has not run — mark it ready for review (gh pr ready) once the fast tier is green");
  return { ok: problems.length === 0, problems };
}

export function main(env = process.env, log = console.log, err = console.error) {
  const stage = process.argv[2] === "fast" ? "fast" : "final";
  const tiers = JSON.parse(fs.readFileSync(TIERS_FILE, "utf8"));
  const needs = JSON.parse(env.NEEDS || "{}");
  let run = {};
  try {
    run = JSON.parse(env.RUN || "{}");
  } catch {
    run = {};
  }
  const { ok, problems } = evaluate({ stage, needs, run, draft: env.DRAFT === "true", tiers });
  for (const [id, n] of Object.entries(needs)) log(`${String(n.result).padEnd(10)} ${id}`);
  if (!ok) {
    for (const p of problems) err(`::error::${stage === "fast" ? "fast tier" : "ci-gate"}: ${p}`);
    return 1;
  }
  log(stage === "fast" ? "the fast tier passed" : "every job that should have run passed");
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
