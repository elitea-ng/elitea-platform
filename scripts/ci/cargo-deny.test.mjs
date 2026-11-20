// Scenarios for cargo-deny.mjs: every tracked lock is scanned unless excluded on
// purpose, an ignore needs a live expiry, and the repository's own deny.toml passes.

import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";

import { EXCLUDED, MINIMUM_LOCKS, ignoreProblems, main, workspacesToScan } from "./cargo-deny.mjs";

const locks = (n) => Array.from({ length: n }, (_, i) => `services/s${i}/Cargo.lock`);

test("each tracked lock is a workspace to scan, an excluded one is not", () => {
  const tracked = [...locks(MINIMUM_LOCKS), "Cargo.lock", ...EXCLUDED.keys(), "services/x/go.mod"];
  const { dirs, problems } = workspacesToScan(tracked);
  assert.deepEqual(problems, []);
  assert.ok(dirs.includes("services/s0"));
  assert.ok(dirs.includes("."));
  assert.ok(!dirs.some((d) => d.includes("fixtures")));
});

test("a stale exclusion and a listing that found nothing both fail", () => {
  const stale = workspacesToScan(locks(MINIMUM_LOCKS));
  assert.match(stale.problems.join("\n"), /no longer|not a tracked Cargo.lock/);
  const none = workspacesToScan([...EXCLUDED.keys()]);
  assert.match(none.problems.join("\n"), /expected at least/);
});

test("an ignore must carry an expiry, and an expired one fails", () => {
  const toml = [
    '  { id = "RUSTSEC-2026-0001", reason = "x (expires 2026-12-31)" },',
    '  { id = "RUSTSEC-2026-0002", reason = "no date" },',
    '  { id = "RUSTSEC-2026-0003", reason = "old (expires 2026-01-01)" },',
  ].join("\n");
  const problems = ignoreProblems(toml, new Date("2026-11-19T00:00:00Z"));
  assert.equal(problems.length, 2);
  assert.match(problems[0], /0002.*no "\(expires/);
  assert.match(problems[1], /0003.*expired on 2026-01-01/);
});

test("main stops at the first problem and runs cargo deny per workspace otherwise", () => {
  const calls = [];
  const run = (cmd, args) => {
    calls.push([cmd, ...args].join(" "));
    if (cmd === "git") return { stdout: [...locks(MINIMUM_LOCKS), ...EXCLUDED.keys()].join("\n") };
    return { status: 0 };
  };
  const root = path.resolve(import.meta.dirname, "..", "..");
  assert.equal(main({ root, run, out: () => {}, err: () => {}, today: new Date("2026-11-19T00:00:00Z") }), 0);
  assert.equal(calls.filter((c) => c.startsWith("cargo deny")).length, MINIMUM_LOCKS);
  assert.ok(calls.some((c) => c.includes("-A unlicensed")));
  // A failing workspace fails the run but every workspace is still scanned.
  const failing = (cmd, args) => (cmd === "git" ? run(cmd, args) : { status: args.some((a) => a.includes("s1/")) ? 1 : 0 });
  assert.equal(main({ root, run: failing, out: () => {}, err: () => {}, today: new Date("2026-11-19T00:00:00Z") }), 1);
});

test("deny.toml in the repository has only live, dated ignores", () => {
  const toml = fs.readFileSync(path.resolve(import.meta.dirname, "..", "..", "deny.toml"), "utf8");
  assert.deepEqual(ignoreProblems(toml, new Date("2026-11-19T00:00:00Z")), []);
});
