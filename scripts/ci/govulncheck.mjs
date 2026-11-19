#!/usr/bin/env node
// govulncheck over every Go module that ships, with the coverage check built in.
//
// Run by the folded `dependency-scanning` job. The reachability-aware check
// (a vulnerable function the code can actually call) over:
//
//   * every module of go.work (elitea-main, elitea-scheduler, the libs, gen, ...),
//     read FROM go.work so a new `use` line is scanned the day it is added, and
//   * services/elitea-llm-gateway, which is deliberately OUTSIDE go.work and is
//     scanned with GOWORK=off, as its own build is (#2246 named it: it must not
//     be missed).
//
// COVERAGE. Every tracked go.mod must be in go.work, be the gateway, or be named
// in EXCLUDED with the reason (test fixtures, evaluation tasks: not shipped). A
// go.mod that is none of them, and an exclusion that no longer exists, fail.
//
// ALLOWLIST. scripts/ci/govulncheck-allowlist.json holds `{id, reason, expires}`
// for a finding that is not fixable now; an entry without a reason, or past its
// date, fails. govulncheck itself has no ignore file, so the text output's
// `Vulnerability #N: GO-YYYY-NNNN` lines are compared with it.

import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { isEntrypoint } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SELF), "..", "..");
export const GATEWAY = "services/elitea-llm-gateway";
export const EXCLUDED_PREFIXES = new Map([
  ["libs/rust/local-tools/tests/fixtures/", "a Go project the Rust local tools parse in their tests"],
  ["scripts/eval/", "evaluation task workspaces, not shipped"],
]);
export const MINIMUM_MODULES = 10;

/** The module directories of go.work's `use` block (and single-line `use`). */
export function workModules(goWork) {
  const dirs = [];
  const block = /^use\s*\(([\s\S]*?)^\)/m.exec(goWork);
  if (block) for (const line of block[1].split("\n")) {
    const m = /^\s*(\S+)\s*(?:\/\/.*)?$/.exec(line);
    if (m && m[1]) dirs.push(m[1].replace(/^\.\//, ""));
  }
  for (const m of goWork.matchAll(/^use\s+([^\s(]\S*)\s*$/gm)) dirs.push(m[1].replace(/^\.\//, ""));
  return dirs;
}

/** Every tracked go.mod is scanned or excluded on purpose. */
export function coverage(tracked, goWork) {
  const problems = [];
  const mods = tracked.filter((f) => f === "go.mod" || f.endsWith("/go.mod")).map((f) => path.posix.dirname(f));
  const use = workModules(goWork);
  const scanned = new Set([...use, GATEWAY]);
  if (use.length + 1 < MINIMUM_MODULES) problems.push(`go.work lists ${use.length} module(s), expected at least ${MINIMUM_MODULES - 1} — the parser stopped finding them`);
  for (const dir of scanned) if (!mods.includes(dir)) problems.push(`${dir}: scanned by govulncheck but has no tracked go.mod`);
  for (const dir of mods) {
    if (scanned.has(dir)) continue;
    if ([...EXCLUDED_PREFIXES.keys()].some((p) => `${dir}/`.startsWith(p))) continue;
    problems.push(`${dir}: a Go module that is neither in go.work nor the gateway nor excluded in scripts/ci/govulncheck.mjs — it would ship unscanned`);
  }
  for (const prefix of EXCLUDED_PREFIXES.keys()) {
    if (!mods.some((m) => `${m}/`.startsWith(prefix))) problems.push(`${prefix}: excluded from govulncheck but holds no tracked go.mod any more — remove the exclusion`);
  }
  return { modules: [...use.map((d) => ({ dir: d, workOff: false })), { dir: GATEWAY, workOff: true }], problems };
}

/** The GO-ids a govulncheck text report names. */
export function findings(report) {
  return [...String(report).matchAll(/^Vulnerability #\d+: (GO-\d{4}-\d+)/gm)].map((m) => m[1]);
}

export function allowlistProblems(entries, today = new Date()) {
  const problems = [];
  for (const e of entries) {
    if (!e.id || !e.reason) problems.push(`allowlist entry ${JSON.stringify(e)} needs an id and a reason`);
    else if (!e.expires || new Date(`${e.expires}T23:59:59Z`) < today) problems.push(`${e.id}: the allowlist entry ${e.expires ? `expired on ${e.expires}` : "has no expiry"} — fix the dependency and remove it`);
  }
  return problems;
}

export function main({ root = ROOT, run = spawnSync, out = console.log, err = console.error, today = new Date() } = {}) {
  const listing = run("git", ["ls-files"], { cwd: root, encoding: "utf8", maxBuffer: 1 << 26 });
  const { modules, problems } = coverage(String(listing.stdout).split("\n").filter(Boolean), fs.readFileSync(path.join(root, "go.work"), "utf8"));
  const allowFile = path.join(root, "scripts", "ci", "govulncheck-allowlist.json");
  const allow = fs.existsSync(allowFile) ? JSON.parse(fs.readFileSync(allowFile, "utf8")) : [];
  problems.push(...allowlistProblems(allow, today));
  for (const p of problems) err(`::error::${p}`);
  if (problems.length) return 1;
  const allowed = new Set(allow.map((e) => e.id));
  let failed = 0;
  for (const { dir, workOff } of modules) {
    out(`::group::govulncheck ${dir}${workOff ? " (GOWORK=off)" : ""}`);
    const result = run("govulncheck", ["./..."], {
      cwd: path.join(root, dir),
      encoding: "utf8",
      maxBuffer: 1 << 27,
      env: { ...process.env, ...(workOff ? { GOWORK: "off" } : {}) },
    });
    const report = `${result.stdout ?? ""}${result.stderr ?? ""}`;
    out(report);
    out("::endgroup::");
    if (result.status === 0) continue;
    const ids = findings(report);
    const open = ids.filter((id) => !allowed.has(id));
    if (ids.length === 0 || open.length > 0) {
      failed += 1;
      err(`::error::govulncheck failed for ${dir}${ids.length === 0 ? " (exit " + result.status + ", no finding parsed)" : `: ${open.join(", ")}`}`);
    } else {
      out(`${dir}: ${ids.join(", ")} allowlisted`);
    }
  }
  if (failed) return 1;
  out(`govulncheck: ${modules.length} module(s) clean`);
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
