// Scenarios for govulncheck.mjs: go.work's modules plus the gateway, the coverage
// of every tracked go.mod, the allowlist, and the repository's own tree.

import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";

import { GATEWAY, MINIMUM_MODULES, allowlistProblems, coverage, findings, main, workModules } from "./govulncheck.mjs";

const root = path.resolve(import.meta.dirname, "..", "..");
const goWork = (dirs) => `go 1.26.9\n\nuse (\n${dirs.map((d) => `\t./${d}`).join("\n")}\n)\n`;
const dirs = Array.from({ length: MINIMUM_MODULES }, (_, i) => `libs/m${i}`);

test("go.work's use block is read, one directory per line", () => {
  assert.deepEqual(workModules(goWork(["a/b", "c"])), ["a/b", "c"]);
  assert.deepEqual(workModules("go 1.26\nuse ./x\n"), ["x"]);
});

test("every tracked go.mod is scanned or excluded; the gateway is always scanned with GOWORK=off", () => {
  const tracked = [...dirs.map((d) => `${d}/go.mod`), `${GATEWAY}/go.mod`, "libs/rust/local-tools/tests/fixtures/projects/go/go.mod", "scripts/eval/adr/tasks/x/files/go.mod"];
  const { modules, problems } = coverage(tracked, goWork(dirs));
  assert.deepEqual(problems, []);
  assert.deepEqual(modules.at(-1), { dir: GATEWAY, workOff: true });
  assert.equal(modules.filter((m) => m.workOff).length, 1);
});

test("a go.mod that is nowhere, and a stale exclusion, fail", () => {
  const tracked = [...dirs.map((d) => `${d}/go.mod`), `${GATEWAY}/go.mod`, "tools/newmod/go.mod"];
  const { problems } = coverage(tracked, goWork(dirs));
  assert.match(problems.join("\n"), /tools\/newmod: a Go module that is neither in go.work/);
  assert.match(problems.join("\n"), /libs\/rust\/local-tools\/tests\/fixtures\/: excluded from govulncheck but holds no tracked go.mod/);
});

test("the text report's GO ids are read", () => {
  assert.deepEqual(findings("Vulnerability #1: GO-2026-0001\n  x\nVulnerability #2: GO-2026-0002\n"), ["GO-2026-0001", "GO-2026-0002"]);
  assert.deepEqual(findings("No vulnerabilities found."), []);
});

test("an allowlist entry needs a reason and a live expiry", () => {
  const today = new Date("2026-11-19T00:00:00Z");
  assert.equal(allowlistProblems([{ id: "GO-1", reason: "r", expires: "2026-12-31" }], today).length, 0);
  assert.equal(allowlistProblems([{ id: "GO-1", reason: "r" }], today).length, 1);
  assert.equal(allowlistProblems([{ id: "GO-1", reason: "r", expires: "2026-01-01" }], today).length, 1);
  assert.equal(allowlistProblems([{ id: "GO-1" }], today).length, 1);
});

test("main fails on a finding, passes an allowlisted one, and scans the gateway with GOWORK=off", () => {
  const tracked = fs.readFileSync(path.join(root, "go.work"), "utf8");
  const mods = workModules(tracked);
  const calls = [];
  const git = { stdout: [...mods.map((d) => `${d}/go.mod`), `${GATEWAY}/go.mod`, "libs/rust/local-tools/tests/fixtures/projects/go/go.mod", "scripts/eval/a/go.mod"].join("\n") };
  const mk = (status, text) => (cmd, args, opts) => {
    if (cmd === "git") return git;
    calls.push({ cwd: opts.cwd, workOff: opts.env.GOWORK });
    return { status, stdout: text };
  };
  const quiet = () => {};
  assert.equal(main({ root, run: mk(0, ""), out: quiet, err: quiet }), 0);
  assert.equal(calls.length, mods.length + 1);
  assert.equal(calls.at(-1).workOff, "off");
  assert.equal(calls[0].workOff, undefined);
  assert.equal(main({ root, run: mk(3, "Vulnerability #1: GO-2099-0001\n"), out: quiet, err: quiet }), 1);
  assert.equal(main({ root, run: mk(1, "boom"), out: quiet, err: quiet }), 1);
});

test("the repository's own go.mod files are all covered", () => {
  const { spawnSync } = process.getBuiltinModule("node:child_process");
  const tracked = String(spawnSync("git", ["ls-files"], { cwd: root, encoding: "utf8", maxBuffer: 1 << 26 }).stdout).split("\n").filter(Boolean);
  assert.deepEqual(coverage(tracked, fs.readFileSync(path.join(root, "go.work"), "utf8")).problems, []);
});

test("GOVULN_SCOPE picks the workspace modules or the gateway", () => {
  const mods = workModules(fs.readFileSync(path.join(root, "go.work"), "utf8"));
  const git = { stdout: [...mods.map((d) => `${d}/go.mod`), `${GATEWAY}/go.mod`, "libs/rust/local-tools/tests/fixtures/projects/go/go.mod", "scripts/eval/a/go.mod"].join("\n") };
  const quiet = () => {};
  for (const [scope, want] of [["workspace", mods.length], ["gateway", 1], ["all", mods.length + 1]]) {
    let n = 0;
    const run = (cmd) => (cmd === "git" ? git : (n += 1, { status: 0, stdout: "" }));
    assert.equal(main({ root, run, out: quiet, err: quiet, env: { GOVULN_SCOPE: scope } }), 0);
    assert.equal(n, want, scope);
  }
});
