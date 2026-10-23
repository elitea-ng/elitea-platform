#!/usr/bin/env node
// The legacy runtime's name stays out of this repository.
//
// The platform used to sit beside a Python plugin runtime, and its name was
// written into comments, identifiers, routes and docs. Pylon removal (PRs 1 to
// 9) took every functional use out and reworded the rest as "the legacy
// runtime". This check keeps it that way: it reads every tracked file, path
// and content, and fails on any mention outside the files below.
//
// What may still say it, and why:
//
//   - FROZEN migrations. Ledgered migrations are checksum-immutable, so the
//     files that existed when this check was written keep their text, and two
//     keep their names. A NEW migration is above the frozen heads and is
//     checked like any other file.
//   - ALLOWED files: recorded evidence (fixtures, goldens, captured legacy
//     contracts) whose bytes a test compares, and real host paths an operator
//     types. Each one must still mention the name, or its entry is stale.
//   - Any file may name the two frozen migration FILES, since a reference to a
//     file has to spell its name.
//
// It also fails when the frozen migrations hold fewer mentions than the floor
// below: a pattern that stopped matching would otherwise pass by seeing nothing.
//
// Run with the runner's node; only the standard library.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { pyRepr, splitlines } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const REPOSITORY_ROOT = path.resolve(path.dirname(SELF), "..", "..");

// Split so this file does not match itself.
const NAME = "py" + "lon";
const MENTION = new RegExp(NAME, "i");

// The ledgered heads when this check was written. A migration numbered above
// its head is new, and new text has no reason to name the legacy runtime.
export const FROZEN_HEADS = {
  "services/elitea-main/migrations/shared/": 172,
  "services/elitea-main/migrations/tenant/": 154,
};

export const FROZEN_FILE_NAMES = [
  "0091_" + NAME + "_viewer_secret_list_parity",
  "0124_elitea_tools_" + NAME + "_shape",
];

export const ALLOWED = {
  "services/elitea-main/internal/api/legacy_bridge_absence_test.go":
    "the banned-pattern list: it names the legacy auth channel so no code can use it again",
  "deploy/ARTIFACT_CUTOVER.md": "real host paths and container names an operator types during the artifact cutover",
  "deploy/scripts/migrate-artifacts.sh": "the real host path of the legacy storage root, in its usage text",
  "docker/model-cache-init/manifest.json": "recorded history of the bundle's former consumer",
  "services/elitea-subapp-host/internal/apps/deepwiki/descriptor.json":
    "a recorded example question in the published tool description",
  "services/elitea-deepwiki-engine/src/wiki/prompts/PROMPTS_MANIFEST.json":
    "the recorded prompt manifest; its sha/chars are pinned by a test",
  "services/elitea-deepwiki-engine/tests/fixtures/wiki/sanitizer_corpus.jsonl": "sanitizer input corpus",
  "conformance/provider/fixtures/deepwiki/descriptor/legacy-v0/provider_descriptor.json": "captured legacy descriptor",
  "conformance/provider/fixtures/deepwiki/descriptor/legacy-v1/provider_descriptor.json": "captured legacy descriptor",
  "conformance/provider/fixtures/deepwiki/generation/composed_result.json": "golden generation output",
  "services/elitea-main/internal/api/v2/indexing/testdata/current_python_schedule_contract.json":
    "captured legacy schedule contract",
  "services/elitea-main/internal/application/indexschedule/testdata/current_python_schedule_due_contract.json":
    "captured legacy schedule contract",
  "testdata/legacy/legacy-rbac-static-catalog.json": "captured legacy permission catalogue",
  "testdata/proto/runtime/v1/configuration-validation/legacy-evidence.json": "captured legacy evidence",
  "testdata/proto/runtime/v1/node-event/current-parity-corpus.json": "captured parity corpus",
  "testdata/proto/runtime/v1/toolkit-available-tools/legacy-evidence.json": "captured legacy evidence",
  "scripts/ci/check-legacy-runtime-name.mjs": "this check's own description",
};

// Mentions the frozen migrations hold between them (179 when this was written).
export const MINIMUM_FROZEN_MENTIONS = 150;

export function trackedFiles(root) {
  const out = execFileSync("git", ["ls-files", "-z"], { cwd: root, maxBuffer: 1 << 30 });
  return out.toString("utf8").split("\0").filter(Boolean);
}

function readStrictUtf8(file) {
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(fs.readFileSync(file));
  } catch {
    return null; // not UTF-8, a directory (a submodule), or gone
  }
}

export function isFrozenMigration(name) {
  for (const [directory, head] of Object.entries(FROZEN_HEADS)) {
    if (!name.startsWith(directory) || !name.endsWith(".sql")) continue;
    const number = Number.parseInt(name.slice(directory.length, directory.length + 4), 10);
    return Number.isInteger(number) && number <= head;
  }
  return false;
}

// A line that mentions the name only inside a frozen migration's file name.
function namesOnlyFrozenFiles(line) {
  let rest = line;
  for (const file of FROZEN_FILE_NAMES) rest = rest.split(file).join("");
  return !MENTION.test(rest);
}

export function scan(root, files) {
  const violations = [];
  let frozenMentions = 0;
  const allowedSeen = new Set();
  for (const name of files) {
    const frozen = isFrozenMigration(name);
    if (MENTION.test(name) && !frozen && !Object.hasOwn(ALLOWED, name)) {
      violations.push(`${name}: the path names the legacy runtime`);
    }
    const text = readStrictUtf8(path.join(root, name));
    if (text === null) continue;
    splitlines(text).forEach((line, index) => {
      if (!MENTION.test(line)) return;
      if (frozen) {
        frozenMentions += 1;
      } else if (Object.hasOwn(ALLOWED, name)) {
        allowedSeen.add(name);
      } else if (!namesOnlyFrozenFiles(line)) {
        violations.push(`${name}:${index + 1}: ${line.trim()}`);
      }
    });
  }
  return { violations, frozenMentions, allowedSeen };
}

export function main(root = REPOSITORY_ROOT, log = console.log) {
  const files = trackedFiles(root);
  const tracked = new Set(files);
  const missing = Object.keys(ALLOWED).filter((name) => !tracked.has(name));
  if (missing.length) {
    log(`::error::allowed file(s) no longer tracked: ${pyRepr(missing)}; update ALLOWED in ${SELF}`);
    return 1;
  }
  const { violations, frozenMentions, allowedSeen } = scan(root, files);
  for (const violation of violations) {
    log(`::error::the legacy runtime is named outside the allowed files: ${violation}`);
  }
  const stale = Object.keys(ALLOWED).filter((name) => !allowedSeen.has(name));
  for (const name of stale) {
    log(`::error::${name} is allowed but no longer mentions the name; drop its entry from ALLOWED in ${SELF}`);
  }
  if (violations.length || stale.length) return 1;
  if (frozenMentions < MINIMUM_FROZEN_MENTIONS) {
    log(
      `::error::found ${frozenMentions} mention(s) in the frozen migrations, expected at least ` +
        `${MINIMUM_FROZEN_MENTIONS}; the pattern stopped matching, so this check saw nothing`,
    );
    return 1;
  }
  log(
    `legacy-runtime-name: ${files.length} tracked files clean; ${frozenMentions} mention(s) in frozen migrations, ` +
      `${Object.keys(ALLOWED).length} allowed file(s)`,
  );
  return 0;
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === SELF) {
  process.exitCode = main();
}
