#!/usr/bin/env node
// No workflow starts on a push.
//
// THE RULE (owner decision, 2026-10-22). A merge to main starts no pipeline.
// publish.yml cut a release on every merge — v4.66.0 to v6.0.0 in two days —
// and every CI workflow re-ran on main what its pull request had just proved.
// Now a pull request runs its gates; main runs on a schedule (the nightly cache
// writers, publish.yml's nightly pre-release) and by dispatch (a release, a dev
// pre-release, a cache warm-up). So no workflow may name the `push` event, in
// any of the three spellings `on:` takes:
//
//   on: push                   a bare event name
//   on: [pull_request, push]   a list
//   on:                        a map with a `push:` key
//     push:
//
// This replaces check-workflow-branch-coverage.mjs, whose subject — every push
// arm that names main also names the release staging branch — no longer exists.
//
// This reads the workflows as text with the regular expressions below (no YAML
// parser) and uses only node's standard library, so no job needs to install
// anything. It fails, rather than passes, when it finds fewer workflows or
// fewer `on:` blocks than its floor: "nothing found, therefore correct" is how
// check-playwright-image-tag stopped gating in silence.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { isEntrypoint } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const REPOSITORY_ROOT = path.resolve(path.dirname(SELF), "..", "..");

export const MINIMUM_WORKFLOW_COUNT = 10;

// `on:` (or the quoted "on":) at the top level, and what follows on its line.
const ON_LINE = /^["']?on["']?:[ \t]*(?<inline>[^\n#]*)/m;
// The indented body under a block-form `on:`.
const ON_BLOCK = /^["']?on["']?:[ \t]*(?:#.*)?\n(?<body>(?:(?:[ \t]+.*)?\n)*?)(?=^\S|(?![\s\S]))/m;

/**
 * The event names of a workflow's `on:`, or null when the file has none.
 *
 * Block form returns the keys at the first indentation level only, so a
 * `push` nested anywhere deeper (an input named push, a path) is not read.
 */
export function triggerEvents(text) {
  const line = ON_LINE.exec(text);
  if (line === null) return null;
  const inline = line.groups.inline.trim();
  if (inline) {
    const list = /^\[(.*)\]$/.exec(inline);
    const items = list ? list[1].split(",") : [inline];
    return items.map((i) => i.trim().replace(/^["']|["']$/g, "")).filter(Boolean);
  }
  const block = ON_BLOCK.exec(text);
  if (block === null) return [];
  const indents = [...block.groups.body.matchAll(/^([ \t]+)[^\s#]/gm)].map((m) => m[1].length);
  if (!indents.length) return [];
  const top = Math.min(...indents);
  const key = new RegExp(`^[ \\t]{${top}}["']?([A-Za-z_]+)["']?:`, "gm");
  return [...block.groups.body.matchAll(key)].map((m) => m[1]);
}

export function main({ root = REPOSITORY_ROOT, out = console.log, err = console.error } = {}) {
  const directory = path.join(root, ".github", "workflows");
  let names = [];
  try {
    names = fs.readdirSync(directory).filter((n) => /\.ya?ml$/.test(n)).sort();
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

  const failures = [];
  for (const name of names) {
    const events = triggerEvents(fs.readFileSync(path.join(directory, name), "utf8"));
    if (events === null || !events.length) {
      failures.push(`${name}: read no trigger out of its \`on:\`. The reader stopped reading; correct the reader rather than the workflow.`);
      continue;
    }
    out(`  ${name}: ${events.join(", ")}`);
    if (events.includes("push")) {
      failures.push(
        `${name} starts on a push. A merge to main starts no workflow: gate the change in its pull request, ` +
          "and give main a `schedule:` (a cache writer, a nightly) or `workflow_dispatch:` instead.",
      );
    }
  }

  if (failures.length) {
    for (const failure of failures) err(`::error::${failure}`);
    return 1;
  }
  out(`no push triggers: ${names.length} workflow(s) checked`);
  return 0;
}

if (isEntrypoint(import.meta.url)) {
  process.exitCode = main();
}
