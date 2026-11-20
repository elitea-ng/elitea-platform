#!/usr/bin/env node
// Which of the workflows ci.yml calls does this pull request reach?
//
// Before ci.yml every workflow carried its own `on.pull_request.paths`. They
// now run as the jobs of one workflow (two tiers, one verdict: ci.yml), and a
// reusable workflow cannot filter on paths, so the filters live in
// scripts/ci/tier-paths.json and this reads them. It writes `run=<json>` to
// $GITHUB_OUTPUT: one boolean per job id, which ci.yml tests with
// `fromJSON(needs.changes.outputs.run)['<job>']`.
//
// The changed files come from the pull request files API: the same diff
// GitHub's own path filter used (the PR's merge base to its head). The API
// stops at 3000 files; a change that large runs everything. A change to the
// tiering itself (`always` in tier-paths.json) also runs everything: the PR
// that edits the gate must exercise all of it.
//
// Patterns are GitHub's path-filter patterns for the shapes in use: `**` any
// depth, `*` within one segment, `?` one character, and a leading `!` that
// excludes what an earlier pattern included (the last match wins).

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { isEntrypoint } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
export const TIERS_FILE = path.join(path.dirname(SELF), "tier-paths.json");
export const API_FILE_LIMIT = 3000;

/** A GitHub path-filter pattern as a RegExp over a repository-relative path. */
export function patternToRegExp(pattern) {
  let source = "";
  for (let i = 0; i < pattern.length; i += 1) {
    const c = pattern[i];
    if (c === "*") {
      if (pattern[i + 1] === "*") {
        i += 1;
        if (pattern[i + 1] === "/") {
          i += 1;
          source += "(?:.*/)?";
        } else {
          source += ".*";
        }
      } else {
        source += "[^/]*";
      }
    } else if (c === "?") {
      source += "[^/]";
    } else if (c === "[") {
      const close = pattern.indexOf("]", i + 1);
      if (close < 0) {
        source += "\\[";
      } else {
        source += pattern.slice(i, close + 1);
        i = close;
      }
    } else {
      source += c.replace(/[.+^${}()|\\]/g, "\\$&");
    }
  }
  return new RegExp(`^${source}$`);
}

/** Does the file list reach the filter? A null/empty filter is "always". */
export function reaches(files, patterns) {
  if (!patterns || patterns.length === 0) return true;
  const compiled = patterns.map((p) => ({ negate: p.startsWith("!"), re: patternToRegExp(p.replace(/^!/, "")) }));
  const hasPositive = compiled.some((c) => !c.negate);
  return files.some((file) => {
    let included = !hasPositive;
    for (const { negate, re } of compiled) {
      if (re.test(file)) included = !negate;
    }
    return included;
  });
}

/** One boolean per job id of the tiers file. */
export function computeRun(tiers, files, { everything = false } = {}) {
  const everythingNow = everything || files.some((f) => tiers.always.includes(f));
  const run = {};
  for (const [id, job] of Object.entries(tiers.jobs)) {
    run[id] = everythingNow || reaches(files, job.paths);
  }
  return run;
}

async function changedFiles(token, repo, number, fetchImpl = fetch) {
  const files = [];
  for (let page = 1; page <= API_FILE_LIMIT / 100; page += 1) {
    const response = await fetchImpl(`https://api.github.com/repos/${repo}/pulls/${number}/files?per_page=100&page=${page}`, {
      headers: { Authorization: `Bearer ${token}`, Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" },
    });
    if (!response.ok) throw new Error(`pulls/${number}/files page ${page}: HTTP ${response.status}`);
    const batch = await response.json();
    for (const f of batch) {
      files.push(f.filename);
      // A rename is a change at both paths.
      if (f.previous_filename) files.push(f.previous_filename);
    }
    if (batch.length < 100) return { files, truncated: false };
  }
  return { files, truncated: true };
}

/**
 * Did the fast tier already pass for this head? `ready_for_review` starts CI
 * again for the SAME commit; when an earlier run of this workflow on that head
 * ended with `fast-gate` green, the fast jobs are not run twice: the ready run
 * goes straight to the heavy tier, and `ci-gate` treats the fast jobs as
 * satisfied (their run flag is false, so a skip is correct). A head that never
 * had a green fast tier runs it.
 */
export async function fastTierAlreadyPassed({ token, repo, headSha, runId }, fetchImpl = fetch) {
  const get = async (url) => {
    const response = await fetchImpl(`https://api.github.com/repos/${repo}/${url}`, {
      headers: { Authorization: `Bearer ${token}`, Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" },
    });
    if (!response.ok) throw new Error(`${url}: HTTP ${response.status}`);
    return response.json();
  };
  const runs = (await get(`actions/workflows/ci.yml/runs?head_sha=${headSha}&event=pull_request&per_page=30`)).workflow_runs ?? [];
  for (const run of runs) {
    if (String(run.id) === String(runId) || run.status !== "completed") continue;
    const jobs = (await get(`actions/runs/${run.id}/jobs?per_page=100`)).jobs ?? [];
    if (jobs.some((j) => j.name === "fast-gate" && j.conclusion === "success")) return run.id;
  }
  return null;
}

/** The run flags with the fast jobs satisfied by an earlier green run. */
export function withFastReused(run, tiers) {
  return Object.fromEntries(Object.entries(run).map(([id, on]) => [id, tiers.jobs[id].tier === "fast" ? false : on]));
}

export async function main(env = process.env, log = console.log, fetchImpl = fetch) {
  const tiers = JSON.parse(fs.readFileSync(TIERS_FILE, "utf8"));
  let run;
  if (!env.PR_NUMBER) {
    // No pull request (never in ci.yml today): run everything rather than nothing.
    run = computeRun(tiers, [], { everything: true });
  } else {
    const { files, truncated } = await changedFiles(env.GITHUB_TOKEN, env.GITHUB_REPOSITORY, env.PR_NUMBER);
    run = computeRun(tiers, files, { everything: truncated });
    log(`${files.length} changed file(s)${truncated ? " (the API list is truncated: running everything)" : ""}`);
    if (env.PR_ACTION === "ready_for_review" && env.HEAD_SHA) {
      const earlier = await fastTierAlreadyPassed({ token: env.GITHUB_TOKEN, repo: env.GITHUB_REPOSITORY, headSha: env.HEAD_SHA, runId: env.GITHUB_RUN_ID }, fetchImpl).catch((error) => {
        log(`::warning::could not look for an earlier fast tier (${error.message}); running it`);
        return null;
      });
      if (earlier) {
        log(`the fast tier already passed for ${env.HEAD_SHA} in run ${earlier}: not run again`);
        run = withFastReused(run, tiers);
      }
    }
  }
  for (const [id, on] of Object.entries(run)) log(`${on ? "run " : "skip"}  ${tiers.jobs[id].tier.padEnd(5)} ${id}`);
  const line = `run=${JSON.stringify(run)}\n`;
  if (env.GITHUB_OUTPUT) fs.appendFileSync(env.GITHUB_OUTPUT, line);
  return 0;
}

if (isEntrypoint(import.meta.url)) {
  main().then(
    (code) => process.exit(code),
    (error) => {
      console.error(`::error::${error.message}`);
      process.exit(1);
    },
  );
}
