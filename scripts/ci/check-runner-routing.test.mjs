// Scenarios for check-runner-routing.mjs. The routing condition is evaluated as GitHub evaluates it (the
// small evaluator below covers the grammar the condition uses), so the truth table tests the exact string the
// workflows carry, not a JavaScript copy of it.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { check, classifyRunsOn, main, ROUTED_RUNS_ON, ROUTING_CONDITION, HOSTED_ONLY, ENV_ACTION } from "./check-runner-routing.mjs";

// ── A GitHub Actions expression evaluator, for the subset the condition uses ──
// Literals ('str', null, true, false, numbers), property paths, == != ! && || and parentheses. Semantics
// as documented: && and || return an operand, not a boolean; == between strings ignores case; between
// other types both sides are coerced to numbers (null → 0, '' → 0, a non-numeric string → NaN); a missing
// property is null.
function evaluate(source, context) {
  const tokens = source.match(/'(?:[^']|'')*'|[A-Za-z_][\w.\-[\]]*|==|!=|&&|\|\||[()!]|\d+(?:\.\d+)?|\S/g);
  let i = 0;
  const peek = () => tokens[i];
  const take = (t) => {
    if (t !== undefined && tokens[i] !== t) throw new Error(`expected ${t} at ${tokens[i]}`);
    return tokens[i++];
  };
  const truthy = (v) => !(v === null || v === false || v === 0 || v === "" || Number.isNaN(v));
  const num = (v) => (v === null ? 0 : typeof v === "boolean" ? Number(v) : typeof v === "string" ? (v.trim() === "" ? 0 : Number(v)) : v);
  const equal = (a, b) => {
    if (typeof a === "string" && typeof b === "string") return a.toLowerCase() === b.toLowerCase();
    if (typeof a === typeof b && a !== null && typeof a !== "number") return a === b;
    return num(a) === num(b);
  };
  const lookup = (pathText) => pathText.split(".").reduce((o, k) => (o === null || o === undefined ? null : (o[k] ?? null)), context);
  function primary() {
    const t = take();
    if (t === "(") {
      const v = or();
      take(")");
      return v;
    }
    if (t === "!") return !truthy(primary());
    if (t.startsWith("'")) return t.slice(1, -1).replace(/''/g, "'");
    if (t === "null") return null;
    if (t === "true") return true;
    if (t === "false") return false;
    if (/^\d/.test(t)) return Number(t);
    return lookup(t);
  }
  function comparison() {
    let v = primary();
    while (peek() === "==" || peek() === "!=") {
      const op = take();
      const r = primary();
      v = op === "==" ? equal(v, r) : !equal(v, r);
    }
    return v;
  }
  function and() {
    let v = comparison();
    while (peek() === "&&") {
      take();
      const r = comparison();
      v = truthy(v) ? r : v;
    }
    return v;
  }
  function or() {
    let v = and();
    while (peek() === "||") {
      take();
      const r = and();
      v = truthy(v) ? v : r;
    }
    return v;
  }
  const v = or();
  if (i !== tokens.length) throw new Error(`trailing tokens: ${tokens.slice(i).join(" ")}`);
  return v;
}

const REPO = "elitea-ng/elitea-platform";
function runsOn({ event, vars = { ENG_RUNNER: "on" }, actor = "alice", triggering = actor, head = REPO, author = "alice" }) {
  const github = { event_name: event, repository: REPO, actor, triggering_actor: triggering, event: {} };
  if (event === "pull_request" || event === "pull_request_target") {
    github.event.pull_request = { head: { repo: head === null ? null : { full_name: head } }, user: { login: author } };
  }
  const expression = ROUTED_RUNS_ON.replace(/^\$\{\{\s*/, "").replace(/\s*\}\}$/, "");
  return evaluate(expression, { github, vars });
}

test("the evaluator follows GitHub's operand-returning && and ||", () => {
  assert.equal(evaluate("true && 'a' || 'b'", {}), "a");
  assert.equal(evaluate("false && 'a' || 'b'", {}), "b");
  assert.equal(evaluate("x.y == 'on'", { x: {} }), false, "a missing property is null, and null is not 'on'");
  assert.equal(evaluate("'ON' == 'on'", {}), true, "string equality ignores case");
});

const TABLE = [
  ["a same-repo pull request", { event: "pull_request" }, "eng-runner"],
  ["a fork pull request", { event: "pull_request", head: "mallory/elitea-platform" }, "ubuntu-latest"],
  ["a pull request from a deleted fork", { event: "pull_request", head: null }, "ubuntu-latest"],
  ["a Dependabot pull request", { event: "pull_request", author: "dependabot[bot]", actor: "dependabot[bot]" }, "ubuntu-latest"],
  ["a human push on top of a Dependabot pull request", { event: "pull_request", author: "dependabot[bot]" }, "ubuntu-latest"],
  ["a Dependabot re-run of a human pull request", { event: "pull_request", triggering: "dependabot[bot]" }, "ubuntu-latest"],
  // The runs on main write the caches; they stay hosted (zstd cache version). merge_group cannot tell who
  // wrote the queued change.
  ["merge_group", { event: "merge_group" }, "ubuntu-latest"],
  ["schedule", { event: "schedule" }, "ubuntu-latest"],
  ["workflow_dispatch", { event: "workflow_dispatch" }, "ubuntu-latest"],
  ["push", { event: "push" }, "ubuntu-latest"],
  ["pull_request_target, even from this repository", { event: "pull_request_target" }, "ubuntu-latest"],
  ["workflow_run", { event: "workflow_run" }, "ubuntu-latest"],
  ["issue_comment", { event: "issue_comment" }, "ubuntu-latest"],
  ["the variable unset", { event: "pull_request", vars: {} }, "ubuntu-latest"],
  ["the variable off", { event: "pull_request", vars: { ENG_RUNNER: "off" } }, "ubuntu-latest"],
  ["the variable 'true' (only 'on' turns it on)", { event: "pull_request", vars: { ENG_RUNNER: "true" } }, "ubuntu-latest"],
];
for (const [what, ctx, want] of TABLE) {
  test(`routing: ${what} → ${want}`, () => assert.equal(runsOn(ctx), want));
}

test("the condition is an allowlist of events, never a negation of pull_request", () => {
  assert.doesNotMatch(ROUTING_CONDITION, /event_name\s*!=/);
});

test("a routed job that runs node or go must install it", () => {
  const noNode = routedJob({ steps: [{ run: "node scripts/ci/x.mjs" }] });
  assert.match(check(model({ schedule: [] }, noNode)).errors.join(), /runs node, but no step installs it/);
  const ok = routedJob({ steps: [{ uses: "actions/setup-node@v7" }, { run: "node scripts/ci/x.mjs" }] });
  assert.deepEqual(check(model({ schedule: [] }, ok)).errors, []);
  const viaEnv = routedJob({ raw: [{ uses: "actions/checkout@v7" }, { uses: ENV_ACTION, with: { node: "true" } }, { run: "node scripts/ci/x.mjs" }] });
  assert.deepEqual(check(model({ schedule: [] }, viaEnv)).errors, [], "the env action with node: true installs it");
  const noGo = routedJob({ steps: [{ run: "go vet ./..." }] });
  assert.match(check(model({ schedule: [] }, noGo)).errors.join(), /runs go, but no step installs it/);
  assert.deepEqual(check(model({ schedule: [] }, routedJob({ steps: [{ uses: "actions/setup-go@v7" }, { run: "go vet ./..." }] }))).errors, []);
  // Prose and paths are not invocations.
  assert.deepEqual(check(model({ schedule: [] }, routedJob({ steps: [{ run: "echo go to node_modules; cat package.json" }] }))).errors, []);
});

// ── classifyRunsOn ──
test("runs-on: plain labels are hosted; the routed expression is routed", () => {
  assert.equal(classifyRunsOn({ "runs-on": "ubuntu-latest" }).kind, "hosted");
  assert.equal(classifyRunsOn({ "runs-on": "macos-latest" }).kind, "hosted");
  assert.equal(classifyRunsOn({ "runs-on": ROUTED_RUNS_ON }).kind, "routed");
});

test("runs-on: every other way to reach eng-runner is refused", () => {
  for (const runsOn of [
    "eng-runner",
    "ENG-RUNNER",
    ["self-hosted", "eng-runner"],
    { group: "eng-runner" },
    "${{ github.event_name != 'pull_request' && 'eng-runner' || 'ubuntu-latest' }}",
    "${{ vars.ENG_RUNNER == 'on' && 'eng-runner' || 'ubuntu-latest' }}",
    ROUTED_RUNS_ON.replace("vars.ENG_RUNNER == 'on' && ", ""),
    ROUTED_RUNS_ON + " ",
    "${{ inputs.runner }}",
    "${{ vars.RUNNER }}",
  ]) {
    assert.equal(classifyRunsOn({ "runs-on": runsOn }).kind, "problem", JSON.stringify(runsOn));
  }
});

test("runs-on: a matrix value is read through", () => {
  assert.equal(classifyRunsOn({ "runs-on": "${{ matrix.os }}", strategy: { matrix: { os: ["ubuntu-latest", "windows-latest"] } } }).kind, "hosted");
  assert.equal(classifyRunsOn({ "runs-on": "${{ matrix.os }}", strategy: { matrix: { include: [{ os: "eng-runner" }] } } }).kind, "problem");
});

// ── check() over synthetic workflows ──
// A routed job: the checkout and the cache-dir action first, then its own steps.
const routedJob = ({ steps, raw, ...extra } = {}) => ({
  "runs-on": ROUTED_RUNS_ON,
  steps: raw ?? [{ uses: "actions/checkout@v7" }, { uses: ENV_ACTION }, ...(steps ?? [{ uses: "actions/setup-node@v7" }, { run: "npm test" }])],
  ...extra,
});
const ENV_ACTIONS = { [ENV_ACTION.replace(/^\.\//, "")]: { runs: { using: "composite", steps: [{ run: 'echo "HOME=x" >> "$GITHUB_ENV"' }, { uses: "actions/setup-node@v7" }] } } };
const model = (on, job, actions = {}, more = {}) => ({ workflows: { "w.yml": { parsed: { on, jobs: { j: job } } }, ...more }, actions: { ...ENV_ACTIONS, ...actions } });

test("a routed job must move its caches onto the work volume first", () => {
  const without = routedJob({ raw: [{ uses: "actions/checkout@v7" }, { uses: "actions/setup-node@v7" }, { run: "npm test" }] });
  assert.match(check(model({ schedule: [] }, without)).errors.join(), /without \.\/\.github\/actions\/eng-runner-env: .*1 GiB/);
  const late = routedJob({ raw: [{ uses: "actions/checkout@v7" }, { uses: "actions/setup-node@v7" }, { uses: ENV_ACTION }, { run: "npm test" }] });
  assert.match(check(model({ schedule: [] }, late)).errors.join(), /runs before \.\/\.github\/actions\/eng-runner-env/);
  const gated = routedJob({ raw: [{ uses: "actions/checkout@v7" }, { uses: ENV_ACTION, if: "always()" }, { uses: "actions/setup-node@v7" }, { run: "npm test" }] });
  assert.match(check(model({ schedule: [] }, gated)).errors.join(), /must run unconditionally/);
  // A plain run step before it (an aggregator's result check) is fine.
  const runFirst = routedJob({ raw: [{ run: "test success = success" }, { uses: "actions/checkout@v7" }, { uses: ENV_ACTION }] });
  assert.deepEqual(check(model({ schedule: [] }, runFirst)).errors, []);
});

test("this repository's env action moves HOME, the caches and TMPDIR", () => {
  const root = path.resolve(path.dirname(new URL(import.meta.url).pathname), "..", "..");
  const text = fs.readFileSync(path.join(root, ENV_ACTION, "action.yml"), "utf8");
  for (const name of ["HOME", "XDG_CACHE_HOME", "GOPATH", "GOMODCACHE", "GOCACHE", "GOLANGCI_LINT_CACHE", "npm_config_cache", "TMPDIR"]) {
    assert.match(text, new RegExp(`echo "${name}=`), name);
  }
});

test("a clean routed job passes and is counted", () => {
  const { errors, routed } = check(model({ workflow_call: null, schedule: [] }, routedJob()));
  assert.deepEqual(errors, []);
  assert.deepEqual(routed, ["w.yml j"]);
});

test("container: and services: are refused on a routed job", () => {
  assert.match(check(model({ schedule: [] }, routedJob({ container: "node:26" }))).errors.join("\n"), /container/);
  assert.match(check(model({ schedule: [] }, routedJob({ services: { pg: { image: "postgres" } } }))).errors.join("\n"), /services/);
});

test("docker, podman, kind, sudo, apt and systemctl steps are refused", () => {
  for (const run of [
    "docker compose up -d",
    "set -e\n  docker run --rm x",
    "docker-compose up",
    "podman build .",
    "kind create cluster",
    "sudo rm -rf /usr/share/dotnet",
    "apt-get install -y jq",
    "sudo apt install -y jq",
    "systemctl restart docker",
    "x=$(docker image inspect y)",
  ]) {
    const errors = check(model({ schedule: [] }, routedJob({ steps: [{ run }] }))).errors;
    assert.equal(errors.length > 0, true, run);
  }
});

test("mentions that are not commands pass", () => {
  for (const run of ["bash docker/model-cache-init/test_entrypoint.sh", "kind=$(grep -m1 '^kind:' f)", "echo hi # sudo later", "cat deploy/docker-compose.yml"]) {
    assert.deepEqual(check(model({ schedule: [] }, routedJob({ steps: [{ run }] }))).errors, [], run);
  }
});

test("docker/* and kind actions are refused", () => {
  for (const uses of ["docker/setup-buildx-action@v4", "docker/login-action@v4", "helm/kind-action@v1", "docker://alpine:3"]) {
    assert.equal(check(model({ schedule: [] }, routedJob({ steps: [{ uses }] }))).errors.length, 1, uses);
  }
});

test("a step gated to GitHub-hosted runners is exempt", () => {
  for (const cond of [HOSTED_ONLY, `\${{ ${HOSTED_ONLY} }}`, `${HOSTED_ONLY} && !cancelled()`]) {
    const steps = [{ if: cond, run: "sudo rm -rf /usr/share/dotnet" }];
    assert.deepEqual(check(model({ schedule: [] }, routedJob({ steps }))).errors, [], cond);
  }
  // Not exempt when the hosted condition is not the leading conjunct (it might be OR-ed away).
  const steps = [{ if: `always() || ${HOSTED_ONLY}`, run: "sudo true" }];
  assert.equal(check(model({ schedule: [] }, routedJob({ steps }))).errors.length, 1);
});

test("local composite actions are read through, and container actions refused", () => {
  const actions = {
    ".github/actions/ok": { runs: { using: "composite", steps: [{ run: "npm ci" }] } },
    ".github/actions/bad": { runs: { using: "composite", steps: [{ name: "nested", uses: "./.github/actions/worse" }] } },
    ".github/actions/worse": { runs: { using: "composite", steps: [{ run: "docker build ." }] } },
    ".github/actions/box": { runs: { using: "docker", image: "Dockerfile" } },
  };
  assert.deepEqual(check(model({ schedule: [] }, routedJob({ steps: [{ uses: "./.github/actions/ok" }] }), actions)).errors, []);
  assert.match(check(model({ schedule: [] }, routedJob({ steps: [{ uses: "./.github/actions/bad" }] }), actions)).errors.join(), /docker.*in \.\/\.github\/actions\/worse/);
  assert.match(check(model({ schedule: [] }, routedJob({ steps: [{ uses: "./.github/actions/box" }] }), actions)).errors.join(), /container action/);
  assert.match(check(model({ schedule: [] }, routedJob({ steps: [{ uses: "./.github/actions/missing" }] }), actions)).errors.join(), /no action\.yml/);
});

test("a routed job in a pull_request_target or workflow_run workflow is refused", () => {
  for (const on of [{ pull_request_target: null }, { workflow_run: { workflows: ["CI"] } }, ["issue_comment"], "pull_request_review"]) {
    assert.match(check(model(on, routedJob())).errors.join(), /can start on/, JSON.stringify(on));
  }
});

test("a called workflow inherits its callers' events", () => {
  const caller = { parsed: { on: { workflow_run: null }, jobs: { call: { uses: "./.github/workflows/w.yml" } } } };
  const errors = check(model({ workflow_call: null }, routedJob(), {}, { "caller.yml": caller })).errors;
  assert.match(errors.join(), /w\.yml j: .*workflow_run/);
  const good = { parsed: { on: { pull_request: null }, jobs: { call: { uses: "./.github/workflows/w.yml" } } } };
  assert.deepEqual(check(model({ workflow_call: null }, routedJob(), {}, { "caller.yml": good })).errors, []);
});

// ── main() floors and this repository ──
function tree(workflowCount, job) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "routing-"));
  const wf = path.join(root, ".github", "workflows");
  fs.mkdirSync(wf, { recursive: true });
  for (let i = 0; i < workflowCount; i += 1) {
    fs.writeFileSync(path.join(wf, `w${i}.yml`), `on:\n  schedule:\n    - cron: '1 1 * * *'\njobs:\n  j:\n    runs-on: ${JSON.stringify(job)}\n    steps:\n      - uses: actions/checkout@v7\n      - uses: ${ENV_ACTION}\n      - run: echo\n`);
  }
  const action = path.join(root, ENV_ACTION);
  fs.mkdirSync(action, { recursive: true });
  fs.writeFileSync(path.join(action, "action.yml"), "runs:\n  using: composite\n  steps:\n    - run: echo\n      shell: bash\n");
  return root;
}
const run = (root, opts = {}) => {
  const lines = [];
  const code = main({ root, out: (l) => lines.push(l), err: (l) => lines.push(l), ...opts });
  return [code, lines.join("\n")];
};

test("too few workflows or routed jobs fails rather than passes", () => {
  assert.match(run(tree(3, "ubuntu-latest"), { minimumRouted: 0 })[1], /expected at least 40/);
  const [code, text] = run(tree(40, "ubuntu-latest"));
  assert.equal(code, 1);
  assert.match(text, /found 0 job\(s\) routed/);
  assert.equal(run(tree(40, ROUTED_RUNS_ON), { minimumRouted: 40 })[0], 0);
});

test("this repository holds the rule", () => {
  const [code, text] = run(path.resolve(path.dirname(new URL(import.meta.url).pathname), "..", ".."));
  assert.equal(code, 0, text);
});
