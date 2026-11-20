#!/usr/bin/env node
// Does this change reach the given path patterns? For a STEP inside a job.
//
// ci.yml filters whole workflows by path (tier-changes.mjs). The folded
// `dependency-scanning` job holds scanners of different inputs (Go modules,
// Cargo manifests), and a scanner that needs minutes should not run for a docs
// change, so a step asks this first:
//
//   node scripts/ci/changed-paths.mjs <id> <pattern>...   writes `<id>=true|false`
//
// to $GITHUB_OUTPUT. On a pull request it diffs the merge base with the base
// branch (`origin/$BASE_REF...HEAD`, a checkout of full history); on any other
// event (the nightly schedule, a dispatch) it is always true: the scanners run
// over everything there. Patterns are GitHub path-filter patterns, as in
// tier-changes.mjs.

import { spawnSync } from "node:child_process";
import fs from "node:fs";

import { isEntrypoint } from "./py-compat.mjs";
import { reaches } from "./tier-changes.mjs";

export function main(argv = process.argv.slice(2), env = process.env, run = spawnSync, log = console.log, err = console.error) {
  const [id, ...patterns] = argv;
  if (!id || patterns.length === 0) {
    err("usage: changed-paths.mjs <output-id> <pattern>...");
    return 2;
  }
  let reached = true;
  if (env.GITHUB_EVENT_NAME === "pull_request") {
    const diff = run("git", ["diff", "--name-only", `origin/${env.BASE_REF}...HEAD`], { encoding: "utf8", maxBuffer: 1 << 26 });
    if (diff.status !== 0) {
      err(`::error::git diff against origin/${env.BASE_REF} failed: ${diff.stderr}`);
      return 1;
    }
    const files = String(diff.stdout).split("\n").filter(Boolean);
    reached = reaches(files, patterns);
    log(`${files.length} changed file(s); ${id}=${reached}`);
  } else {
    log(`${env.GITHUB_EVENT_NAME || "no event"}: ${id}=true (not a pull request)`);
  }
  if (env.GITHUB_OUTPUT) fs.appendFileSync(env.GITHUB_OUTPUT, `${id}=${reached}\n`);
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
