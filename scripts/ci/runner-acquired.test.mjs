// runner-acquired.mjs: what counts as "never acquired", and the desktop verdict.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { loadYaml } from "./ci-compat.mjs";
import { FIRST, PROBE, SECOND, findJob, main, neverAcquired, probe, verdict } from "./runner-acquired.mjs";

const NAME = "Host (fmt, clippy, tests) + desktop web build";
// The shape of job 138021821938 (PR #2980, run 45264812475), as the API returned it.
const notAcquiredJob = { id: 138021821938, name: `ci-desktop / ${NAME}`, status: "completed", conclusion: "cancelled", runner_name: "", steps: [] };
const notAcquiredNotes = [
  { annotation_level: "failure", message: "The job was not acquired by Runner of type hosted even after multiple attempts" },
  { annotation_level: "notice", message: "Due to capacity constraints, jobs targeting macOS arm64 runners may experience longer queue times." },
];

test("never acquired: completed, not a success, no runner, no step, and GitHub's annotation", () => {
  assert.equal(neverAcquired(notAcquiredJob, notAcquiredNotes), true);
  assert.equal(neverAcquired({ ...notAcquiredJob, runner_name: null }, notAcquiredNotes), true);
});

test("anything else is not: a job that ran, a newer push's cancel, a timeout, a success", () => {
  // Ran and failed: it had a runner and steps.
  assert.equal(neverAcquired({ ...notAcquiredJob, conclusion: "failure", runner_name: "GitHub Actions 1000184474", steps: [{}] }, []), false);
  // Cancelled while queued by a newer push: no runner, no step, but another message.
  assert.equal(neverAcquired(notAcquiredJob, [{ message: "Canceling since a higher priority waiting request for ci-2947 exists" }]), false);
  assert.equal(neverAcquired(notAcquiredJob, []), false);
  // Timed out: it ran.
  assert.equal(neverAcquired({ ...notAcquiredJob, runner_name: "GitHub Actions 1", steps: [{}, {}] }, notAcquiredNotes), false);
  assert.equal(neverAcquired({ ...notAcquiredJob, steps: [{}] }, notAcquiredNotes), false);
  assert.equal(neverAcquired({ ...notAcquiredJob, conclusion: "success" }, notAcquiredNotes), false);
  assert.equal(neverAcquired({ ...notAcquiredJob, status: "in_progress" }, notAcquiredNotes), false);
  assert.equal(neverAcquired(undefined, notAcquiredNotes), false);
});

test("findJob: the one job by name, with or without the caller's prefix; none or two is an error", () => {
  assert.equal(findJob([{ name: "x" }, notAcquiredJob], NAME), notAcquiredJob);
  assert.equal(findJob([{ name: NAME, id: 1 }], NAME).id, 1);
  assert.throws(() => findJob([{ name: "x" }], NAME), /found 0/);
  assert.throws(() => findJob([notAcquiredJob, { ...notAcquiredJob, id: 2 }], NAME), /found 2/);
  // A second-attempt job's longer name is not the first attempt's.
  assert.equal(findJob([notAcquiredJob, { name: `ci-desktop / ${NAME} — second attempt (the first got no runner)` }], NAME), notAcquiredJob);
});

const fakeApi = (jobs, annotations, seen = []) => async (url) => {
  seen.push(url);
  if (url.includes("/actions/runs/")) return { ok: true, json: async () => ({ jobs }) };
  if (url.includes("/check-runs/")) return { ok: true, json: async () => annotations };
  return { ok: false, status: 404 };
};
const env = { GITHUB_TOKEN: "t", GITHUB_REPOSITORY: "o/r", GITHUB_RUN_ID: "45264812475", GITHUB_RUN_ATTEMPT: "1", JOB_NAME: NAME };

test("probe reads this run attempt's jobs and the job's annotations", async () => {
  const seen = [];
  assert.equal(await probe(env, fakeApi([notAcquiredJob], notAcquiredNotes, seen), () => {}), true);
  assert.match(seen[0], /\/repos\/o\/r\/actions\/runs\/45264812475\/attempts\/1\/jobs/);
  assert.match(seen[1], /\/repos\/o\/r\/check-runs\/138021821938\/annotations/);
  assert.equal(await probe(env, fakeApi([{ ...notAcquiredJob, runner_name: "r", steps: [{}] }], []), () => {}), false);
});

test("probe fails loudly: a missing variable or an API error is an error, not `false`", async () => {
  await assert.rejects(probe({ ...env, JOB_NAME: "" }, fakeApi([], []), () => {}), /JOB_NAME/);
  await assert.rejects(probe(env, async () => ({ ok: false, status: 403 }), () => {}), /HTTP 403/);
});

const ok = { result: "success" };
const n = (over = {}) => ({ desktop: ok, "host-runner": { result: "skipped", outputs: {} }, "desktop-second-attempt": { result: "skipped", outputs: {} }, "pane-drivers": ok, "windows-host": ok, ...over });

test("verdict: pass when every job passed", () => {
  assert.equal(verdict(n()).verdict, "pass");
});

test("verdict: pass when the host was never acquired and its second attempt ran and passed", () => {
  const v = verdict(n({ desktop: { result: "cancelled" }, "host-runner": { result: "success", outputs: { "not-acquired": "true" } }, "desktop-second-attempt": ok }));
  assert.equal(v.verdict, "pass");
  assert.match(v.why.join("\n"), /never acquired.*second attempt ran and passed/);
});

test("verdict: fail for a real failure, a second attempt that did not pass, or any other failed job", () => {
  // The host ran and failed: no second attempt.
  assert.equal(verdict(n({ desktop: { result: "failure" }, "host-runner": { result: "success", outputs: { "not-acquired": "false" } } })).verdict, "fail");
  // The probe itself failed: no output, no pass.
  assert.equal(verdict(n({ desktop: { result: "cancelled" }, "host-runner": { result: "failure", outputs: {} } })).verdict, "fail");
  // Never acquired, and the second attempt failed, or was never acquired either, or skipped.
  for (const result of ["failure", "cancelled", "skipped"]) {
    const v = verdict(n({ desktop: { result: "cancelled" }, "host-runner": { result: "success", outputs: { "not-acquired": "true" } }, "desktop-second-attempt": { result } }));
    assert.equal(v.verdict, "fail", result);
  }
  // The host passed but another job did not (or is missing).
  assert.equal(verdict(n({ "pane-drivers": { result: "failure" } })).verdict, "fail");
  assert.equal(verdict(n({ "windows-host": { result: "skipped" } })).verdict, "fail");
  assert.equal(verdict(n({ desktop: undefined })).verdict, "fail");
});

test("ci-desktop.yml is wired to this script: the probed name, the job ids, and every job in the verdict's needs", () => {
  const file = path.join(path.dirname(new URL(import.meta.url).pathname), "..", "..", ".github", "workflows", "ci-desktop.yml");
  const jobs = loadYaml(fs.readFileSync(file, "utf8")).jobs;
  // A renamed host job would make the probe throw, and the second attempt would never start.
  const probeStep = jobs[PROBE].steps.find((s) => s.id === "probe");
  assert.equal(probeStep.env.JOB_NAME, jobs[FIRST].name);
  assert.deepEqual([].concat(jobs[SECOND].needs), [PROBE]);
  assert.match(jobs[SECOND].if, /needs\.host-runner\.outputs\.not-acquired == 'true'/);
  // A job left out of the verdict's needs would be ignored by a `pass`.
  const others = Object.keys(jobs).filter((id) => id !== "verdict").sort();
  assert.deepEqual([...jobs.verdict.needs].sort(), others);
  // The second attempt never writes the cache key the first one owns.
  const cache = jobs[SECOND].steps.find((s) => String(s.uses).startsWith("Swatinem/rust-cache"));
  assert.equal(cache.with["save-if"], false);
});

test("main writes the output line for the step", async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "runner-acquired-"));
  const out = path.join(dir, "out");
  fs.writeFileSync(out, "");
  await main(["node", "x", "verdict"], { NEEDS: JSON.stringify(n()), GITHUB_OUTPUT: out }, () => {});
  await main(["node", "x", "probe"], { ...env, GITHUB_OUTPUT: out }, () => {}, fakeApi([notAcquiredJob], notAcquiredNotes));
  assert.equal(fs.readFileSync(out, "utf8"), "verdict=pass\nnot-acquired=true\n");
  await assert.rejects(main(["node", "x", "nope"], {}, () => {}), /usage/);
});
