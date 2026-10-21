// Scenarios for check-workflow-cache-writes.mjs.
//
// Each negative case is a step shape that DID, or would, write the Actions cache
// from a pull request. Each positive case is the shape the repository uses now.
// The suite builds a throwaway tree with enough filler workflows to clear the
// checker's floors, so a case fails or passes on its own step alone.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import * as checker from "./check-workflow-cache-writes.mjs";

const PR_AND_PUSH = "on:\n  pull_request:\n  push:\n    branches: [main]\n";
const MAIN_PUSH = "github.ref == 'refs/heads/main' && github.event_name == 'push'";
const FILLER = "- uses: actions/cache/restore@v4\n  with: {path: x, key: k}\n- uses: actions/cache/restore@v4\n  with: {path: x, key: k}\n";

// textwrap.indent: the prefix on every line that is not blank.
const indent = (text, prefix) => text.replace(/^(?=.*\S)/gm, prefix);

function workflow(steps, on = PR_AND_PUSH) {
  return on + "jobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n" + indent(steps, "      ");
}

function tree(subject, { hcl = "", action = null, extra = {} } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "cache-writes-"));
  const wf = path.join(root, ".github", "workflows");
  fs.mkdirSync(wf, { recursive: true });
  for (let i = 0; i < checker.MINIMUM_WORKFLOWS; i += 1) fs.writeFileSync(path.join(wf, `filler${i}.yml`), workflow(FILLER));
  fs.writeFileSync(path.join(wf, "subject.yml"), subject);
  for (const [name, text] of Object.entries(extra)) fs.writeFileSync(path.join(wf, name), text);
  if (hcl) {
    fs.mkdirSync(path.join(root, "deploy", "ci"), { recursive: true });
    fs.writeFileSync(path.join(root, "deploy", "ci", "x.hcl"), hcl);
  }
  if (action !== null) {
    fs.mkdirSync(path.join(root, ".github", "actions", "build-stack-images"), { recursive: true });
    fs.writeFileSync(path.join(root, ".github", "actions", "build-stack-images", "action.yml"), action);
  }
  return root;
}

function runChecker(argv) {
  const lines = [];
  const code = checker.main(argv, (line) => lines.push(line), (line) => lines.push(line));
  return [code, lines.join("\n")];
}

function check(steps, { on = PR_AND_PUSH, hcl = "", action = null } = {}) {
  const root = tree(workflow(steps, on), { hcl, action });
  try {
    return runChecker(["--root", root]);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
}

/** Like check(), with the whole subject workflow given verbatim. */
function checkRaw(subject, extra = {}) {
  const root = tree(subject, { extra });
  try {
    return runChecker(["--root", root]);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
}

function assertWrites(steps, needle, options) {
  const [code, out] = check(steps, options);
  assert.equal(code, 1, out);
  assert.ok(out.includes(needle), out);
}

function assertClean(steps, options) {
  const [code, out] = check(steps, options);
  assert.equal(code, 0, out);
}

// ── setup-go ──────────────────────────────────────────────────────────────
test("setup go default cache is refused", () => {
  assertWrites("- uses: actions/setup-go@v7\n  with: {go-version-file: go.work, cache-dependency-path: go.sum}\n", "cache: false");
});

test("setup go cache true is refused", () => {
  assertWrites("- uses: actions/setup-go@v7\n  with: {go-version: '1.26', cache: true}\n", "cache: false");
});

test("setup go cache false passes", () => {
  assertClean("- uses: actions/setup-go@v7\n  with: {go-version: '1.26', cache: false}\n");
});

// ── actions/cache ─────────────────────────────────────────────────────────
test("full actions cache without condition is refused", () => {
  assertWrites("- uses: actions/cache@v4\n  with: {path: x, key: k}\n", "actions/cache saves the cache");
});

test("actions cache gated on ref only is refused", () => {
  // A schedule or dispatch on main is not a push to main.
  assertWrites("- uses: actions/cache@v4\n  if: github.ref == 'refs/heads/main'\n  with: {path: x, key: k}\n", "actions/cache saves the cache");
});

test("actions cache save without condition is refused", () => {
  assertWrites("- uses: actions/cache/save@v4\n  with: {path: x, key: k}\n", "actions/cache/save saves the cache");
});

test("actions cache gated on main push passes", () => {
  assertClean(`- uses: actions/cache@v4\n  if: ${MAIN_PUSH}\n  with: {path: x, key: k}\n`);
});

test("actions cache in release only workflow passes", () => {
  assertClean("- uses: actions/cache@v4\n  with: {path: x, key: k}\n", { on: "on:\n  push:\n    branches: [main, next]\n" });
});

test("actions cache in push to any branch is refused", () => {
  assertWrites("- uses: actions/cache@v4\n  with: {path: x, key: k}\n", "actions/cache saves", { on: "on:\n  push:\n" });
});

// ── BuildKit cache-to ─────────────────────────────────────────────────────
test("cache to min on pull request is refused", () => {
  // The ci-image-scan shape this change replaced.
  assertWrites(
    "- uses: docker/build-push-action@v7\n  with:\n    cache-to: type=gha,mode=${{ github.event_name == 'push' && github.ref == 'refs/heads/main' && 'max' || 'min' }},scope=s\n",
    "cache-to must be empty",
  );
});

test("cache to literal is refused", () => {
  assertWrites("- uses: docker/build-push-action@v7\n  with: {cache-to: 'type=gha,mode=max,scope=s'}\n", "cache-to must be empty");
});

test("cache to gated passes", () => {
  assertClean(
    "- uses: docker/build-push-action@v7\n  with:\n    cache-to: ${{ github.event_name == 'push' && github.ref == 'refs/heads/main' && 'type=gha,mode=max,scope=s' || '' }}\n",
  );
});

test("bake set cache to mode variable is refused", () => {
  // The helm-install-smoke LAYER_MODE shape this change replaced.
  assertWrites(
    "- uses: docker/bake-action@v7\n  with:\n    files: docker-bake.hcl\n    targets: a\n    set: |\n      a.cache-to=type=gha,mode=${{ env.LAYER_MODE }},scope=a\n",
    "writes the layer cache from every run",
  );
});

test("bake of docker bake hcl without override is refused", () => {
  assertWrites("- uses: docker/bake-action@v7\n  with:\n    files: docker-bake.hcl\n    targets: a\n", "keeps its mode=max cache-to");
});

test("bake set gated passes", () => {
  assertClean(
    "- uses: docker/bake-action@v7\n  with:\n    files: docker-bake.hcl\n    targets: a\n    set: |\n" +
      "      a.cache-to=${{ github.event_name == 'push' && github.ref == 'refs/heads/main' && 'type=gha,mode=max,scope=a' || '' }}\n",
  );
});

test("buildx bake run without clear is refused", () => {
  assertWrites("- run: docker buildx bake --load elitea-x\n", "add `--set '*.cache-to='`");
});

test("buildx bake run with clear passes", () => {
  assertClean("- run: docker buildx bake --set '*.cache-to=' --load elitea-x\n");
});

test("buildx bake print passes", () => {
  assertClean("- run: docker buildx bake --print > /dev/null\n");
});

test("cache to in release only workflow passes", () => {
  assertClean("- uses: docker/build-push-action@v7\n  with: {cache-to: 'type=gha,mode=max,scope=s'}\n", { on: "on:\n  push:\n    branches: [main]\n" });
});

test("cache to in ci hcl is refused", () => {
  assertWrites("- run: 'true'\n", "deploy/ci HCL", { hcl: 'target "a" {\n  cache-to = ["type=gha,mode=min,scope=a"]\n}\n' });
});

// ── Rust, golangci-lint, node, python, trivy ──────────────────────────────
test("rust cache without save if is refused", () => {
  assertWrites("- uses: Swatinem/rust-cache@v2\n  with: {workspaces: x}\n", "save-if");
});

test("rust cache with save if passes", () => {
  assertClean("- uses: Swatinem/rust-cache@v2\n  with:\n    save-if: ${{ github.ref == 'refs/heads/main' }}\n");
});

test("rust cache with save if false passes", () => {
  assertClean("- uses: Swatinem/rust-cache@v2\n  with: {save-if: false}\n");
});

test("golangci lint default cache is refused", () => {
  assertWrites("- uses: golangci/golangci-lint-action@v9\n  with: {version: latest}\n", "golangci-lint-action saves");
});

test("golangci lint skip save gated passes", () => {
  assertClean(`- uses: golangci/golangci-lint-action@v9\n  with:\n    skip-save-cache: \${{ !(${MAIN_PUSH}) }}\n`);
});

test("setup node cache npm is refused", () => {
  assertWrites("- uses: actions/setup-node@v7\n  with: {node-version: '26', cache: npm, package-manager-cache: false}\n", "setup-node `cache:`");
});

test("setup node automatic cache is refused", () => {
  assertWrites("- uses: actions/setup-node@v7\n  with: {node-version: '26'}\n", "package-manager-cache: false");
});

test("setup node without cache passes", () => {
  assertClean("- uses: actions/setup-node@v7\n  with: {node-version: '26', package-manager-cache: false}\n");
});

test("setup python cache pip is refused", () => {
  assertWrites("- uses: actions/setup-python@v7\n  with: {python-version: '3.12', cache: pip}\n", "setup-python `cache:`");
});

test("trivy default cache is refused", () => {
  assertWrites("- uses: aquasecurity/trivy-action@v0.36.0\n  with: {image-ref: x}\n", "trivy-action caches");
});

test("trivy cache false passes", () => {
  assertClean("- uses: aquasecurity/trivy-action@v0.36.0\n  with: {image-ref: x, cache: 'false'}\n");
});

// ── build-stack-images ────────────────────────────────────────────────────
test("stack images cache write true is refused", () => {
  assertWrites("- uses: ./.github/actions/build-stack-images\n  with: {cache-write: 'true'}\n", "cache-write");
});

test("stack images default ref only is refused", () => {
  const action = "inputs:\n  cache-write:\n    default: ${{ github.ref == 'refs/heads/main' }}\nruns:\n  using: composite\n  steps: []\n";
  assertWrites("- run: 'true'\n", "`cache-write` default", { action });
});

test("stack images default main push passes", () => {
  const action = `inputs:\n  cache-write:\n    default: \${{ ${MAIN_PUSH} }}\nruns:\n  using: composite\n  steps: []\n`;
  assertClean("- uses: ./.github/actions/build-stack-images\n  with: {cache-write: 'false'}\n", { action });
});

// ── the floors ────────────────────────────────────────────────────────────
test("stack images two mount writers for one suffix are refused", () => {
  const two =
    PR_AND_PUSH +
    [
      "jobs:",
      "  a:",
      "    runs-on: ubuntu-latest",
      "    steps:",
      "      - uses: ./.github/actions/build-stack-images",
      '        with: {mount-cache-suffix: rust, mount-cache-writer: "true"}',
      "  b:",
      "    runs-on: ubuntu-latest",
      "    steps:",
      "      - uses: ./.github/actions/build-stack-images",
      '        with: {mount-cache-suffix: rust, mount-cache-writer: "true"}',
      "",
    ].join("\n");
  const [code, out] = checkRaw(two);
  assert.equal(code, 1, out);
  assert.ok(out.includes("exactly one job in the repository"), out);
});

test("stack images one mount writer per suffix passes", () => {
  const ok =
    PR_AND_PUSH +
    [
      "jobs:",
      "  a:",
      "    runs-on: ubuntu-latest",
      "    steps:",
      "      - uses: ./.github/actions/build-stack-images",
      '        with: {mount-cache-suffix: rust, mount-cache-writer: "true"}',
      "  b:",
      "    runs-on: ubuntu-latest",
      "    steps:",
      "      - uses: ./.github/actions/build-stack-images",
      '        with: {mount-cache-suffix: e2e, mount-cache-writer: "true"}',
      "  c:",
      "    runs-on: ubuntu-latest",
      "    steps:",
      "      - uses: ./.github/actions/build-stack-images",
      "        with: {mount-cache-suffix: rust}",
      "",
    ].join("\n");
  const [code, out] = checkRaw(ok);
  assert.equal(code, 0, out);
});

test("stack images mount writers in two workflows for one suffix are refused", () => {
  // The cache key is repository-wide, so a second writer in ANOTHER file races too.
  const one =
    PR_AND_PUSH +
    [
      "jobs:",
      "  a:",
      "    runs-on: ubuntu-latest",
      "    steps:",
      "      - uses: ./.github/actions/build-stack-images",
      '        with: {mount-cache-suffix: rust, mount-cache-writer: "true"}',
      "",
    ].join("\n");
  const [code, out] = checkRaw(one, { "other.yml": one });
  assert.equal(code, 1, out);
  assert.ok(out.includes("exactly one job in the repository"), out);
  assert.ok(out.includes("other.yml"), out);
});

test("stack images mount writer on a matrix job is refused", () => {
  const matrix =
    PR_AND_PUSH +
    [
      "jobs:",
      "  a:",
      "    runs-on: ubuntu-latest",
      "    strategy:",
      "      matrix: {leg: [1, 2]}",
      "    steps:",
      "      - uses: ./.github/actions/build-stack-images",
      '        with: {mount-cache-suffix: rust, mount-cache-writer: "true"}',
      "",
    ].join("\n");
  const [code, out] = checkRaw(matrix);
  assert.equal(code, 1, out);
  assert.ok(out.includes("matrix job"), out);
});

function legs(entries, axis = "") {
  const lines = [
    "jobs:",
    "  a:",
    "    runs-on: ubuntu-latest",
    "    strategy:",
    "      matrix:",
    ...(axis ? [`        ${axis}`] : []),
    "        include:",
    ...entries.map(([suffix, writer], n) => `          - {image: i${n}, mount-suffix: ${suffix}, mount-writer: "${writer}"}`),
    "    steps:",
    "      - uses: ./.github/actions/build-stack-images",
    "        with:",
    "          mount-cache-suffix: ${{ matrix.mount-suffix }}",
    "          mount-cache-writer: ${{ matrix.mount-writer }}",
  ];
  return PR_AND_PUSH + lines.join("\n") + "\n";
}

test("stack images one writer leg per suffix passes", () => {
  const [code, out] = checkRaw(legs([["rust", "true"], ["rust", "false"], ["go", "true"]]));
  assert.equal(code, 0, out);
});

test("stack images two writer legs for one suffix are refused", () => {
  const [code, out] = checkRaw(legs([["rust", "true"], ["rust", "true"]]));
  assert.equal(code, 1, out);
  assert.ok(out.includes("exactly one job in the repository"), out);
});

test("stack images writer leg must be a literal", () => {
  const [code, out] = checkRaw(legs([["rust", "${{ github.ref == 'refs/heads/main' }}"]]));
  assert.equal(code, 1, out);
  assert.ok(out.includes("include entry must give the literal"), out);
});

test("stack images matrix writer with another axis is refused", () => {
  // With an axis, one include entry can become several legs.
  const [code, out] = checkRaw(legs([["rust", "true"]], "shard: [1, 2]"));
  assert.equal(code, 1, out);
  assert.ok(out.includes("include-only matrix"), out);
});

test("stack images mount writer expression is refused", () => {
  assertWrites("- uses: ./.github/actions/build-stack-images\n  with: {mount-cache-writer: '${{ strategy.job-index == 0 }}'}\n", "literal");
});

test("an empty tree is refused not passed", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "cache-writes-"));
  try {
    const [code, out] = runChecker(["--root", root]);
    assert.equal(code, 1);
    assert.ok(out.includes("this check read nothing"), out);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("the repository itself is clean", () => {
  const [code, out] = runChecker([]);
  assert.equal(code, 0, out);
});

test("a composite action in a dot-directory is still read, as Python's pathlib glob read it", () => {
  // `uses: ./.github/actions/.hidden` runs it like any other action, and
  // pathlib's `*/action.yml` matched the directory, so skipping it would let
  // an unguarded cache step hide there.
  const root = tree(workflow(FILLER));
  try {
    const dir = path.join(root, ".github", "actions", ".hidden");
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, "action.yml"), "runs:\n  using: composite\n  steps:\n    - uses: actions/cache@v4\n      with: {path: x, key: k}\n");
    const [code, out] = runChecker(["--root", root]);
    assert.equal(code, 1, out);
    assert.ok(out.includes(".github/actions/.hidden/action.yml"), out);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("the command line takes --x v and --x=v and refuses what it does not know", () => {
  const root = tree(workflow(FILLER));
  try {
    assert.equal(runChecker([`--root=${root}`, "--min-workflows=3", "--min-cache-steps", "2"])[0], 0);
    const [floor, floorOut] = runChecker(["--root", root, "--min-workflows", "999"]);
    assert.equal(floor, 1, floorOut);
    assert.ok(floorOut.includes("expected at least 999"), floorOut);
    for (const argv of [["--bogus"], ["--root"], ["--min-workflows", "x"], ["positional"]]) {
      const [code, out] = runChecker(argv);
      assert.equal(code, 2, `${argv.join(" ")}: ${out}`);
      assert.ok(out.startsWith("usage: check-workflow-cache-writes.mjs"), out);
      assert.ok(out.includes("check-workflow-cache-writes.mjs: error: "), out);
    }
    const [help, helpOut] = runChecker(["-h"]);
    assert.equal(help, 0);
    assert.ok(helpOut.startsWith("usage: "), helpOut);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
