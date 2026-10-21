#!/usr/bin/env node
// Every push arm that names `main` must also name the release staging branch.
//
// Issue #518. `helm-lint.yml` guarded `deploy/helm/**`, and its push arm named
// `main` only. The release staging branch is where pull requests land first, and
// that branch becomes main later. So the chart on the staging branch was
// unrenderable and no push run reported it. A measurement made on 2026-08-18 gives
// the size of the hole: `actions/runs?branch=claude/issue-248-c5b380&event=push`
// returned `total_count: 0` for EVERY workflow in this repository. Not one push
// run has ever started on the staging branch.
//
// The same audit found a second shape of the same defect. Four workflows named
// their OWN file in the pull_request paths and not in the push paths, so a push
// that edited only the gate could not start the gate it edited. This check holds
// that too.
//
// The gate is not the wiring. `helm-lint.yml` was correct in its own file, and it
// could not start where it was needed. So this check holds the wiring, and it
// reads two artifacts:
//
//   .github/staging-branch.txt   one line, the branch name, or the file is absent
//   .github/workflows/*.yml      the push arm of each workflow
//
// Two states, and both are checked:
//
//   1. The file names a branch. That branch must exist on `origin`, and every
//      workflow whose push arm names `main` must also name it, unless the
//      exemption table below names the workflow with a reason.
//   2. The file is absent or blank. The staging branch is retired, so NO workflow
//      may keep a push arm for it. A dead branch name in a trigger is a gate that
//      stopped starting in silence — the same defect in the other direction.
//
// This file reads the workflows as text with the regular expressions below
// (no YAML parser) and uses only node's standard library, so no job needs to
// install anything.

import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { pyRepr, splitlines } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const REPOSITORY_ROOT = path.resolve(path.dirname(SELF), "..", "..");

// A count below this floor means the glob stopped matching, not that the
// repository got smaller. "Nothing found, therefore correct" is how
// check-playwright-image-tag stopped gating in silence.
export const MINIMUM_WORKFLOW_COUNT = 10;

// Branch names a push arm may hold that are not the staging branch. `next` is
// publish.yml's second release line.
export const PERMANENT_BRANCHES = new Set(["main", "next"]);

// Workflows that must NOT carry the staging arm, each with the reason. An entry
// here is a decision, not a silence.
export const EXEMPT_WORKFLOWS = {
  "publish.yml":
    "It cuts a release: it tags, builds and pushes images, and writes to " +
    "the registry. A staging arm would publish release artifacts from a " +
    "release candidate.",
  "deepwiki-real-engine.yml":
    "It has no push arm at all, and no pull_request arm either. It runs " +
    "the native DeepWiki engine's real analysis pass over the standalone " +
    "stack, cloning a public repository from github.com; the per-change " +
    "gate for the same product surface is the `deepwiki-stack` job in " +
    "ci-web-e2e.yml, which runs the same engine image on its fixture " +
    "runner. Weekly cadence plus manual dispatch, like ci-web-mutation.yml.",
  "nightly-real-llm.yml":
    "No push arm and no pull_request arm: it is the real-model chat lane's " +
    "exploratory superset (the two tool-call-offering specs the PR lane " +
    "leaves out, on both runtimes) on a nightly cadence plus manual " +
    "dispatch. The per-change gate for the same surface is the " +
    "`chat-stream-real` job in ci-web-e2e.yml.",
  "cancel-merged-pr-runs.yml":
    "No push arm and no pull_request arm, deliberately: it runs on " +
    "pull_request_target `closed` only, and cancels the unfinished runs of " +
    "a pull request that merged. A push arm would give it nothing to " +
    "cancel, and it must never touch the runs a merge starts on a trunk " +
    "branch.",
  "nightly-image-scan.yml":
    "No push arm and no pull_request arm, deliberately (owner request " +
    "2026-10-20): it scans the PUBLISHED `latest` images every night and " +
    "keeps one issue per vulnerable image. A commit-time rebuild-and-scan " +
    "was what it replaced. The per-change gate is publish.yml's release " +
    "scan, which stops a vulnerable image advancing `latest`; the " +
    "PR-time checks of the image list, the exemption register and the " +
    "gate rule are in image-matrix.yml.",
  "ci-image-cleanup.yml":
    "No push arm and no pull_request arm, deliberately: it deletes old " +
    "versions of the CI-only ci-* container packages on a daily schedule " +
    "(plus dispatch) from main, and a merge is no reason to delete " +
    "anything. Its policy is unit-tested in no-binaries.yml.",
  "ci-web-mutation.yml":
    "It has no push arm at all. It is a weekly cadence job plus manual " +
    "dispatch, not a per-merge gate.",
  // Issue #526 removed the time-limited `ci-web.yml` entry. Commit 98fb8cff
  // (#549) retired the release staging branch: it deleted
  // `.github/staging-branch.txt`, and origin holds no branch of that name any
  // more. So the entry excused a coverage rule that state 2 already switches
  // off, and it kept `ci-web.yml` — the largest gate set here — silent in the
  // one state where the rule speaks again. Do NOT add a staging arm to
  // `ci-web.yml`: with the branch retired, the stale-name rule below rejects
  // it.
};

// Python's re.MULTILINE patterns; `(?![\s\S])` is Python's `\Z`.
const PUSH_BLOCK = /^  push:\n(?<body>(?:(?:    .*)?\n)*?)(?=^  \S|^\S|(?![\s\S]))/m;
const BRANCHES_BLOCK = /^    branches:\n(?<body>(?:(?:      .*)?\n)*?)(?=^    \S|(?![\s\S]))/m;
const BRANCH_ITEM = /^      - "?(?<name>[^"\n#]+?)"?\s*$/gm;
const PATHS_BLOCK = /^    paths:\n(?<body>(?:(?:      .*)?\n)*?)(?=^    \S|(?![\s\S]))/m;

const items = (body) => [...body.matchAll(BRANCH_ITEM)].map((m) => m.groups.name.trim());

export function readStagingBranch(root = REPOSITORY_ROOT) {
  const file = path.join(root, ".github", "staging-branch.txt");
  if (!fs.existsSync(file)) return "";
  return fs.readFileSync(file, "utf8").trim();
}

/**
 * The branch names of the push arm, or null when there is no arm.
 *
 * An empty list is a parse failure, never a pass: a push arm always names at
 * least one branch, so zero names means this reader stopped reading.
 */
export function pushBranches(text) {
  const push = PUSH_BLOCK.exec(text);
  if (push === null) return null;
  const branches = BRANCHES_BLOCK.exec(push.groups.body);
  if (branches === null) return [];
  return items(branches.groups.body);
}

/**
 * The path filters of the push arm, or null when there are none.
 *
 * null means the arm selects every file, which is the strongest state and
 * needs no self-reference.
 */
export function pushPaths(text) {
  const push = PUSH_BLOCK.exec(text);
  if (push === null) return null;
  const paths = PATHS_BLOCK.exec(push.groups.body);
  if (paths === null) return null;
  return items(paths.groups.body);
}

/** Ask origin. Report the unreachable case apart from the absent case. */
export function branchExistsOnOrigin(branch, root = REPOSITORY_ROOT) {
  const completed = spawnSync("git", ["ls-remote", "--heads", "origin", `refs/heads/${branch}`], {
    cwd: root,
    encoding: "utf8",
  });
  if (completed.status !== 0) {
    return [false, `git ls-remote could not read origin (exit ${completed.status}): ${(completed.stderr || "").trim()}`];
  }
  if (!(completed.stdout || "").trim()) return [false, `origin holds no branch named ${pyRepr(branch)}`];
  return [true, splitlines(completed.stdout.trim())[0]];
}

export function main({ root = REPOSITORY_ROOT, out = console.log, err = console.error } = {}) {
  const directory = path.join(root, ".github", "workflows");
  let names = [];
  try {
    names = fs.readdirSync(directory).filter((n) => n.endsWith(".yml")).sort();
  } catch {
    names = [];
  }
  if (names.length < MINIMUM_WORKFLOW_COUNT) {
    err(
      `::error::found ${names.length} workflow file(s) in ${directory}, expected at least ${MINIMUM_WORKFLOW_COUNT} — ` +
        "the glob stopped matching, so this check measured nothing",
    );
    return 1;
  }

  const stagingBranch = readStagingBranch(root);
  const failures = [];

  if (stagingBranch) {
    const [reachable, detail] = branchExistsOnOrigin(stagingBranch, root);
    if (!reachable) {
      failures.push(
        `.github/staging-branch.txt names ${pyRepr(stagingBranch)}, and ${detail}. ` +
          "Either correct the name, or delete the file and remove the " +
          "staging arm from every workflow push trigger. A trigger that " +
          "names a branch nobody pushes to is a gate that stopped " +
          "starting in silence.",
      );
    } else {
      out(`staging branch: ${stagingBranch} -> ${detail}`);
    }
  } else {
    out(".github/staging-branch.txt is absent or blank: no workflow may keep a staging push arm.");
  }

  for (const name of names) {
    const text = fs.readFileSync(path.join(directory, name), "utf8");
    const branches = pushBranches(text);

    if (branches === null) {
      out(`  ${name}: no push trigger`);
      if (!Object.hasOwn(EXEMPT_WORKFLOWS, name)) {
        failures.push(`${name} has no push trigger and no entry in the exemption table. Add the trigger, or record the reason it has none.`);
      }
      continue;
    }

    if (!branches.length) {
      failures.push(
        `${name} has a push trigger and this check read no branch name ` +
          "out of it. The reader stopped reading; correct the reader " +
          "rather than the workflow.",
      );
      continue;
    }

    out(`  ${name}: push branches = ${branches.join(", ")}`);

    const stale = branches.filter((b) => !PERMANENT_BRANCHES.has(b) && b !== stagingBranch);
    if (stale.length) {
      failures.push(
        `${name} names ${stale.join(", ")} in its push trigger, and ` +
          `.github/staging-branch.txt names ${stagingBranch || "(nothing)"}. Remove the stale name or ` +
          "correct the file.",
      );
    }

    const paths = pushPaths(text);
    if (paths !== null) {
      if (!paths.length) {
        failures.push(`${name} has a \`paths:\` key on its push arm and this check read no path out of it. The reader stopped reading.`);
      } else if (!paths.includes(`.github/workflows/${name}`)) {
        failures.push(
          `${name} filters its push arm by path and does not name ` +
            `.github/workflows/${name}. A push that edits only the gate ` +
            "then cannot start the gate it edited.",
        );
      }
    }

    if (Object.hasOwn(EXEMPT_WORKFLOWS, name)) {
      if (stagingBranch && branches.includes(stagingBranch)) {
        failures.push(`${name} is in the exemption table and names ${stagingBranch} anyway. Remove the exemption entry.`);
      } else {
        out(`    exempt: ${EXEMPT_WORKFLOWS[name]}`);
      }
      continue;
    }

    if (branches.includes("main") && stagingBranch && !branches.includes(stagingBranch)) {
      failures.push(
        `${name} names \`main\` in its push trigger and does not name ` +
          `${stagingBranch}. A workflow that runs only on main cannot ` +
          "guard the branch that becomes main. Add the branch, or add an " +
          "entry to EXEMPT_WORKFLOWS in this script with the reason.",
      );
    }
  }

  const present = new Set(names);
  const unknown = Object.keys(EXEMPT_WORKFLOWS).filter((n) => !present.has(n)).sort();
  if (unknown.length) {
    failures.push(
      "the exemption table names workflows that do not exist: " +
        unknown.join(", ") +
        ". An exemption for a deleted workflow hides the next one.",
    );
  }

  if (failures.length) {
    for (const failure of failures) err(`::error::${failure}`);
    return 1;
  }

  out(`workflow branch coverage: ${names.length} workflow(s) checked, all correct`);
  return 0;
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === SELF) {
  process.exitCode = main();
}
