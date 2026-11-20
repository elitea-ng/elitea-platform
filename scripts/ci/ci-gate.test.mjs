// Scenarios for ci-gate.mjs and tier-changes.mjs: what each verdict says for a
// draft, a failure, a path-filtered skip and a heavy job that never ran.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { evaluate, TIERS_FILE } from "./ci-gate.mjs";
import { computeRun, fastTierAlreadyPassed, patternToRegExp, reaches, withFastReused } from "./tier-changes.mjs";

const tiers = {
  jobs: {
    a: { tier: "fast", paths: ["a/**"] },
    b: { tier: "fast", paths: null },
    h: { tier: "heavy", paths: ["h/**"] },
    g: { tier: "heavy", paths: ["g/**"] },
  },
};
const needs = (over = {}) => ({
  changes: { result: "success" },
  "fast-gate": { result: "success" },
  a: { result: "success" },
  b: { result: "success" },
  h: { result: "success" },
  g: { result: "skipped" },
  ...over,
});
const run = { a: true, b: true, h: true, g: false };

test("everything that should run passed: the verdict is green, a path-filtered skip counts", () => {
  assert.deepEqual(evaluate({ stage: "final", needs: needs(), run, draft: false, tiers }), { ok: true, problems: [] });
});

test("a draft fails ci-gate with the word draft, and names no heavy job as missing", () => {
  const result = evaluate({ stage: "final", needs: needs({ h: { result: "skipped" }, "fast-gate": { result: "success" } }), run, draft: true, tiers });
  assert.equal(result.ok, false);
  assert.match(result.problems[0], /^draft:/);
  assert.equal(result.problems.length, 1);
});

test("the fast stage passes for a draft: only the fast jobs count", () => {
  const result = evaluate({ stage: "fast", needs: { changes: { result: "success" }, a: { result: "success" }, b: { result: "success" } }, run, draft: true, tiers });
  assert.equal(result.ok, true);
});

test("a failed or cancelled job fails the gate", () => {
  for (const result of ["failure", "cancelled"]) {
    const out = evaluate({ stage: "final", needs: needs({ a: { result } }), run, draft: false, tiers });
    assert.equal(out.ok, false);
    assert.match(out.problems.join("\n"), new RegExp(`a: ${result}`));
  }
});

test("a heavy job that should have run but is skipped is missing", () => {
  const out = evaluate({ stage: "final", needs: needs({ h: { result: "skipped" }, "fast-gate": { result: "failure" } }), run, draft: false, tiers });
  assert.equal(out.ok, false);
  assert.match(out.problems.join("\n"), /h: missing \(skipped; the fast tier did not pass\)/);
});

test("a fast job that should have run but was skipped is missing", () => {
  const out = evaluate({ stage: "fast", needs: { changes: { result: "success" }, a: { result: "skipped" }, b: { result: "success" } }, run, draft: false, tiers });
  assert.equal(out.ok, false);
  assert.match(out.problems.join("\n"), /a: missing/);
});

test("a failed path filter fails every gate", () => {
  const out = evaluate({ stage: "fast", needs: { changes: { result: "failure" } }, run: {}, draft: false, tiers });
  assert.equal(out.ok, false);
  assert.match(out.problems[0], /^changes: failure/);
});

test("a job the gate does not need is named", () => {
  const n = needs();
  delete n.g;
  const out = evaluate({ stage: "final", needs: n, run, draft: false, tiers });
  assert.match(out.problems.join("\n"), /g: not in the gate's needs/);
});

test("patterns: ** crosses directories, * does not, ! excludes, last match wins", () => {
  assert.ok(patternToRegExp("libs/rust/**").test("libs/rust/a/b.rs"));
  assert.ok(!patternToRegExp("libs/rust/**").test("libs/go/a.rs"));
  assert.ok(patternToRegExp("scripts/runtime/*code*").test("scripts/runtime/run_code.py"));
  assert.ok(!patternToRegExp("scripts/runtime/*code*").test("scripts/runtime/x/code.py"));
  assert.ok(patternToRegExp("services/elitea-main/cmd/elitea-main/event_stream_nats*.go").test("services/elitea-main/cmd/elitea-main/event_stream_nats_x.go"));
  assert.ok(patternToRegExp("go.work").test("go.work"));
  assert.ok(!patternToRegExp("go.work").test("goxwork"));
  const helm = ["deploy/helm/**", "!deploy/helm/tests/**", "!deploy/helm/**/*.md"];
  assert.ok(reaches(["deploy/helm/elitea/values.yaml"], helm));
  assert.ok(!reaches(["deploy/helm/tests/a.yaml"], helm));
  assert.ok(!reaches(["deploy/helm/elitea/README.md"], helm));
  assert.ok(reaches(["anything"], null));
});

test("computeRun: a change reaches only the jobs whose paths it matches; the tiering files reach all", () => {
  const t = { always: ["scripts/ci/tier-paths.json"], jobs: tiers.jobs };
  assert.deepEqual(computeRun(t, ["a/x.go"]), { a: true, b: true, h: false, g: false });
  assert.deepEqual(computeRun(t, ["scripts/ci/tier-paths.json"]), { a: true, b: true, h: true, g: true });
  assert.deepEqual(computeRun(t, [], { everything: true }), { a: true, b: true, h: true, g: true });
});

test("the real tier file: a docs-only change reaches the docs job and not the Rust chains", () => {
  const real = JSON.parse(fs.readFileSync(TIERS_FILE, "utf8"));
  const out = computeRun(real, ["apps/elitea-docs/src/content/docs/index.mdx"]);
  assert.equal(out["ci-docs"], true);
  assert.equal(out["ci-rust"], false);
  assert.equal(out["no-binaries"], true);
});

test("changed-paths: a pull request is diffed against its base, any other event is always true", async () => {
  const { main } = await import("./changed-paths.mjs");
  const out = fs.mkdtempSync(path.join(os.tmpdir(), "cp-"));
  const outFile = path.join(out, "o");
  const run = (cmd, args) => ({ status: 0, stdout: args.some((a) => a === "origin/main...HEAD") ? "services/elitea-main/go.mod\nREADME.md\n" : "" });
  const quiet = () => {};
  const env = { GITHUB_EVENT_NAME: "pull_request", BASE_REF: "main", GITHUB_OUTPUT: outFile };
  assert.equal(main(["go", "**/go.mod", "go.work"], env, run, quiet, quiet), 0);
  assert.equal(main(["rust", "**/Cargo.lock"], env, run, quiet, quiet), 0);
  assert.equal(main(["all", "x"], { GITHUB_EVENT_NAME: "schedule", GITHUB_OUTPUT: outFile }, run, quiet, quiet), 0);
  assert.equal(fs.readFileSync(outFile, "utf8"), "go=true\nrust=false\nall=true\n");
  assert.equal(main([], env, run, quiet, quiet), 2);
});

test("ready_for_review reuses a green fast tier of the same head, and ci-gate accepts the skipped fast jobs", async () => {
  const jobsOf = { 1: [{ name: "fast-gate", conclusion: "failure" }], 2: [{ name: "fast-gate", conclusion: "success" }] };
  const fetchImpl = async (url) => ({
    ok: true,
    json: async () => (url.includes("/workflows/ci.yml/runs") ? { workflow_runs: [{ id: 9, status: "completed" }, { id: 1, status: "completed" }, { id: 3, status: "in_progress" }, { id: 2, status: "completed" }] } : { jobs: jobsOf[Number(url.match(/runs\/(\d+)\/jobs/)[1])] ?? [] }),
  });
  assert.equal(await fastTierAlreadyPassed({ token: "t", repo: "o/r", headSha: "abc", runId: 9 }, fetchImpl), 2);
  const none = async (url) => ({ ok: true, json: async () => (url.includes("/workflows/") ? { workflow_runs: [{ id: 1, status: "completed" }] } : { jobs: jobsOf[1] }) });
  assert.equal(await fastTierAlreadyPassed({ token: "t", repo: "o/r", headSha: "abc", runId: 9 }, none), null);
  const reused = withFastReused({ a: true, b: true, h: true, g: false }, tiers);
  assert.deepEqual(reused, { a: false, b: false, h: true, g: false });
  const n = needs({ a: { result: "skipped" }, b: { result: "skipped" } });
  assert.deepEqual(evaluate({ stage: "final", needs: n, run: reused, draft: false, tiers }), { ok: true, problems: [] });
});
