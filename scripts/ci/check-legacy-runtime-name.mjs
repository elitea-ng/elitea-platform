#!/usr/bin/env node
// The legacy runtime's name stays out of this repository.
//
// The platform used to sit beside a Python plugin runtime, and its name was
// written into comments, identifiers, routes and docs. The removal programme
// (PRs 1 to 10) took every functional use out and reworded the rest as "the
// legacy runtime". This check keeps it that way: it reads every tracked file,
// path and content, and fails on any mention outside the files below.
//
// What may still say it, and why:
//
//   - FROZEN migrations. Ledgered migrations are checksum-immutable, so the
//     files that existed when this check was written keep their text, and two
//     keep their names. A migration numbered above the frozen heads is new and
//     is checked like any other file.
//   - ALLOWED files: recorded evidence (fixtures, goldens, captured legacy
//     contracts) whose bytes a test compares, and real host paths an operator
//     types.
//   - Any file may name the two frozen migration FILES, since a reference to a
//     file has to spell its name.
//
// Each exemption is an EXACT count of mentioning lines, not a licence: one
// more line in an allowed file fails, and so does one fewer (the entry is then
// stale). The frozen migrations are counted together, so a new .sql file given
// an old number cannot hide a mention there. A count that dropped to zero
// would also mean the pattern stopped matching, which is why it is exact
// rather than a floor.
//
// A file that is not UTF-8 is searched as bytes, so an encoding cannot hide a
// mention either.
//
// Run with the runner's node; only the standard library. This file spells the
// name only in pieces, so it holds no mention of its own.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { isEntrypoint, pyRepr, readStrictUtf8, splitlines, trackedFiles } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const REPOSITORY_ROOT = path.resolve(path.dirname(SELF), "..", "..");

const NAME = "py" + "lon";
const MENTION = new RegExp(NAME, "i");

// The ledgered heads when this check was written. A migration numbered above
// its head is new, and new text has no reason to name the legacy runtime.
export const FROZEN_HEADS = {
  "services/elitea-main/migrations/shared/": 172,
  "services/elitea-main/migrations/tenant/": 154,
};

// Mentioning lines the frozen migrations hold between them.
export const FROZEN_MENTIONS = 179;

export const FROZEN_FILE_NAMES = [
  "0091_" + NAME + "_viewer_secret_list_parity",
  "0124_elitea_tools_" + NAME + "_shape",
];

// file → [mentioning lines, why the file may hold them]
export const ALLOWED = {
  "services/elitea-main/internal/api/legacy_bridge_absence_test.go": [
    1,
    "the banned-pattern list: it names the legacy auth channel so no code can use it again",
  ],
  "deploy/ARTIFACT_CUTOVER.md": [6, "real host paths and container names an operator types during the artifact cutover"],
  "deploy/scripts/migrate-artifacts.sh": [1, "the real host path of the legacy storage root, in its usage text"],
  "docker/model-cache-init/manifest.json": [1, "recorded history of the bundle's former consumer"],
  "services/elitea-subapp-host/internal/apps/deepwiki/descriptor.json": [
    1,
    "a recorded example question in the published tool description",
  ],
  "services/elitea-deepwiki-engine/src/wiki/prompts/PROMPTS_MANIFEST.json": [
    1,
    "the recorded prompt manifest; its sha/chars are pinned by a test",
  ],
  "services/elitea-deepwiki-engine/tests/fixtures/wiki/sanitizer_corpus.jsonl": [6, "sanitizer input corpus"],
  "conformance/provider/fixtures/deepwiki/descriptor/legacy-v0/provider_descriptor.json": [1, "captured legacy descriptor"],
  "conformance/provider/fixtures/deepwiki/descriptor/legacy-v1/provider_descriptor.json": [1, "captured legacy descriptor"],
  "conformance/provider/fixtures/deepwiki/generation/composed_result.json": [1, "golden generation output"],
  "services/elitea-main/internal/api/v2/indexing/testdata/current_python_schedule_contract.json": [
    1,
    "captured legacy schedule contract",
  ],
  "services/elitea-main/internal/application/indexschedule/testdata/current_python_schedule_due_contract.json": [
    1,
    "captured legacy schedule contract",
  ],
  "testdata/legacy/legacy-rbac-static-catalog.json": [20, "captured legacy permission catalogue"],
  "testdata/proto/runtime/v1/configuration-validation/legacy-evidence.json": [1, "captured legacy evidence"],
  "testdata/proto/runtime/v1/node-event/current-parity-corpus.json": [2, "captured parity corpus"],
  "testdata/proto/runtime/v1/toolkit-available-tools/legacy-evidence.json": [1, "captured legacy evidence"],
};

export function isFrozenMigration(name) {
  for (const [directory, head] of Object.entries(FROZEN_HEADS)) {
    if (!name.startsWith(directory)) continue;
    const match = /^(\d{4})_[^/]+\.sql$/.exec(name.slice(directory.length));
    return match !== null && Number.parseInt(match[1], 10) <= head;
  }
  return false;
}

// A line that mentions the name only inside a frozen migration's file name.
function namesOnlyFrozenFiles(line) {
  let rest = line;
  for (const file of FROZEN_FILE_NAMES) rest = rest.split(file).join("");
  return !MENTION.test(rest);
}

// The file's lines, or for a file that is not UTF-8, its bytes read as Latin-1:
// every byte maps to one character, so an ASCII mention is still found.
function lines(file) {
  const text = readStrictUtf8(file);
  if (text !== null) return [splitlines(text), true];
  try {
    return [splitlines(fs.readFileSync(file).toString("latin1")), false];
  } catch {
    return [[], false]; // a directory (a submodule), or gone
  }
}

export function scan(root, files) {
  const violations = [];
  let frozenMentions = 0;
  const allowedMentions = new Map();
  for (const name of files) {
    const frozen = isFrozenMigration(name);
    if (MENTION.test(name) && !frozen) {
      violations.push(`${name}: the path names the legacy runtime`);
    }
    const [fileLines, utf8] = lines(path.join(root, name));
    fileLines.forEach((line, index) => {
      if (!MENTION.test(line)) return;
      if (frozen) {
        frozenMentions += 1;
      } else if (Object.hasOwn(ALLOWED, name)) {
        allowedMentions.set(name, (allowedMentions.get(name) ?? 0) + 1);
      } else if (!namesOnlyFrozenFiles(line)) {
        const where = utf8 ? "" : " (not UTF-8)";
        violations.push(`${name}:${index + 1}${where}: ${line.trim()}`);
      }
    });
  }
  return { violations, frozenMentions, allowedMentions };
}

export function main(root = REPOSITORY_ROOT, log = console.log, files = trackedFiles(root)) {
  const tracked = new Set(files);
  const missing = Object.keys(ALLOWED).filter((name) => !tracked.has(name));
  if (missing.length) {
    log(`::error::allowed file(s) no longer tracked: ${pyRepr(missing)}; update ALLOWED in ${SELF}`);
    return 1;
  }
  const { violations, frozenMentions, allowedMentions } = scan(root, files);
  for (const violation of violations) {
    log(`::error::the legacy runtime is named outside the allowed files: ${violation}`);
  }
  const miscounted = Object.entries(ALLOWED).filter(([name, [expected]]) => (allowedMentions.get(name) ?? 0) !== expected);
  for (const [name, [expected]] of miscounted) {
    log(
      `::error::${name} holds ${allowedMentions.get(name) ?? 0} mentioning line(s), ALLOWED records ${expected}; ` +
        `a new mention is not allowed, and a removed one makes the entry stale (update ALLOWED in ${SELF})`,
    );
  }
  if (frozenMentions !== FROZEN_MENTIONS) {
    log(
      `::error::the frozen migrations hold ${frozenMentions} mentioning line(s), expected exactly ${FROZEN_MENTIONS}. ` +
        "They are checksum-immutable, so a change means a new or renumbered file, or a pattern that stopped matching",
    );
  }
  if (violations.length || miscounted.length || frozenMentions !== FROZEN_MENTIONS) return 1;
  log(
    `legacy-runtime-name: ${files.length} tracked files clean; ${frozenMentions} mention(s) in frozen migrations, ` +
      `${Object.keys(ALLOWED).length} allowed file(s)`,
  );
  return 0;
}

if (isEntrypoint(import.meta.url)) {
  process.exitCode = main();
}
