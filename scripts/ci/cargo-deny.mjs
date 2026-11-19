#!/usr/bin/env node
// cargo-deny over EVERY tracked Cargo.lock, with the coverage check built in.
//
// Run by the folded `dependency-scanning` job (dependency-scanning.yml) with a
// prebuilt cargo-deny binary: advisories, yanked crates, the licence allowlist
// and registry/git sources of deny.toml, for each workspace. No compile.
//
// COVERAGE. The lock files come from `git ls-files`, not from a list in a
// workflow, so a new Cargo.lock is scanned the day it is committed. A lock that
// is deliberately NOT scanned is named in EXCLUDED below, with the reason; a
// path that is excluded and no longer tracked fails too (a stale exclusion is a
// hole that outlived its reason).
//
// EXPIRY. Every advisory `ignore` in deny.toml ends in `(expires YYYY-MM-DD)`.
// An ignore without one, or past it, fails: an allowlist entry that outlives its
// reason is how a scanner stops scanning.
//
// Workspace crates carry no `license` field, so `-A unlicensed` is passed; the
// licence allowlist then holds third-party crates only.

import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { isEntrypoint } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SELF), "..", "..");

/** Cargo.lock paths that are not scanned, and why. */
export const EXCLUDED = new Map([
  ["scripts/runtime/fixtures/code-data-processing/Cargo.lock", "a sandbox test fixture (user code the code runner executes), not shipped"],
]);
export const MINIMUM_LOCKS = 8;

/** The workspaces to scan: the directory of each tracked Cargo.lock not excluded. */
export function workspacesToScan(tracked) {
  const locks = tracked.filter((f) => f === "Cargo.lock" || f.endsWith("/Cargo.lock"));
  const problems = [];
  for (const excluded of EXCLUDED.keys()) {
    if (!locks.includes(excluded)) problems.push(`${excluded}: excluded from cargo-deny but not a tracked Cargo.lock any more — remove the exclusion`);
  }
  if (locks.length < MINIMUM_LOCKS) problems.push(`found ${locks.length} Cargo.lock file(s), expected at least ${MINIMUM_LOCKS} — the listing stopped matching`);
  const dirs = locks.filter((l) => !EXCLUDED.has(l)).map((l) => path.posix.dirname(l));
  return { dirs, problems };
}

/** Problems with the `ignore` entries of deny.toml: no expiry, or expired. */
export function ignoreProblems(denyToml, today = new Date()) {
  const problems = [];
  const entries = denyToml.split("\n").filter((l) => /^\s*\{\s*id\s*=\s*"RUSTSEC-/.test(l));
  for (const line of entries) {
    const id = /id\s*=\s*"([^"]+)"/.exec(line)[1];
    const expires = /\(expires (\d{4}-\d{2}-\d{2})\)/.exec(line)?.[1];
    if (!expires) problems.push(`${id}: the ignore has no "(expires YYYY-MM-DD)" in its reason`);
    else if (new Date(`${expires}T23:59:59Z`) < today) problems.push(`${id}: the ignore expired on ${expires} — fix the dependency and remove it`);
  }
  return problems;
}

export function main({ root = ROOT, run = spawnSync, out = console.log, err = console.error, today = new Date() } = {}) {
  const listing = run("git", ["ls-files"], { cwd: root, encoding: "utf8", maxBuffer: 1 << 26 });
  const { dirs, problems } = workspacesToScan(String(listing.stdout).split("\n").filter(Boolean));
  problems.push(...ignoreProblems(fs.readFileSync(path.join(root, "deny.toml"), "utf8"), today));
  for (const p of problems) err(`::error::${p}`);
  if (problems.length) return 1;
  let failed = 0;
  for (const dir of dirs) {
    out(`::group::cargo deny ${dir}`);
    const result = run("cargo", ["deny", "--manifest-path", `${dir}/Cargo.toml`, "--all-features", "check", "-A", "unlicensed", "--hide-inclusion-graph"], {
      cwd: root,
      stdio: "inherit",
    });
    out("::endgroup::");
    if (result.status !== 0) {
      failed += 1;
      err(`::error::cargo deny failed for ${dir}`);
    }
  }
  if (failed) return 1;
  out(`cargo-deny: ${dirs.length} workspace(s) clean`);
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
