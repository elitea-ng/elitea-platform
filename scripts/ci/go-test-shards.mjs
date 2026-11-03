#!/usr/bin/env node
// Split the Go workspace tests into shards, and prove that the split is complete.
//
// WHY THIS EXISTS
//
// The ci-go.yml Test job ran every workspace package in one job. On main it
// took 20 to 40 minutes (measured 2026-11-02 and 2026-11-03). One package,
// services/elitea-main/internal/infra/db/repos, ran for 6 to 10 minutes, and
// it started last. Now the job is a matrix of shards. Each shard runs on its
// own runner with its own services.
//
// A split is dangerous when it loses a package: the shards pass, and nobody
// sees that a package did not run. This script therefore makes the split a
// checked fact, not a hope:
//
//   1. It reads the package list from the go tool at run time. It does not
//      keep a copy of the list.
//   2. Each package goes to exactly one shard. A package that matches no
//      shard is an error. A package that two shards claim with the same
//      specificity is an error.
//   3. A pattern that selects no package is an error (a stale pattern). A
//      shard that selects no package is an error.
//   4. Every shard computes its own selection with the same function, so the
//      plan job and the shards cannot disagree.
//
// THE SHARD FILE (scripts/go/test-shards.json)
//
// Each shard has a name, a `storage` flag (true starts the storage
// emulators), and a list of patterns. A pattern is a directory path relative
// to the repository root:
//
//   services/elitea-main/internal/api        this package only
//   services/elitea-main/internal/api/...    this package and every package below it
//   ...                                      every package (the catch-all)
//
// The MOST SPECIFIC pattern wins. An exact path is more specific than a
// `/...` pattern on the same path. A longer path is more specific than a
// shorter one. So `internal/api/...` in one shard and
// `internal/api/v2/eliteacore` in another shard do not conflict: eliteacore
// goes to the second shard.
//
// Usage:
//
//   node scripts/ci/go-test-shards.mjs check
//       Read the workspace, assign every package, print one line per shard,
//       and fail on any error above.
//   node scripts/ci/go-test-shards.mjs matrix
//       The same check, then print the GitHub Actions matrix as JSON:
//       {"include":[{"name":"...","storage":true}, ...]}.
//   node scripts/ci/go-test-shards.mjs select <shard> <out-file>
//       The same check, then write the packages of one shard to <out-file>,
//       one `<module dir><TAB><import path>` line per package.
//       scripts/go/workspace-run.sh reads that file (ELITEA_GO_PACKAGES_FILE).
//   node scripts/ci/go-test-shards.mjs collect-coverage <out-dir>
//       Copy <module>/coverage.out of each workspace module to
//       <out-dir>/<module>/coverage.out. Fail if there is no profile.
//   node scripts/ci/go-test-shards.mjs merge-coverage <in-dir> <out-dir>
//       <in-dir> holds one directory per shard. Join the profiles of the same
//       module into <out-dir>/<module>/coverage.out. The shards hold different
//       packages, so a join is a correct merge.
//
// Environment: ELITEA_GO_TEST_SHARDS names a different shard file (tests use it).

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
export const repoRoot = path.resolve(scriptDir, "..", "..");
export const defaultShardFile = path.join(repoRoot, "scripts", "go", "test-shards.json");

const NAME = /^[a-z0-9][a-z0-9-]*$/;

// Return the specificity of `pattern` for the package directory `dir`, or -1
// when the pattern does not match. Both are relative to the repository root,
// with forward slashes.
export function specificity(pattern, dir) {
  if (pattern === "...") {
    return 0;
  }
  if (pattern.endsWith("/...")) {
    const prefix = pattern.slice(0, -4);
    if (dir === prefix || dir.startsWith(prefix + "/")) {
      // `x/...` is less specific than the exact `x`, which scores 2 * length + 1.
      return 2 * prefix.length;
    }
    return -1;
  }
  return dir === pattern ? 2 * pattern.length + 1 : -1;
}

// Read and check the shard file. Throw on a file that the rules above reject.
export function parseShards(text, source = "test-shards.json") {
  let data;
  try {
    data = JSON.parse(text);
  } catch (error) {
    throw new Error(`${source}: not valid JSON: ${error.message}`);
  }
  if (!data || !Array.isArray(data.shards) || data.shards.length === 0) {
    throw new Error(`${source}: "shards" must be a non-empty list`);
  }
  const names = new Set();
  return data.shards.map((shard, index) => {
    const where = `${source}: shard ${index}`;
    if (!shard || typeof shard.name !== "string" || !NAME.test(shard.name)) {
      throw new Error(`${where}: "name" must match ${NAME}`);
    }
    if (names.has(shard.name)) {
      throw new Error(`${where}: the name "${shard.name}" is used two times`);
    }
    names.add(shard.name);
    if (typeof shard.storage !== "boolean") {
      throw new Error(`${where} (${shard.name}): "storage" must be true or false`);
    }
    if (!Array.isArray(shard.packages) || shard.packages.length === 0) {
      throw new Error(`${where} (${shard.name}): "packages" must be a non-empty list`);
    }
    for (const pattern of shard.packages) {
      if (typeof pattern !== "string" || pattern === "" || pattern.startsWith("/") || pattern.endsWith("/") || pattern.split("/").includes("..") || pattern.includes("\\")) {
        throw new Error(`${where} (${shard.name}): bad pattern ${JSON.stringify(pattern)}`);
      }
      if (pattern !== "..." && pattern.includes("...") && !pattern.endsWith("/...")) {
        throw new Error(`${where} (${shard.name}): "..." is allowed only as the full pattern or as a "/..." suffix: ${pattern}`);
      }
    }
    return { name: shard.name, storage: shard.storage, packages: [...shard.packages] };
  });
}

// Give each package to one shard. `packages` is a list of
// {module, dir, importPath}, with `module` and `dir` relative to the root.
// Return {byShard, errors}. A non-empty `errors` means the split is not safe.
export function assign(shards, packages) {
  const errors = [];
  const byShard = new Map(shards.map((shard) => [shard.name, []]));
  const used = new Set();
  if (packages.length === 0) {
    errors.push("the workspace lists no package; an empty split cannot pass");
    return { byShard, errors };
  }
  for (const pkg of packages) {
    let best = -1;
    let winners = [];
    for (const shard of shards) {
      for (const pattern of shard.packages) {
        const score = specificity(pattern, pkg.dir);
        if (score < 0 || score < best) {
          continue;
        }
        if (score > best) {
          best = score;
          winners = [];
        }
        winners.push({ shard: shard.name, pattern });
      }
    }
    const shardNames = [...new Set(winners.map((winner) => winner.shard))];
    if (shardNames.length === 0) {
      errors.push(`package ${pkg.dir} is in no shard`);
      continue;
    }
    if (shardNames.length > 1) {
      errors.push(`package ${pkg.dir} is in ${shardNames.length} shards (${shardNames.join(", ")}) with the same specificity`);
      continue;
    }
    for (const winner of winners) {
      used.add(`${winner.shard}\u0000${winner.pattern}`);
    }
    byShard.get(shardNames[0]).push(pkg);
  }
  for (const shard of shards) {
    for (const pattern of shard.packages) {
      if (!used.has(`${shard.name}\u0000${pattern}`)) {
        errors.push(`shard ${shard.name}: pattern ${pattern} selects no package (stale)`);
      }
    }
    if (byShard.get(shard.name).length === 0) {
      errors.push(`shard ${shard.name} selects no package`);
    }
  }
  return { byShard, errors };
}

function toSlash(p) {
  return p.split(path.sep).join("/");
}

function relative(root, dir) {
  const rel = toSlash(path.relative(root, dir));
  return rel === "" ? "." : rel;
}

// Ask the go tool for every workspace module and every package in it.
// `-find` does not resolve imports, so it needs no module download.
export function listWorkspacePackages(root = repoRoot, run = execFileSync) {
  const modulesOut = run("go", ["list", "-m", "-f", "{{.Dir}}"], { cwd: root, encoding: "utf8" });
  const modules = modulesOut.split("\n").map((line) => line.trim()).filter(Boolean);
  if (modules.length === 0) {
    throw new Error("go list -m named no workspace module");
  }
  const packages = [];
  for (const moduleDir of modules) {
    const out = run("go", ["list", "-e", "-find", "-f", "{{.ImportPath}}\t{{.Dir}}", "./..."], { cwd: moduleDir, encoding: "utf8" });
    for (const line of out.split("\n")) {
      if (!line.trim()) {
        continue;
      }
      const [importPath, dir] = line.split("\t");
      if (!importPath || !dir) {
        throw new Error(`go list printed a line this script cannot read: ${JSON.stringify(line)}`);
      }
      packages.push({ module: relative(root, moduleDir), dir: relative(root, dir), importPath });
    }
  }
  return { modules: modules.map((dir) => relative(root, dir)), packages };
}

function loadPlan() {
  const file = process.env.ELITEA_GO_TEST_SHARDS || defaultShardFile;
  const shards = parseShards(fs.readFileSync(file, "utf8"), path.relative(repoRoot, file) || file);
  const { modules, packages } = listWorkspacePackages();
  const { byShard, errors } = assign(shards, packages);
  if (errors.length > 0) {
    for (const error of errors) {
      console.error(`go-test-shards: ${error}`);
    }
    throw new Error(`the shard split has ${errors.length} error(s); fix ${path.relative(repoRoot, file)}`);
  }
  return { shards, modules, packages, byShard };
}

function report(plan) {
  console.error(`go-test-shards: ${plan.packages.length} package(s) in ${plan.modules.length} module(s), ${plan.shards.length} shard(s)`);
  for (const shard of plan.shards) {
    const pkgs = plan.byShard.get(shard.name);
    const moduleCount = new Set(pkgs.map((pkg) => pkg.module)).size;
    console.error(`  ${shard.name.padEnd(12)} ${String(pkgs.length).padStart(4)} package(s) in ${moduleCount} module(s)${shard.storage ? ", storage emulators" : ""}`);
  }
}

// Read every profile below `dir`, keyed by its path relative to `dir`.
function findProfiles(dir) {
  const found = [];
  const walk = (current) => {
    for (const entry of fs.readdirSync(current, { withFileTypes: true })) {
      const full = path.join(current, entry.name);
      if (entry.isDirectory()) {
        walk(full);
      } else if (entry.isFile() && entry.name === "coverage.out") {
        found.push(full);
      }
    }
  };
  walk(dir);
  return found;
}

// Join Go coverage profiles. Each profile starts with one `mode:` line. All
// profiles must use the same mode, or the joined file is not valid.
export function joinProfiles(texts) {
  let mode = null;
  const body = [];
  for (const text of texts) {
    const lines = text.split("\n").filter((line) => line !== "");
    if (lines.length === 0) {
      continue;
    }
    if (!lines[0].startsWith("mode: ")) {
      throw new Error(`a coverage profile must start with "mode: ", got ${JSON.stringify(lines[0])}`);
    }
    if (mode !== null && lines[0] !== mode) {
      throw new Error(`coverage profiles use different modes: ${mode} and ${lines[0]}`);
    }
    mode = lines[0];
    body.push(...lines.slice(1));
  }
  if (mode === null) {
    throw new Error("no coverage profile holds a mode line");
  }
  return [mode, ...body].join("\n") + "\n";
}

export function mergeCoverage(inDir, outDir) {
  const groups = new Map();
  for (const shardDir of fs.readdirSync(inDir, { withFileTypes: true })) {
    if (!shardDir.isDirectory()) {
      continue;
    }
    const base = path.join(inDir, shardDir.name);
    for (const file of findProfiles(base)) {
      const key = toSlash(path.relative(base, file));
      if (!groups.has(key)) {
        groups.set(key, []);
      }
      groups.get(key).push(fs.readFileSync(file, "utf8"));
    }
  }
  if (groups.size === 0) {
    throw new Error(`no coverage.out below ${inDir}; an empty merge cannot pass`);
  }
  for (const [key, texts] of groups) {
    const target = path.join(outDir, key);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, joinProfiles(texts));
  }
  return [...groups.keys()].sort();
}

export function collectCoverage(outDir, root = repoRoot, run = execFileSync) {
  const modulesOut = run("go", ["list", "-m", "-f", "{{.Dir}}"], { cwd: root, encoding: "utf8" });
  const copied = [];
  for (const moduleDir of modulesOut.split("\n").map((line) => line.trim()).filter(Boolean)) {
    const source = path.join(moduleDir, "coverage.out");
    if (!fs.existsSync(source)) {
      continue;
    }
    const rel = relative(root, moduleDir);
    const target = path.join(outDir, rel, "coverage.out");
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.copyFileSync(source, target);
    copied.push(rel);
  }
  if (copied.length === 0) {
    throw new Error("no workspace module holds a coverage.out; the test step wrote no profile");
  }
  return copied;
}

export function main(argv) {
  const [command, ...rest] = argv;
  switch (command) {
    case "check": {
      report(loadPlan());
      console.error("go-test-shards: every package is in exactly one shard");
      return 0;
    }
    case "matrix": {
      const plan = loadPlan();
      report(plan);
      process.stdout.write(JSON.stringify({ include: plan.shards.map((shard) => ({ name: shard.name, storage: shard.storage })) }) + "\n");
      return 0;
    }
    case "select": {
      const [name, outFile] = rest;
      if (!name || !outFile) {
        throw new Error("usage: go-test-shards.mjs select <shard> <out-file>");
      }
      const plan = loadPlan();
      if (!plan.byShard.has(name)) {
        throw new Error(`no shard is named ${name}`);
      }
      const pkgs = plan.byShard.get(name);
      fs.writeFileSync(outFile, pkgs.map((pkg) => `${pkg.module}\t${pkg.importPath}\n`).join(""));
      report(plan);
      console.error(`go-test-shards: wrote ${pkgs.length} package(s) of shard ${name} to ${outFile}`);
      return 0;
    }
    case "collect-coverage": {
      const [outDir] = rest;
      if (!outDir) {
        throw new Error("usage: go-test-shards.mjs collect-coverage <out-dir>");
      }
      const copied = collectCoverage(outDir);
      console.error(`go-test-shards: collected ${copied.length} profile(s): ${copied.join(", ")}`);
      return 0;
    }
    case "merge-coverage": {
      const [inDir, outDir] = rest;
      if (!inDir || !outDir) {
        throw new Error("usage: go-test-shards.mjs merge-coverage <in-dir> <out-dir>");
      }
      const merged = mergeCoverage(inDir, outDir);
      console.error(`go-test-shards: merged ${merged.length} module profile(s): ${merged.join(", ")}`);
      return 0;
    }
    default:
      throw new Error("usage: go-test-shards.mjs <check|matrix|select|collect-coverage|merge-coverage> ...");
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    process.exitCode = main(process.argv.slice(2));
  } catch (error) {
    console.error(`go-test-shards: ${error.message}`);
    process.exitCode = 1;
  }
}
