#!/usr/bin/env node
// Was a hosted job ever given a runner? And what is the workflow's verdict?
//
// GitHub gives a hosted job three tries at a runner, five minutes apart. When
// none takes it, GitHub ends the job as cancelled, about 15 minutes after it
// was queued, with no runner, no step, and the annotation "The job was not
// acquired by Runner of type hosted even after multiple attempts". Nothing of
// ours ran, so it is neither a pass nor a failure of the change. On 2026-11-23
// it ended ci-desktop.yml's macOS host five times (GitHub's macOS arm64
// capacity: its own notice on those jobs), while none of our macOS jobs ran.
//
//   probe    (ci-desktop.yml `host-runner`) reads this run's jobs and the
//            job's check-run annotations, and writes `not-acquired=true|false`
//            to $GITHUB_OUTPUT. Only all three signs together are "never
//            acquired": a job that timed out, failed, or was cancelled by a
//            newer push has a runner, steps, or another message, and gets no
//            second attempt. Env: GITHUB_TOKEN, GITHUB_REPOSITORY,
//            GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT, JOB_NAME (the job's `name:`).
//   verdict  (ci-desktop.yml `verdict`) reads NEEDS (toJSON(needs)) and writes
//            `verdict=pass|fail`: pass only when every job passed, or the
//            first attempt was never acquired and the second attempt ran and
//            passed. ci.yml's ci-gate reads it (scripts/ci/ci-gate.mjs). It
//            exits 0 either way: it is the input of the gate, not a check of
//            its own (a failed job is already red).

import fs from "node:fs";

import { isEntrypoint } from "./py-compat.mjs";

export const NOT_ACQUIRED = /was not acquired by Runner/i;
export const FIRST = "desktop";
export const PROBE = "host-runner";
export const SECOND = "desktop-second-attempt";

/** True when GitHub never gave this job a runner: completed, not a success, no runner, no step, and GitHub's annotation. */
export function neverAcquired(job, annotations) {
  if (!job || job.status !== "completed" || job.conclusion === "success") return false;
  if (job.runner_name) return false;
  if ((job.steps ?? []).length > 0) return false;
  return annotations.some((a) => NOT_ACQUIRED.test(String(a.message ?? "")));
}

/** The one job of the run with this name (a called workflow's jobs are named "<caller job> / <name>"). */
export function findJob(jobs, name) {
  const found = jobs.filter((j) => j.name === name || String(j.name).endsWith(` / ${name}`));
  if (found.length !== 1) throw new Error(`expected one job named "${name}" in this run, found ${found.length}`);
  return found[0];
}

export async function probe(env, fetchImpl = fetch, log = console.log) {
  for (const key of ["GITHUB_TOKEN", "GITHUB_REPOSITORY", "GITHUB_RUN_ID", "JOB_NAME"]) {
    if (!env[key]) throw new Error(`${key} is not set`);
  }
  const api = env.GITHUB_API_URL || "https://api.github.com";
  const get = async (url) => {
    const response = await fetchImpl(`${api}/repos/${env.GITHUB_REPOSITORY}/${url}`, {
      headers: { Authorization: `Bearer ${env.GITHUB_TOKEN}`, Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" },
    });
    if (!response.ok) throw new Error(`${url}: HTTP ${response.status}`);
    return response.json();
  };
  const attempt = env.GITHUB_RUN_ATTEMPT || "1";
  const jobs = [];
  for (let page = 1; page <= 10; page++) {
    const batch = (await get(`actions/runs/${env.GITHUB_RUN_ID}/attempts/${attempt}/jobs?per_page=100&page=${page}`)).jobs ?? [];
    jobs.push(...batch);
    if (batch.length < 100) break;
  }
  const job = findJob(jobs, env.JOB_NAME);
  const annotations = await get(`check-runs/${job.id}/annotations?per_page=100`);
  const result = neverAcquired(job, annotations);
  log(`${job.name} (job ${job.id}): ${job.status}/${job.conclusion}, runner ${JSON.stringify(job.runner_name ?? null)}, ${(job.steps ?? []).length} step(s)`);
  for (const a of annotations) log(`  ${a.annotation_level}: ${a.message}`);
  log(result ? "never acquired by a runner: one second attempt" : "it was acquired (or ended for another reason): no second attempt");
  return result;
}

/** {verdict: "pass"|"fail", why: string[]} from the verdict job's `needs`. */
export function verdict(needs) {
  const why = [];
  const first = needs[FIRST]?.result;
  const retried = needs[PROBE]?.outputs?.["not-acquired"] === "true";
  const second = needs[SECOND]?.result;
  if (first === "success") {
    // The host passed on its first attempt.
  } else if (retried && second === "success") {
    why.push(`${FIRST}: never acquired by a runner; the second attempt ran and passed`);
  } else if (retried) {
    why.push(`${FIRST}: never acquired by a runner; the second attempt is ${second ?? "missing"}`);
    return { verdict: "fail", why };
  } else {
    why.push(`${FIRST}: ${first ?? "missing"}`);
    return { verdict: "fail", why };
  }
  for (const [id, need] of Object.entries(needs)) {
    if (id === FIRST || id === PROBE || id === SECOND) continue;
    if (need?.result !== "success") {
      why.push(`${id}: ${need?.result ?? "missing"}`);
      return { verdict: "fail", why };
    }
  }
  return { verdict: "pass", why };
}

export async function main(argv = process.argv, env = process.env, log = console.log, fetchImpl = fetch) {
  const mode = argv[2];
  let line;
  if (mode === "probe") {
    line = `not-acquired=${(await probe(env, fetchImpl, log)) ? "true" : "false"}\n`;
  } else if (mode === "verdict") {
    const needs = JSON.parse(env.NEEDS || "{}");
    for (const [id, n] of Object.entries(needs)) log(`${String(n?.result).padEnd(10)} ${id}`);
    const v = verdict(needs);
    for (const w of v.why) log(w);
    log(`verdict: ${v.verdict}`);
    line = `verdict=${v.verdict}\n`;
  } else {
    throw new Error(`usage: runner-acquired.mjs probe|verdict (got ${mode ?? "nothing"})`);
  }
  if (env.GITHUB_OUTPUT) fs.appendFileSync(env.GITHUB_OUTPUT, line);
  return 0;
}

if (isEntrypoint(import.meta.url)) {
  main().then(
    (code) => process.exit(code),
    (error) => {
      console.error(`::error::${error.message}`);
      process.exit(1);
    },
  );
}
