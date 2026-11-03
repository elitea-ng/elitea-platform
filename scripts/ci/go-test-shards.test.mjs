// Scenarios for go-test-shards.mjs. The split must put each package in exactly
// one shard, so the error shapes are held here. The real workspace is checked
// last, with the real shard file.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import {
  assign,
  defaultShardFile,
  joinProfiles,
  listWorkspacePackages,
  mergeCoverage,
  parseShards,
  specificity,
} from "./go-test-shards.mjs";

const pkg = (dir, module = "svc") => ({ module, dir, importPath: `example.com/${dir}` });
const shard = (name, packages, storage = false) => ({ name, storage, packages });

test("an exact pattern beats a /... pattern on the same path, and a longer path beats a shorter one", () => {
  assert.equal(specificity("svc/api", "svc/api/v2"), -1);
  assert.ok(specificity("svc/api", "svc/api") > specificity("svc/api/...", "svc/api"));
  assert.ok(specificity("svc/api/v2/...", "svc/api/v2/x") > specificity("svc/api/...", "svc/api/v2/x"));
  assert.ok(specificity("svc/api/...", "svc/api/v2/x") > specificity("...", "svc/api/v2/x"));
  assert.equal(specificity("svc/api/...", "svc/apix"), -1);
  assert.equal(specificity("...", "anything/at/all"), 0);
});

test("the most specific shard gets the package", () => {
  const shards = [shard("core", ["svc/api/eliteacore"]), shard("api", ["svc/api/..."]), shard("rest", ["..."])];
  const { byShard, errors } = assign(shards, [pkg("svc/api"), pkg("svc/api/eliteacore"), pkg("svc/api/mcp"), pkg("libs/x")]);
  assert.deepEqual(errors, []);
  assert.deepEqual(byShard.get("core").map((p) => p.dir), ["svc/api/eliteacore"]);
  assert.deepEqual(byShard.get("api").map((p) => p.dir), ["svc/api", "svc/api/mcp"]);
  assert.deepEqual(byShard.get("rest").map((p) => p.dir), ["libs/x"]);
});

test("a package in no shard is an error", () => {
  const { errors } = assign([shard("api", ["svc/api/..."])], [pkg("svc/api"), pkg("libs/x")]);
  assert.deepEqual(errors, ["package libs/x is in no shard"]);
});

test("a package that two shards claim with the same specificity is an error", () => {
  const { errors } = assign([shard("a", ["svc/..."]), shard("b", ["svc/..."]), shard("rest", ["..."])], [pkg("svc/x")]);
  assert.ok(errors.includes("package svc/x is in 2 shards (a, b) with the same specificity"), errors.join("\n"));
});

test("two catch-all shards are an error", () => {
  const { errors } = assign([shard("a", ["..."]), shard("b", ["..."])], [pkg("svc/x")]);
  assert.ok(errors.some((error) => error.startsWith("package svc/x is in 2 shards")), errors.join("\n"));
});

test("a stale pattern and an empty shard are errors", () => {
  const { errors } = assign([shard("gone", ["svc/removed/..."]), shard("rest", ["..."])], [pkg("svc/x")]);
  assert.ok(errors.includes("shard gone: pattern svc/removed/... selects no package (stale)"), errors.join("\n"));
  assert.ok(errors.includes("shard gone selects no package"), errors.join("\n"));
});

test("a pattern that another shard always beats is stale", () => {
  const { errors } = assign([shard("a", ["svc/x"]), shard("b", ["svc/x/..."]), shard("rest", ["..."])], [pkg("svc/x"), pkg("libs/y")]);
  assert.ok(errors.includes("shard b: pattern svc/x/... selects no package (stale)"), errors.join("\n"));
});

test("an empty package list is an error", () => {
  const { errors } = assign([shard("rest", ["..."])], []);
  assert.deepEqual(errors, ["the workspace lists no package; an empty split cannot pass"]);
});

test("the shard file rejects bad shapes", () => {
  const good = { shards: [{ name: "rest", storage: false, packages: ["..."] }] };
  assert.equal(parseShards(JSON.stringify(good)).length, 1);
  const bad = [
    "not json",
    JSON.stringify({ shards: [] }),
    JSON.stringify({ shards: [{ name: "Bad Name", storage: false, packages: ["..."] }] }),
    JSON.stringify({ shards: [{ name: "a", storage: false, packages: ["..."] }, { name: "a", storage: false, packages: ["x"] }] }),
    JSON.stringify({ shards: [{ name: "a", storage: "yes", packages: ["..."] }] }),
    JSON.stringify({ shards: [{ name: "a", storage: false, packages: [] }] }),
    JSON.stringify({ shards: [{ name: "a", storage: false, packages: ["/abs"] }] }),
    JSON.stringify({ shards: [{ name: "a", storage: false, packages: ["svc/../x"] }] }),
    JSON.stringify({ shards: [{ name: "a", storage: false, packages: ["svc/.../x"] }] }),
    JSON.stringify({ shards: [{ name: "a", storage: false, packages: ["svc/"] }] }),
  ];
  for (const text of bad) {
    assert.throws(() => parseShards(text), undefined, text);
  }
});

test("the package list comes from go list -m and go list -find", () => {
  const root = path.join(os.tmpdir(), "root");
  const calls = [];
  const run = (cmd, args, options) => {
    calls.push([cmd, args.join(" "), options.cwd]);
    if (args[1] === "-m") {
      return `${root}/svc\n${root}/libs/x\n`;
    }
    if (options.cwd === `${root}/svc`) {
      return `example.com/svc\t${root}/svc\nexample.com/svc/api\t${root}/svc/api\n`;
    }
    return `example.com/x\t${root}/libs/x\n`;
  };
  const { modules, packages } = listWorkspacePackages(root, run);
  assert.deepEqual(modules, ["svc", "libs/x"]);
  assert.deepEqual(packages.map((p) => `${p.module} ${p.dir} ${p.importPath}`), [
    "svc svc example.com/svc",
    "svc svc/api example.com/svc/api",
    "libs/x libs/x example.com/x",
  ]);
  assert.ok(calls.slice(1).every(([, args]) => args.includes("-find")), "the package list must not resolve imports");
});

test("an empty module list is an error", () => {
  assert.throws(() => listWorkspacePackages("/r", () => "\n"), /no workspace module/);
});

test("coverage profiles of one module join under one mode line", () => {
  assert.equal(joinProfiles(["mode: atomic\na.go:1.1,2.2 1 1\n", "mode: atomic\nb.go:1.1,2.2 1 0\n"]), "mode: atomic\na.go:1.1,2.2 1 1\nb.go:1.1,2.2 1 0\n");
  assert.throws(() => joinProfiles(["mode: atomic\n", "mode: set\n"]), /different modes/);
  assert.throws(() => joinProfiles(["a.go:1.1,2.2 1 1\n"]), /must start with/);
  assert.throws(() => joinProfiles([""]), /no coverage profile/);
});

test("the merge joins the same module across shards and keeps the others", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "go-shards-"));
  try {
    const write = (rel, text) => {
      fs.mkdirSync(path.dirname(path.join(dir, rel)), { recursive: true });
      fs.writeFileSync(path.join(dir, rel), text);
    };
    write("in/coverage-go-a/services/main/coverage.out", "mode: atomic\na.go:1.1,2.2 1 1\n");
    write("in/coverage-go-b/services/main/coverage.out", "mode: atomic\nb.go:1.1,2.2 1 1\n");
    write("in/coverage-go-b/libs/x/coverage.out", "mode: atomic\nx.go:1.1,2.2 1 1\n");
    const merged = mergeCoverage(path.join(dir, "in"), path.join(dir, "out"));
    assert.deepEqual(merged, ["libs/x/coverage.out", "services/main/coverage.out"]);
    assert.equal(fs.readFileSync(path.join(dir, "out/services/main/coverage.out"), "utf8"), "mode: atomic\na.go:1.1,2.2 1 1\nb.go:1.1,2.2 1 1\n");
    fs.mkdirSync(path.join(dir, "empty/coverage-go-a"), { recursive: true });
    assert.throws(() => mergeCoverage(path.join(dir, "empty"), path.join(dir, "out2")), /no coverage.out/);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("the repository's shard file puts every workspace package in exactly one shard", () => {
  const shards = parseShards(fs.readFileSync(defaultShardFile, "utf8"));
  const { packages } = listWorkspacePackages();
  const { byShard, errors } = assign(shards, packages);
  assert.deepEqual(errors, []);
  const total = [...byShard.values()].reduce((sum, list) => sum + list.length, 0);
  assert.equal(total, packages.length);
  assert.equal(new Set([...byShard.values()].flat().map((p) => p.importPath)).size, packages.length);
});
