#!/usr/bin/env node
/*
A vendored crate's manifest changes only together with its patch note.

Every crate under libs/rust/vendor/ is a reviewed copy of a published crate.
libs/rust/vendor/README.md records each difference from the archive, and the
tree digest of each copy. PR #1547, a Dependabot nightly group, bumped
quick-xml from 0.41 to 0.42 inside libs/rust/vendor/calamine/Cargo.toml.
Dependabot reached that file through a path dependency of the desktop
doc-extract-sidecar. calamine 0.36 does not compile against quick-xml 0.42,
and the lockfiles of the other workspaces broke. Nobody had reviewed the
change against the copy.

.github/dependabot.yml now excludes the vendor directory. This gate fails on
any pull request that edits a vendored manifest (`Cargo.toml` or
`Cargo.toml.orig`) without also adding a line to libs/rust/vendor/README.md
that names the crate's directory. A re-vendor that records its change passes.
A scanner bump, or a hand edit that skips the note, does not.

    node scripts/ci/check-vendor-manifest-notes.mjs <base-ref> [<head-ref>]

<base-ref> is the merge base side of the comparison, for example
`origin/main`. The diff is `<base-ref>...<head-ref>`, and <head-ref> defaults
to HEAD. The run fails when git cannot
produce that diff: a gate with no input must not report success.
*/
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

export const README = "libs/rust/vendor/README.md";
const MANIFEST = /^libs\/rust\/vendor\/([^/]+)\/Cargo\.toml(?:\.orig)?$/;

/**
 * The faults of one diff.
 *
 * @param {string[]} changed the paths the diff touches
 * @param {string[]} readmeAdded the lines the diff adds to README, without the `+`
 * @returns {{ crates: string[], problems: string[] }}
 */
export function check(changed, readmeAdded) {
  const crates = [
    ...new Set(changed.map((p) => MANIFEST.exec(p)?.[1]).filter(Boolean)),
  ].sort();
  const problems = [];
  for (const crate of crates) {
    // The directory name as a word: `zavora-slide` must not be satisfied by a
    // note that names only `zavora-slide-opc`.
    const named = new RegExp(`(^|[^A-Za-z0-9_-])${crate.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}([^A-Za-z0-9_-]|$)`);
    if (!readmeAdded.some((line) => named.test(line))) {
      problems.push(
        `libs/rust/vendor/${crate}/Cargo.toml changed, but ${README} adds no line that names \`${crate}\`. ` +
          "A vendored manifest moves only in a re-vendor that records the change (see \"Moving zip\" and the crate's own section). " +
          "If a scanner made this change, revert it: .github/dependabot.yml excludes libs/rust/vendor.",
      );
    }
  }
  return { crates, problems };
}

/** The lines a unified diff adds, without the leading `+`. */
export function addedLines(diff) {
  return diff
    .split("\n")
    .filter((l) => l.startsWith("+") && !l.startsWith("+++"))
    .map((l) => l.slice(1));
}

function git(args) {
  return execFileSync("git", args, { encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
}

function main() {
  const base = process.argv[2];
  const head = process.argv[3] ?? "HEAD";
  if (!base) {
    console.error("ERROR: usage: check-vendor-manifest-notes.mjs <base-ref>");
    process.exit(2);
  }
  let changed;
  let readmeDiff;
  try {
    changed = git(["diff", "--name-only", `${base}...${head}`]).split("\n").filter(Boolean);
    readmeDiff = git(["diff", `${base}...${head}`, "--", README]);
  } catch (e) {
    console.error(`ERROR: git could not diff ${base}...${head}: ${e.message}`);
    process.exit(1);
  }
  const { crates, problems } = check(changed, addedLines(readmeDiff));
  for (const p of problems) console.error(`ERROR: ${p}`);
  if (problems.length > 0) process.exit(1);
  console.log(
    crates.length === 0
      ? `OK: ${changed.length} changed path(s), no vendored manifest among them.`
      : `OK: vendored manifest(s) of ${crates.join(", ")} changed, each with a note in ${README}.`,
  );
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main();
