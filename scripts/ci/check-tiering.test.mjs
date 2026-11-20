// Scenarios for check-tiering.mjs: a sound model passes, and each way the tiers
// can drift fails with its own message. The repository itself is checked last.

import assert from "node:assert/strict";
import { test } from "node:test";

import { check, checkDesktopPackage, load, main, DESKTOP_PACKAGE_PATHS, DRAFT_CONDITION, MINIMUM_CALL_JOBS } from "./check-tiering.mjs";

const call = (id, extra = {}) => ({ needs: ["changes"], if: `fromJSON(needs.changes.outputs.run)['${id}']`, uses: `./.github/workflows/${id}.yml`, ...extra });
const heavyCall = (id) =>
  call(id, { needs: ["changes", "fast-gate"], if: `${DRAFT_CONDITION} && fromJSON(needs.changes.outputs.run)['${id}']` });

function model() {
  const fast = Array.from({ length: MINIMUM_CALL_JOBS / 2 }, (_, i) => `fast-${i}`);
  const heavy = Array.from({ length: MINIMUM_CALL_JOBS / 2 }, (_, i) => `heavy-${i}`);
  const jobs = { changes: { "runs-on": "x" } };
  const tierJobs = {};
  const workflows = {
    "ci.yml": { parsed: { on: { pull_request: {} } }, text: "" },
  };
  for (const id of fast) {
    jobs[id] = call(id);
    tierJobs[id] = { tier: "fast", paths: null };
  }
  jobs["fast-gate"] = { if: "always()", needs: ["changes", ...fast] };
  for (const id of heavy) {
    jobs[id] = heavyCall(id);
    tierJobs[id] = { tier: "heavy", paths: ["x/**"] };
  }
  jobs["ci-gate"] = { if: "always()", needs: Object.keys(jobs) };
  for (const id of [...fast, ...heavy]) {
    workflows[`${id}.yml`] = { parsed: { on: { workflow_call: null, workflow_dispatch: null } }, text: `concurrency:\n  group: ${id}-\${{ github.ref }}\n` };
  }
  return { ci: { jobs }, tiers: { jobs: tierJobs }, workflows };
}

const has = (errors, fragment) => assert.ok(errors.some((e) => e.includes(fragment)), `${fragment} in ${JSON.stringify(errors)}`);

test("a sound model has no problems", () => {
  assert.deepEqual(check(model()), []);
});

test("a heavy job without the draft condition is refused", () => {
  const m = model();
  m.ci.jobs["heavy-0"].if = "fromJSON(needs.changes.outputs.run)['heavy-0']";
  has(check(m), "heavy-0: a heavy-tier job has no");
});

test("ci-gate must list every job", () => {
  const m = model();
  m.ci.jobs["ci-gate"].needs = m.ci.jobs["ci-gate"].needs.filter((n) => n !== "heavy-3");
  has(check(m), "ci-gate: needs does not list heavy-3");
});

test("ci-gate and fast-gate run with always()", () => {
  const m = model();
  m.ci.jobs["ci-gate"].if = "success()";
  m.ci.jobs["fast-gate"].if = undefined;
  const errors = check(m);
  has(errors, "ci-gate must run with");
  has(errors, "fast-gate must run with");
});

test("fast-gate lists every fast job and no heavy one", () => {
  const m = model();
  m.ci.jobs["fast-gate"].needs = ["changes", "heavy-1"];
  const errors = check(m);
  has(errors, "fast-gate: needs does not list fast-2");
  has(errors, "fast-gate: needs the heavy job heavy-1");
});

test("a job missing from tier-paths.json, and one with no job, are refused", () => {
  const m = model();
  delete m.tiers.jobs["fast-1"];
  m.tiers.jobs.ghost = { tier: "fast", paths: null };
  const errors = check(m);
  has(errors, "fast-1: a job that calls a workflow has no entry");
  has(errors, "ghost: in tier-paths.json but ci.yml has no such job");
});

test("the tier in the file must agree with the needs", () => {
  const m = model();
  m.tiers.jobs["heavy-2"].tier = "fast";
  has(check(m), 'heavy-2: tier-paths.json says "fast"');
});

test("a called workflow keeps no pull_request arm and needs workflow_call", () => {
  const m = model();
  m.workflows["fast-0.yml"].parsed = { on: { pull_request: {}, workflow_dispatch: null } };
  const errors = check(m);
  has(errors, "fast-0.yml: called by ci.yml but has no `workflow_call`");
  has(errors, "fast-0.yml: called by ci.yml but still starts on its own `pull_request`");
});

test("a workflow that starts on a pull request outside ci.yml is refused, the allowlist is not", () => {
  const m = model();
  m.workflows["new.yml"] = { parsed: { on: { pull_request: {} } }, text: "" };
  m.workflows["mirror-buildkit.yml"] = { parsed: { on: { pull_request: {} } }, text: "" };
  const errors = check(m);
  has(errors, "new.yml: starts on a pull request but is not a job of ci.yml");
  assert.equal(errors.filter((e) => e.includes("mirror-buildkit")).length, 0);
});

test("a concurrency group built from github.workflow is refused in a called workflow", () => {
  const m = model();
  m.workflows["fast-4.yml"].text = "concurrency:\n  group: ${{ github.workflow }}-${{ github.ref }}\n  cancel-in-progress: true\n";
  has(check(m), "fast-4.yml: its concurrency group uses github.workflow");
});

test("a parser that finds nothing fails", () => {
  const m = model();
  m.ci = { jobs: {} };
  has(check(m), "expected at least");
});

function packageModel() {
  const pr = "${{ github.event_name == 'pull_request' }}";
  return {
    ci: { jobs: { "desktop-package": { needs: ["changes", "fast-gate"], uses: "./.github/workflows/desktop-package.yml", with: { require_signed: false } } } },
    tiers: { jobs: { "desktop-package": { tier: "heavy", paths: [...DESKTOP_PACKAGE_PATHS].reverse() } } },
    workflows: {
      "desktop-package.yml": { parsed: { jobs: { macos: { steps: [{ uses: "./.github/actions/desktop-macos-package", with: { "dry-run": pr, debug: pr } }] } } }, text: "" },
      "publish.yml": { parsed: { jobs: { desktop: { uses: "./.github/workflows/desktop-package.yml", with: { require_signed: true } } } }, text: "" },
    },
  };
}

test("the macOS package gate: a sound model passes, in any path order", () => {
  assert.deepEqual(checkDesktopPackage(packageModel()), []);
});

test("the macOS package gate: a path added to or dropped from the PR filter is refused", () => {
  const added = packageModel();
  added.tiers.jobs["desktop-package"].paths.push("apps/elitea-web/**");
  has(checkDesktopPackage(added), "desktop-package: tier-paths.json paths are");
  const dropped = packageModel();
  dropped.tiers.jobs["desktop-package"].paths = DESKTOP_PACKAGE_PATHS.filter((p) => !p.includes("agent-desktop"));
  has(checkDesktopPackage(dropped), "not exactly");
  const always = packageModel();
  always.tiers.jobs["desktop-package"].paths = null;
  has(checkDesktopPackage(always), "not exactly");
});

test("the macOS package gate: a pull request stays a dry run, publish.yml stays signed", () => {
  const m = packageModel();
  m.ci.jobs["desktop-package"].with.require_signed = true;
  m.workflows["desktop-package.yml"].parsed.jobs.macos.steps[0].with.debug = "false";
  m.workflows["desktop-package.yml"].parsed.jobs.macos.steps[0].with["dry-run"] = "true";
  m.workflows["publish.yml"].parsed.jobs.desktop.with.require_signed = false;
  const errors = checkDesktopPackage(m);
  has(errors, "ci.yml must pass `require_signed: false`");
  has(errors, "the package step's `debug` must be");
  has(errors, "the package step's `dry-run` must be");
  has(errors, "publish.yml: the desktop-package call must pass `require_signed: true`");
  delete m.ci.jobs["desktop-package"];
  has(checkDesktopPackage(m), "ci.yml has no `desktop-package` job");
});

test("the repository passes", () => {
  const model = load();
  const errors = [...check(model), ...checkDesktopPackage(model)];
  assert.deepEqual(errors, []);
  assert.equal(main({ out: () => {}, err: () => {} }), 0);
});
