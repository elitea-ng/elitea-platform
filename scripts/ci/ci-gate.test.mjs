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

test("ci-desktop: a never-acquired job whose second attempt passed is accepted only by the verdict output `pass`", () => {
  const t = { jobs: { ...tiers.jobs, "ci-desktop": { tier: "heavy", paths: ["d/**"] } } };
  const r = { ...run, "ci-desktop": true };
  for (const result of ["failure", "cancelled"]) {
    const pass = evaluate({ stage: "final", needs: needs({ "ci-desktop": { result, outputs: { verdict: "pass" } } }), run: r, draft: false, tiers: t });
    assert.deepEqual(pass, { ok: true, problems: [] });
    for (const outputs of [{ verdict: "fail" }, { verdict: "" }, { verdict: "PASS" }, {}, undefined]) {
      const out = evaluate({ stage: "final", needs: needs({ "ci-desktop": { result, outputs } }), run: r, draft: false, tiers: t });
      assert.equal(out.ok, false, JSON.stringify({ result, outputs }));
      assert.match(out.problems.join("\n"), new RegExp(`ci-desktop: ${result}`));
    }
  }
  // Only the workflows that publish a verdict: another job's `verdict` output decides nothing.
  const other = evaluate({ stage: "final", needs: needs({ h: { result: "failure", outputs: { verdict: "pass" } } }), run, draft: false, tiers });
  assert.equal(other.ok, false);
  // A skipped ci-desktop that should have run stays missing, whatever it outputs.
  const skipped = evaluate({ stage: "final", needs: needs({ "ci-desktop": { result: "skipped", outputs: { verdict: "pass" } }, "fast-gate": { result: "failure" } }), run: r, draft: false, tiers: t });
  assert.equal(skipped.ok, false);
  assert.match(skipped.problems.join("\n"), /ci-desktop: missing/);
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

test("computeRun: an `unless` job does not run where the job it names does", () => {
  const t = { always: ["scripts/ci/tier-paths.json"], jobs: { go: { tier: "fast", paths: ["svc/**"] }, contract: { tier: "fast", paths: ["svc/tests/contract/**", "contract.yml"], unless: "go" } } };
  assert.deepEqual(computeRun(t, ["svc/tests/contract/a_test.go"]), { go: true, contract: false });
  assert.deepEqual(computeRun(t, ["contract.yml"]), { go: false, contract: true });
  assert.deepEqual(computeRun(t, [], { everything: true }), { go: true, contract: false });
  assert.throws(() => computeRun({ always: [], jobs: { c: { tier: "fast", paths: ["x"], unless: "nope" } } }, ["x"]), /unless names nope/);
});

test("computeRun: a change reaches only the jobs whose paths it matches; the tiering files reach all", () => {
  const t = { always: ["scripts/ci/tier-paths.json"], jobs: tiers.jobs };
  assert.deepEqual(computeRun(t, ["a/x.go"]), { a: true, b: true, h: false, g: false });
  assert.deepEqual(computeRun(t, ["scripts/ci/tier-paths.json"]), { a: true, b: true, h: true, g: true });
  assert.deepEqual(computeRun(t, [], { everything: true }), { a: true, b: true, h: true, g: true });
});

test("computeRun: a union entry runs exactly when one of its members does", () => {
  const t = { always: ["scripts/ci/tier-paths.json"], jobs: { ...tiers.jobs, u: { tier: "heavy", union: ["h", "g"] } } };
  assert.equal(computeRun(t, ["a/x.go"]).u, false);
  assert.equal(computeRun(t, ["g/x.rs"]).u, true);
  assert.equal(computeRun(t, ["scripts/ci/tier-paths.json"]).u, true);
  assert.throws(() => computeRun({ always: [], jobs: { u: { tier: "heavy", union: ["nope"] } } }, []), /union names nope/);
});

test("the real tier file: ci-build-once runs for a Rust-only change, a chart change and a stack change, not for docs", () => {
  const real = JSON.parse(fs.readFileSync(TIERS_FILE, "utf8"));
  assert.equal(computeRun(real, ["services/elitea-vector/src/main.rs"])["ci-build-once"], true);
  assert.equal(computeRun(real, ["libs/rust/agent-runtime/src/lib.rs"])["ci-build-once"], true);
  assert.equal(computeRun(real, ["deploy/helm/elitea/values.yaml"])["ci-build-once"], true);
  assert.equal(computeRun(real, ["services/elitea-main/cmd/elitea-main/main.go"])["ci-build-once"], true);
  assert.equal(computeRun(real, ["apps/elitea-docs/src/content/docs/index.mdx"])["ci-build-once"], false);
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

test("ci-web-e2e skipped because the lane filter reached no lane is a verdict, not an absence", () => {
  const t = { jobs: { "ci-build-once": { tier: "heavy", paths: ["deploy/**"] }, "ci-web-e2e": { tier: "heavy", paths: ["deploy/**"] } } };
  const r = { "ci-build-once": true, "ci-web-e2e": true };
  const none = { "reaches-stack": "false", "reaches-model": "false", "reaches-handoff": "false", "reaches-sandbox": "false" };
  const base = (build, e2e = { result: "skipped" }) => ({ changes: { result: "success" }, "fast-gate": { result: "success" }, "ci-build-once": build, "ci-web-e2e": e2e });
  // A chart-only pull request: build-once ran and reached no lane.
  assert.deepEqual(evaluate({ stage: "final", needs: base({ result: "success", outputs: none }), run: r, draft: false, tiers: t }), { ok: true, problems: [] });
  // Any lane reached, an output missing, or build-once not a success: still missing.
  for (const build of [
    { result: "success", outputs: { ...none, "reaches-stack": "true" } },
    { result: "success", outputs: { ...none, "reaches-sandbox": "true" } },
    { result: "success", outputs: { "reaches-stack": "false" } },
    { result: "success" },
    { result: "skipped", outputs: none },
  ]) {
    const out = evaluate({ stage: "final", needs: base(build), run: r, draft: false, tiers: t });
    assert.equal(out.ok, false, JSON.stringify(build));
    assert.match(out.problems.join("\n"), /ci-web-e2e: missing/);
  }
  // A failed ci-web-e2e is never excused.
  const failed = evaluate({ stage: "final", needs: base({ result: "success", outputs: none }, { result: "failure" }), run: r, draft: false, tiers: t });
  assert.match(failed.problems.join("\n"), /ci-web-e2e: failure/);
});
