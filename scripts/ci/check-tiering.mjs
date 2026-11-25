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
//   * `ci-gate` or `fast-gate` does not run with `if: always()`, `ci-gate` is not
//     skipped on a draft (GATE_DRAFT_SKIP), or `ci-gate`
//     does not list every other job of the workflow (a job missing from `needs`
//     cannot fail the verdict);
//   * a job that calls a workflow has no entry (tier and paths) in
//     scripts/ci/tier-paths.json, or an entry has no job, or the tier in the file
//     disagrees with the job's `needs`;
//   * a `union` entry (a job that runs when any of the listed jobs does,
//     ci-build-once) names a job without its own entry or another union, also
//     has paths, or a job that needs it is not in it;
//   * a workflow ci.yml calls has no `workflow_call` arm or still starts on its
//     own `pull_request`, or a workflow outside the allowlist starts on a pull
//     request without being called by ci.yml (a new workflow would run untiered);
//   * a called workflow's concurrency group is built from `github.workflow` (in a
//     called workflow that is the CALLER's name, so every called workflow shares
//     one group and they cancel each other).
//   * the handoff load gate's path list misses a shared migration whose SQL
//     names a table of the publish transaction, lists one that names none, or
//     falls back to a wildcard of migrations/shared (checkLoadGateMigrations, #2940).
//
// And the desktop package job (`desktop-package`, checkDesktopPackage) keeps its gate:
// on a pull request it runs only for DESKTOP_PACKAGE_PATHS (exactly those), as an
// unsigned dry run of both platforms (macOS debug) that uploads nothing; publish.yml
// (the nightly on main and every release) runs it in full and attaches the packages
// (`upload_release_assets: true`): macOS signed and verified
// (`require_signed_macos: true`), Windows with `require_signed_windows` stated (false
// until a Windows signer exists, owner decision 2026-11-22). The Windows job has no
// `if:`: it runs wherever the macOS one does.
//
// Reads YAML with the `yaml` package (./ci-compat.mjs; `npm ci --prefix scripts/ci`)
// and runs with the runner's node. It fails when it finds fewer jobs than its
// floor: "found nothing, therefore clean" is how a guard stops guarding.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { loadYaml, isEntrypoint } from "./ci-compat.mjs";
import { reaches } from "./tier-changes.mjs";

const SELF = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SELF), "..", "..");

export const MINIMUM_CALL_JOBS = 20;
/** Workflows that keep their own pull-request trigger, and why. */
export const OWN_PR_TRIGGER = new Map([
  ["ci.yml", "the orchestrator itself"],
  ["mirror-buildkit.yml", "starts only when its own file changes; its `packages: write` is not a gate"],
  ["cancel-merged-pr-runs.yml", "housekeeping on close, not a check"],
  ["eng-runner-image.yml", "starts only when the eng-runner image or its own file changes; it builds a CI runner image, not product code"],
]);
export const DRAFT_CONDITION = "github.event.pull_request.draft == false";
/** ci-gate's half of the draft rule: skipped on a draft, so a draft shows fast-gate's verdict and a grey gate. */
export const GATE_DRAFT_SKIP = "github.event.pull_request.draft != true";
// One web unit suite per run: ci-web's plain shards stand down exactly where ci-web-coverage
// runs every unit test. ci.yml passes ci-web the coverage job's own filter, and the shards
// are skipped on that input alone, so a run never has neither suite.
export const WEB_COVERAGE_RUNS = "${{ github.event.pull_request.draft == false && fromJSON(needs.changes.outputs.run)['ci-web-coverage'] }}";
export function checkWebUnitSuite(ci, workflows) {
  const errors = [];
  const call = ci.jobs?.["ci-web"];
  if (!call) return errors;
  if (String(call.with?.coverage_runs ?? "") !== WEB_COVERAGE_RUNS) errors.push(`ci-web: ci.yml must pass \`coverage_runs: ${WEB_COVERAGE_RUNS}\` (the plain unit shards stand down only where ci-web-coverage runs)`);
  const coverage = ci.jobs?.["ci-web-coverage"];
  if (!/github\.event\.pull_request\.draft == false/.test(String(coverage?.if ?? "")) || !/\['ci-web-coverage'\]/.test(String(coverage?.if ?? ""))) errors.push("ci-web-coverage: its if no longer reads `draft == false` and its own filter; ci-web's coverage_runs must follow it");
  const unit = workflows["ci-web.yml"]?.parsed?.jobs?.["unit-tests"];
  if (unit && String(unit.if ?? "") !== "${{ !inputs.coverage_runs }}") errors.push("ci-web.yml: `unit-tests` must be `if: ${{ !inputs.coverage_runs }}` and nothing else");
  return errors;
}

/**
 * The pull-request gate of the macOS package job (owner decision 2026-11-20): the
 * desktop app (its Tauri config and packaging scripts are under it), the vendored
 * agent-desktop it links, the toolchain pin the packaging action asserts, and the
 * packaging workflow and actions. Not the other libs/rust crates the host links:
 * the dry run is a debug build, and ci-desktop.yml's macOS host job already
 * builds and tests that same debug graph for them. Exact: a path
 * added here runs a 20-minute macOS job on every pull request that touches it.
 */
export const DESKTOP_PACKAGE_PATHS = [
  "apps/elitea-desktop/**",
  "libs/rust/vendor/agent-desktop/**",
  "rust-toolchain.toml",
  "scripts/ci/assert-rust-toolchain.sh",
  ".github/workflows/desktop-package.yml",
  ".github/actions/desktop-macos-package/**",
  ".github/actions/desktop-windows-package/**",
  ".github/actions/desktop-linux-package/**",
  // The dry run is the desktop lane of the brand output scan (brand/sample).
  "brand/**",
  "scripts/brand/**",
  "scripts/ci/brand-output-scan.mjs",
];
const PR_ONLY = "${{ github.event_name == 'pull_request' }}";
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
  // A `union` entry (ci-build-once) runs when any member does. Its members are
  // jobs with paths, and every job that needs it is a member: a consumer left
  // out of the union could run on a change that builds nothing for it.
  for (const [id, entry] of Object.entries(tierJobs)) {
    if (entry.union === undefined) continue;
    if (!Array.isArray(entry.union) || entry.union.length === 0) {
      errors.push(`${id}: its union must be a non-empty list of job ids`);
      continue;
    }
    if (entry.paths !== undefined) errors.push(`${id}: has both union and paths; a union entry runs when a member does, from no paths of its own`);
    for (const member of entry.union) {
      if (!tierJobs[member]) errors.push(`${id}: its union names ${member}, which has no entry in tier-paths.json`);
      else if (tierJobs[member].union !== undefined) errors.push(`${id}: its union names ${member}, another union (one level only)`);
    }
    for (const [other, job] of Object.entries(jobs)) {
      if (other === "ci-gate" || !asList(job.needs).includes(id)) continue;
      if (!entry.union.includes(other)) errors.push(`${other}: needs ${id} but is not in its union in tier-paths.json, so it can run when ${id} does not`);
    }
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
    // Skipped on a draft (grey, not red); `!= true` keeps an event without a pull request running it.
    if (!String(final.if ?? "").includes(GATE_DRAFT_SKIP)) errors.push(`ci-gate must be skipped on a draft: its if needs \`${GATE_DRAFT_SKIP}\``);
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

// ci-go's shards run no-binaries' two elitea-main gates (api-core, infra-suites) and
// ci-contract's tests (db-storage). Those run there only when ci-go does not: ci.yml passes
// no-binaries ci-go's own filter as `go_runs` and its two steps skip on it; ci-contract
// carries `unless: ci-go` in tier-paths.json.
export const GO_RUNS = "${{ fromJSON(needs.changes.outputs.run)['ci-go'] }}";
const GO_DUPLICATES = [
  ["no-binaries", "no-binaries.yml", (wf) => Object.values(wf?.jobs ?? {}).flatMap((j) => j.steps ?? []).filter((s) => /go test -count=1 \.\/(?:internal\/api\/|tests\/deployedge\/)/.test(String(s.run ?? ""))).map((s) => [s.name, s.if])],
];
export function checkGoDuplicates(ci, workflows, tiers = { jobs: {} }) {
  const errors = [];
  // ci-contract does not start at all where ci-go runs: tier-paths.json `unless`, so the
  // changes output says false and the gates read its skip as correct, not missing.
  if (tiers.jobs?.["ci-contract"] && tiers.jobs["ci-contract"].unless !== "ci-go") errors.push("tier-paths.json: ci-contract needs `\"unless\": \"ci-go\"` (its tests are ci-go's db-storage shard)");
  if (String(ci.jobs?.["ci-go"]?.if ?? "") !== "fromJSON(needs.changes.outputs.run)['ci-go']") errors.push("ci-go: its if is no longer its own filter alone; no-binaries' and ci-contract's `go_runs` must follow it");
  for (const [job, file, gated] of GO_DUPLICATES) {
    if (!ci.jobs?.[job]) continue;
    if (String(ci.jobs[job].with?.go_runs ?? "") !== GO_RUNS) errors.push(`${job}: ci.yml must pass \`go_runs: ${GO_RUNS}\` (its duplicate Go tests run only where ci-go does not)`);
    const items = gated(workflows[file]?.parsed);
    if (job === "no-binaries" && items.length !== 2) errors.push(`no-binaries.yml: found ${items.length} of the 2 elitea-main gate steps (./internal/api/, ./tests/deployedge/...)`);
    for (const [name, cond] of items) {
      const c = String(cond ?? "");
      if (!/!inputs\.go_runs\b/.test(c) || /\|\|/.test(c)) errors.push(`${file}: \`${name}\` must skip on \`!inputs.go_runs\` (no \`||\`): ci-go runs the same tests`);
    }
  }
  return errors;
}

/**
 * The governed stack runs the worker's egress, governed and MCP code against its own
 * test hosts. PR #2976 changed that code (the private-address refusal, egress-guard and
 * toolkits/mcp/governed.rs), ci-governed-conformance was skipped by its path filter,
 * and the stack's hosts were then refused as private addresses on main (#2981). Each
 * file below stands for a part of that code and must reach the job.
 */
export const GOVERNED_MUST_REACH = [
  "libs/rust/egress-guard/src/private.rs",
  "libs/rust/egress-guard/src/resolver.rs",
  "libs/rust/agent-runtime/src/egress.rs",
  "libs/rust/agent-runtime/src/toolkits/mcp.rs",
  "libs/rust/agent-runtime/src/toolkits/mcp/governed.rs",
  "libs/rust/agent-runtime/src/toolkits/governed/adapters/x.rs",
  "services/elitea-worker-rust/src/protocol/egress.rs",
  "services/elitea-worker-rust/src/indexing/governed.rs",
  "services/elitea-worker-rust/src/execution/governed_direct.rs",
  "services/elitea-worker-rust/src/agents/governed_nested.rs",
  "deploy/docker-compose.governed.yml",
];

/** Every GOVERNED_MUST_REACH file reaches ci-governed-conformance. */
export function checkGovernedConformance(tiers) {
  const paths = tiers.jobs?.["ci-governed-conformance"]?.paths;
  if (!Array.isArray(paths)) return ["ci-governed-conformance: tier-paths.json has no path list"];
  return GOVERNED_MUST_REACH.filter((file) => !reaches([file], paths)).map(
    (file) => `ci-governed-conformance: a change to ${file} does not start it; the worker's egress, governed and MCP code runs on the governed stack (#2981)`,
  );
}

/**
 * The desktop package job keeps its gate: exact pull-request paths, a dry run of both
 * platforms (and a macOS debug build) on a pull request only, and the full run from
 * publish.yml that attaches the packages, macOS signed.
 * @param {{ci: any, tiers: any, workflows: Record<string, {parsed: any, text: string}>}} model
 * @returns {string[]}
 */
export function checkDesktopPackage({ ci, tiers, workflows }) {
  const errors = [];
  const job = ci.jobs?.["desktop-package"];
  if (!job) return ["ci.yml has no `desktop-package` job (the macOS package dry run)"];
  const paths = tiers.jobs?.["desktop-package"]?.paths;
  const want = [...DESKTOP_PACKAGE_PATHS].sort();
  const got = Array.isArray(paths) ? [...paths].sort() : paths;
  if (JSON.stringify(got) !== JSON.stringify(want)) {
    errors.push(`desktop-package: tier-paths.json paths are ${JSON.stringify(paths)}, not exactly ${JSON.stringify(DESKTOP_PACKAGE_PATHS)} (check-tiering.mjs DESKTOP_PACKAGE_PATHS)`);
  }
  for (const os of ["macos", "windows"]) {
    if (job.with?.[`require_signed_${os}`] !== false) errors.push(`desktop-package: ci.yml must pass \`require_signed_${os}: false\` (a pull request is a dry run)`);
  }
  if (job.with?.upload_release_assets === true) errors.push("desktop-package: ci.yml must not pass `upload_release_assets: true` (a pull request uploads nothing)");
  const jobs = workflows["desktop-package.yml"]?.parsed?.jobs ?? {};
  const packaging = { macos: ["./.github/actions/desktop-macos-package", ["dry-run", "debug"]], windows: ["./.github/actions/desktop-windows-package", ["dry-run"]], linux: ["./.github/actions/desktop-linux-package", []] };
  for (const [name, [action, inputs]] of Object.entries(packaging)) {
    const pkg = (jobs[name]?.steps ?? []).find((s) => String(s.uses ?? "") === action);
    if (!pkg) {
      errors.push(`desktop-package.yml: the \`${name}\` job does not use ${action}`);
      continue;
    }
    if (jobs[name].if !== undefined) errors.push(`desktop-package.yml: the \`${name}\` job has an \`if:\` (${jobs[name].if}); it runs on every call (owner decision 2026-11-22)`);
    for (const input of inputs) {
      if (String(pkg.with?.[input] ?? "") !== PR_ONLY) errors.push(`desktop-package.yml: the \`${name}\` package step's \`${input}\` must be \`${PR_ONLY}\` (pull requests only; main and releases build in full)`);
    }
  }
  const publish = Object.values(workflows["publish.yml"]?.parsed?.jobs ?? {}).filter((j) => j.uses === "./.github/workflows/desktop-package.yml");
  if (publish.length !== 1) errors.push(`publish.yml: ${publish.length} job(s) call desktop-package.yml, expected 1 (the nightly and release packages)`);
  for (const j of publish) {
    if (j.with?.require_signed_macos !== true) errors.push("publish.yml: the desktop-package call must pass `require_signed_macos: true` (signed, notarised and verified)");
    if (typeof j.with?.require_signed_windows !== "boolean") errors.push("publish.yml: the desktop-package call must state `require_signed_windows` (false until a Windows signer exists)");
    if (j.with?.upload_release_assets !== true) errors.push("publish.yml: the desktop-package call must pass `upload_release_assets: true` (the packages are attached to the release)");
  }
  return errors;
}

/**
 * The handoff publish load gate's migrations (#2940). ci-handoff-loadgate
 * starts on the shared migrations that can change the publish path, by name,
 * not on all of migrations/shared/**: a p99 gate on the shared pool reads a
 * runner stall as a regression, so it should not run for an auth column. The
 * list is exact only while every migration that names a table of the publish
 * transaction is in it, so this derives that set from the SQL and refuses a
 * migration the filter misses (a new one fails here until it is added).
 */
export const LOADGATE_JOB = "ci-handoff-loadgate";
export const LOADGATE_MIGRATIONS = "services/elitea-main/migrations/shared";
/**
 * The tables the publish reads and writes (internal/infra/db/repos/workspace_store.go),
 * as an identifier in any form: schema-qualified or not (after a SET
 * search_path), quoted or not, any case. Matched on the SQL with its comments
 * removed, so a comment that mentions "buckets" does not count.
 */
export const LOADGATE_TABLES = /(?<![\w$])"?(?:workspace_(?:segments|refs|handoffs)|transfer_grants|buckets)"?(?![\w$])/i;
/** A shared migration that names no publish table: the job must not start on it. */
export const LOADGATE_SENTINEL = `${LOADGATE_MIGRATIONS}/9999_sentinel_names_no_publish_table.sql`;
export function stripSqlComments(sql) {
  return String(sql).replace(/\/\*[\s\S]*?\*\//g, " ").replace(/--[^\n]*/g, " ");
}
const namesPublishTable = (sql) => LOADGATE_TABLES.test(stripSqlComments(sql));
export function checkLoadGateMigrations(tiers, migrations) {
  const errors = [];
  const paths = tiers.jobs?.[LOADGATE_JOB]?.paths;
  if (!Array.isArray(paths) || paths.length === 0) return [`${LOADGATE_JOB}: tier-paths.json has no paths list`];
  // Any pattern that reaches a migration naming no publish table (a wildcard
  // of the directory, of migrations/**, of the service) is the old filter back.
  if (reaches([LOADGATE_SENTINEL], paths)) {
    errors.push(`${LOADGATE_JOB}: its paths reach every shared migration (a wildcard); list the migrations that name the publish tables instead (#2940)`);
  }
  const relevant = Object.entries(migrations).filter(([, sql]) => namesPublishTable(sql)).map(([name]) => `${LOADGATE_MIGRATIONS}/${name}`);
  if (relevant.length === 0) {
    errors.push(`${LOADGATE_JOB}: found no shared migration naming the publish tables: the scan stopped finding them`);
  }
  for (const file of relevant) {
    if (!reaches([file], paths)) errors.push(`${LOADGATE_JOB}: ${file} names a table of the handoff publish path; add it to the job's paths in tier-paths.json`);
  }
  // Every listed migration must exist and name a publish table: a scan that
  // broke would refuse every listed file here, so the list is its own floor.
  for (const pattern of paths) {
    if (!pattern.startsWith(`${LOADGATE_MIGRATIONS}/`) || pattern.includes("*")) continue;
    const name = pattern.slice(LOADGATE_MIGRATIONS.length + 1);
    if (!(name in migrations)) errors.push(`${LOADGATE_JOB}: lists ${pattern}, which does not exist`);
    else if (!namesPublishTable(migrations[name])) errors.push(`${LOADGATE_JOB}: lists ${pattern}, which names no table of the publish path`);
  }
  return errors;
}

function loadMigrations(root) {
  const directory = path.join(root, LOADGATE_MIGRATIONS);
  const migrations = {};
  for (const name of fs.readdirSync(directory).filter((n) => n.endsWith(".sql")).sort()) {
    migrations[name] = fs.readFileSync(path.join(directory, name), "utf8");
  }
  return migrations;
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
  return { ci, tiers, workflows, migrations: loadMigrations(root) };
}

export function main({ root = ROOT, out = console.log, err = console.error } = {}) {
  let model;
  try {
    model = load(root);
  } catch (error) {
    err(`::error::${error.message}`);
    return 1;
  }
  const errors = [...check(model), ...checkDesktopPackage(model), ...checkGovernedConformance(model.tiers), ...checkWebUnitSuite(model.ci, model.workflows), ...checkGoDuplicates(model.ci, model.workflows, model.tiers), ...checkLoadGateMigrations(model.tiers, model.migrations)];
  if (errors.length) {
    for (const e of errors) err(`::error::${e}`);
    return 1;
  }
  out(`the tiers are sound: ${Object.keys(model.tiers.jobs).length} jobs, every one in ci-gate`);
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
