// Scenarios for check-tiering.mjs: a sound model passes, and each way the tiers
// can drift fails with its own message. The repository itself is checked last.

import assert from "node:assert/strict";
import { test } from "node:test";

import { check, checkDesktopPackage, checkGovernedConformance, GOVERNED_MUST_REACH, checkGoDuplicates, checkLoadGateMigrations, LOADGATE_MIGRATIONS, checkWebUnitSuite, GO_RUNS, load, main, DESKTOP_PACKAGE_PATHS, WEB_COVERAGE_RUNS, DRAFT_CONDITION, FAST_GATE_CONDITION, GATE_DRAFT_SKIP, MINIMUM_CALL_JOBS } from "./check-tiering.mjs";

const call = (id, extra = {}) => ({ needs: ["changes"], if: `fromJSON(needs.changes.outputs.run)['${id}']`, uses: `./.github/workflows/${id}.yml`, ...extra });
const heavyCall = (id) =>
  call(id, { needs: ["changes", "fast-gate"], if: `${FAST_GATE_CONDITION} && ${DRAFT_CONDITION} && fromJSON(needs.changes.outputs.run)['${id}']` });

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
  jobs["ci-gate"] = { if: `always() && ${GATE_DRAFT_SKIP}`, needs: Object.keys(jobs) };
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

test("a heavy job that leans on the implicit success() is refused", () => {
  // #2295: a ready run reused the fast tier, so the fast jobs were skipped; through
  // fast-gate they are ancestors, and the implicit success() skipped every heavy job.
  const m = model();
  m.ci.jobs["heavy-1"].if = `${DRAFT_CONDITION} && fromJSON(needs.changes.outputs.run)['heavy-1']`;
  has(check(m), "heavy-1: a heavy-tier job's if has no");
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

test("ci-gate is skipped on a draft, not failed", () => {
  const m = model();
  m.ci.jobs["ci-gate"].if = "always()";
  has(check(m), "ci-gate must be skipped on a draft");
  // == false would also skip an event that has no pull request; != true runs it.
  m.ci.jobs["ci-gate"].if = "always() && github.event.pull_request.draft == false";
  has(check(m), "ci-gate must be skipped on a draft");
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

function unionModel() {
  const m = model();
  m.tiers.jobs["heavy-0"] = { tier: "heavy", union: ["heavy-1", "heavy-2"] };
  for (const id of ["heavy-1", "heavy-2"]) m.ci.jobs[id].needs = ["changes", "fast-gate", "heavy-0"];
  return m;
}

test("a union entry whose consumers are its members is sound", () => {
  assert.deepEqual(check(unionModel()), []);
});

test("a job that needs a union job but is not in its union is refused", () => {
  const m = unionModel();
  m.ci.jobs["heavy-3"].needs = ["changes", "fast-gate", "heavy-0"];
  has(check(m), "heavy-3: needs heavy-0 but is not in its union");
});

test("a union names jobs with entries of their own, one level, and has no paths", () => {
  const m = unionModel();
  m.tiers.jobs["heavy-0"].union.push("ghost", "heavy-4");
  m.tiers.jobs["heavy-4"] = { tier: "heavy", union: ["heavy-5"] };
  m.tiers.jobs["heavy-0"].paths = ["x/**"];
  const errors = check(m);
  has(errors, "heavy-0: its union names ghost, which has no entry");
  has(errors, "heavy-0: its union names heavy-4, another union");
  has(errors, "heavy-0: has both union and paths");
  m.tiers.jobs["heavy-0"] = { tier: "heavy", union: [] };
  has(check(m), "heavy-0: its union must be a non-empty list");
});

test("a parser that finds nothing fails", () => {
  const m = model();
  m.ci = { jobs: {} };
  has(check(m), "expected at least");
});

function packageModel() {
  const pr = "${{ github.event_name == 'pull_request' }}";
  return {
    ci: { jobs: { "desktop-package": { needs: ["changes", "fast-gate"], uses: "./.github/workflows/desktop-package.yml", with: { require_signed_macos: false, require_signed_windows: false } } } },
    tiers: { jobs: { "desktop-package": { tier: "heavy", paths: [...DESKTOP_PACKAGE_PATHS].reverse() } } },
    workflows: {
      "desktop-package.yml": {
        parsed: {
          jobs: {
            macos: { steps: [{ uses: "./.github/actions/desktop-macos-package", with: { "dry-run": pr, debug: pr } }] },
            windows: { steps: [{ uses: "./.github/actions/desktop-windows-package", with: { "dry-run": pr } }] },
            linux: { steps: [{ uses: "./.github/actions/desktop-linux-package", with: {} }] },
          },
        },
        text: "",
      },
      "publish.yml": {
        parsed: { jobs: { desktop: { uses: "./.github/workflows/desktop-package.yml", with: { require_signed_macos: true, require_signed_windows: false, upload_release_assets: true } } } },
        text: "",
      },
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
  m.ci.jobs["desktop-package"].with.require_signed_macos = true;
  m.ci.jobs["desktop-package"].with.upload_release_assets = true;
  m.workflows["desktop-package.yml"].parsed.jobs.macos.steps[0].with.debug = "false";
  m.workflows["desktop-package.yml"].parsed.jobs.macos.steps[0].with["dry-run"] = "true";
  m.workflows["publish.yml"].parsed.jobs.desktop.with.require_signed_macos = false;
  const errors = checkDesktopPackage(m);
  has(errors, "ci.yml must pass `require_signed_macos: false`");
  has(errors, "ci.yml must not pass `upload_release_assets: true`");
  has(errors, "the `macos` package step's `debug` must be");
  has(errors, "the `macos` package step's `dry-run` must be");
  has(errors, "publish.yml: the desktop-package call must pass `require_signed_macos: true`");
  delete m.ci.jobs["desktop-package"];
  has(checkDesktopPackage(m), "ci.yml has no `desktop-package` job");
});

test("the Windows package gate: on in every call, a dry run on a pull request, its signing stated in publish.yml", () => {
  const m = packageModel();
  m.ci.jobs["desktop-package"].with.require_signed_windows = true;
  m.workflows["desktop-package.yml"].parsed.jobs.windows.if = "vars.DESKTOP_WINDOWS_PACKAGE == 'true'";
  m.workflows["desktop-package.yml"].parsed.jobs.windows.steps[0].with["dry-run"] = "false";
  delete m.workflows["publish.yml"].parsed.jobs.desktop.with.require_signed_windows;
  delete m.workflows["publish.yml"].parsed.jobs.desktop.with.upload_release_assets;
  const errors = checkDesktopPackage(m);
  has(errors, "ci.yml must pass `require_signed_windows: false`");
  has(errors, "the `windows` job has an `if:`");
  has(errors, "the `windows` package step's `dry-run` must be");
  has(errors, "must state `require_signed_windows`");
  has(errors, "must pass `upload_release_assets: true`");
  // A Windows signer later flips it to true: still a sound model.
  const signed = packageModel();
  signed.workflows["publish.yml"].parsed.jobs.desktop.with.require_signed_windows = true;
  assert.deepEqual(checkDesktopPackage(signed), []);
  const gone = packageModel();
  delete gone.workflows["desktop-package.yml"].parsed.jobs.windows;
  has(checkDesktopPackage(gone), "the `windows` job does not use ./.github/actions/desktop-windows-package");
  // The Linux job (#3034) is held the same way: there, on every call.
  const noLinux = packageModel();
  delete noLinux.workflows["desktop-package.yml"].parsed.jobs.linux;
  has(checkDesktopPackage(noLinux), "the `linux` job does not use ./.github/actions/desktop-linux-package");
  const gatedLinux = packageModel();
  gatedLinux.workflows["desktop-package.yml"].parsed.jobs.linux.if = "vars.DESKTOP_LINUX == 'on'";
  has(checkDesktopPackage(gatedLinux), "the `linux` job has an `if:`");
});

test("one web unit suite: the plain shards stand down only where coverage runs", () => {
  const web = (unitIf) => ({ "ci-web.yml": { parsed: { jobs: { "unit-tests": { if: unitIf } } } } });
  const ci = (coverageRuns, coverageIf = `${FAST_GATE_CONDITION} && ${DRAFT_CONDITION} && fromJSON(needs.changes.outputs.run)['ci-web-coverage']`) => ({
    jobs: { "ci-web": { with: coverageRuns === undefined ? undefined : { coverage_runs: coverageRuns } }, "ci-web-coverage": { if: coverageIf } },
  });
  const ok = "${{ !inputs.coverage_runs }}";
  assert.deepEqual(checkWebUnitSuite(ci(WEB_COVERAGE_RUNS), web(ok)), []);
  // No input: the shards would never be told; another filter: a run with neither suite.
  assert.match(checkWebUnitSuite(ci(undefined), web(ok)).join("\n"), /coverage_runs/);
  assert.match(checkWebUnitSuite(ci(WEB_COVERAGE_RUNS.replace("ci-web-coverage", "ci-web")), web(ok)).join("\n"), /coverage_runs/);
  // The coverage job lost its draft rule: the input no longer mirrors it.
  assert.match(checkWebUnitSuite(ci(WEB_COVERAGE_RUNS, "fromJSON(needs.changes.outputs.run)['ci-web-coverage']"), web(ok)).join("\n"), /ci-web-coverage/);
  // The shard job reads something else, or nothing.
  assert.match(checkWebUnitSuite(ci(WEB_COVERAGE_RUNS), web(undefined)).join("\n"), /unit-tests/);
  assert.match(checkWebUnitSuite(ci(WEB_COVERAGE_RUNS), web("${{ !inputs.coverage_runs || true }}")).join("\n"), /unit-tests/);
});

test("duplicate Go tests run only where ci-go does not", () => {
  const ok = "${{ !cancelled() && !inputs.go_runs }}";
  const model = ({ goIf = "fromJSON(needs.changes.outputs.run)['ci-go']", nb = GO_RUNS, stepIf = ok, steps = 2, unless = "ci-go" } = {}) => [
    { jobs: { "ci-go": { if: goIf }, "no-binaries": { with: { go_runs: nb } }, "ci-contract": {} } },
    {
      "no-binaries.yml": { parsed: { jobs: { gates: { steps: [
        { name: "edges", if: stepIf, run: "go test -count=1 ./tests/deployedge/..." },
        { name: "grants", if: stepIf, run: "go test -count=1 ./internal/api/" },
      ].slice(0, steps) } } } },
    },
    { jobs: { "ci-contract": { unless } } },
  ];
  assert.deepEqual(checkGoDuplicates(...model()), []);
  assert.match(checkGoDuplicates(...model({ nb: null })).join("\n"), /no-binaries: ci.yml must pass/);
  assert.match(checkGoDuplicates(...model({ unless: null })).join("\n"), /ci-contract needs/);
  assert.match(checkGoDuplicates(...model({ goIf: "false" })).join("\n"), /ci-go: its if/);
  assert.match(checkGoDuplicates(...model({ stepIf: "${{ !cancelled() }}" })).join("\n"), /edges/);
  assert.match(checkGoDuplicates(...model({ stepIf: "${{ !inputs.go_runs || true }}" })).join("\n"), /grants/);
  assert.match(checkGoDuplicates(...model({ steps: 1 })).join("\n"), /1 of the 2/);
});

test("the repository passes", () => {
  const model = load();
  const errors = [...check(model), ...checkDesktopPackage(model)];
  assert.deepEqual(errors, []);
  assert.equal(main({ out: () => {}, err: () => {} }), 0);
});

test("the load gate starts on exactly the migrations that name the publish tables (#2940)", () => {
  const dir = LOADGATE_MIGRATIONS;
  const migrations = {
    // Mentions a publish table only in comments: not a publish migration.
    "0001_auth.sql": "-- buckets of time\nALTER TABLE elitea_auth.users ADD COLUMN x int; /* not elitea_storage.buckets */",
    "0100_workspace.sql": "CREATE INDEX ON elitea_runtime.workspace_segments (seq);",
  };
  const paths = [".github/workflows/ci-handoff-loadgate.yml", `${dir}/0100_workspace.sql`];
  const tiers = (list) => ({ jobs: { "ci-handoff-loadgate": { tier: "heavy", paths: list } } });
  assert.deepEqual(checkLoadGateMigrations(tiers(paths), migrations), []);
  // A new migration on a publish table that the filter misses is refused, in
  // any spelling of the table: qualified, after SET search_path, quoted.
  for (const sql of [
    "ALTER TABLE elitea_storage.transfer_grants ADD COLUMN y int;",
    "SET search_path = elitea_runtime;\nALTER TABLE workspace_refs ADD COLUMN y int;",
    'CREATE TRIGGER t BEFORE INSERT ON elitea_storage."Buckets" FOR EACH ROW EXECUTE FUNCTION f();',
  ]) {
    has(checkLoadGateMigrations(tiers(paths), { ...migrations, "0200_new.sql": sql }), `${dir}/0200_new.sql names a table of the handoff publish path`);
  }
  // Any wildcard that reaches every shared migration is the old filter back.
  for (const wildcard of [`${dir}/**`, "services/elitea-main/migrations/**", "services/elitea-main/**", "**"]) {
    has(checkLoadGateMigrations(tiers([...paths, wildcard]), migrations), "wildcard");
  }
  // A listed file that names no publish table, or no longer exists, is refused.
  has(checkLoadGateMigrations(tiers([...paths, `${dir}/0001_auth.sql`]), migrations), "names no table of the publish path");
  has(checkLoadGateMigrations(tiers([...paths, `${dir}/0999_gone.sql`]), migrations), "does not exist");
  // A scan that finds nothing fails, and so does an empty or missing list.
  has(checkLoadGateMigrations(tiers(paths), { "0001_auth.sql": migrations["0001_auth.sql"] }), "the scan stopped finding them");
  has(checkLoadGateMigrations(tiers([]), migrations), "has no paths list");
  has(checkLoadGateMigrations({ jobs: {} }, migrations), "has no paths list");
});

test("the repository's load gate filter is exact", () => {
  const model = load();
  assert.deepEqual(checkLoadGateMigrations(model.tiers, model.migrations), []);
});

test("ci-governed-conformance: the worker's egress, governed and MCP code and the governed compose file start it (#2981)", () => {
  assert.deepEqual(checkGovernedConformance(load().tiers), []);
  // The filter before #2983: PR #2976's egress-guard and toolkits/mcp/governed.rs did not start it.
  const old = { jobs: { "ci-governed-conformance": { paths: ["conformance/governed/**", "deploy/docker-compose.governed.yml", "deploy/governed/**"] } } };
  const missed = checkGovernedConformance(old).join("\n");
  assert.equal(checkGovernedConformance(old).length, GOVERNED_MUST_REACH.length - 1);
  assert.match(missed, /egress-guard\/src\/private\.rs/);
  assert.match(missed, /toolkits\/mcp\/governed\.rs/);
  assert.deepEqual(checkGovernedConformance({ jobs: {} }), ["ci-governed-conformance: tier-paths.json has no path list"]);
});
