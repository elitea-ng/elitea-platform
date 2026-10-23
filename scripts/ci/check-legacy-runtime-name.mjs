#!/usr/bin/env node
// The legacy runtime's name appears nowhere in this repository.
//
// The platform used to sit beside a Python plugin runtime, and its name was
// written into comments, identifiers, routes, docs, migrations and recorded
// fixtures. The removal programme (PRs 1 to 10) took every functional use out
// and reworded the rest as "the legacy runtime". This check keeps it that way:
// it reads every tracked file, path and content, and fails on any mention.
// There is no exemption list.
//
// Migrations are not exempt either. The applied ones were reworded in their
// comments only; the migration runner still accepts the checksum each one was
// released with (releasedTexts in
// services/elitea-main/internal/infra/db/migrate/released.go), and a test
// proves their executable text did not change.
//
// A file that is not UTF-8 is searched as bytes, so an encoding cannot hide a
// mention.
//
// Run with the runner's node; only the standard library. This file spells the
// name only in pieces, so it holds no mention of its own.

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { isEntrypoint, readStrictUtf8, splitlines, trackedFiles } from "./py-compat.mjs";

const REPOSITORY_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

const MENTION = new RegExp("py" + "lon", "i");

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
  for (const name of files) {
    if (MENTION.test(name)) violations.push(`${name}: the path names the legacy runtime`);
    const [fileLines, utf8] = lines(path.join(root, name));
    fileLines.forEach((line, index) => {
      if (!MENTION.test(line)) return;
      const where = utf8 ? "" : " (not UTF-8)";
      violations.push(`${name}:${index + 1}${where}: ${line.trim()}`);
    });
  }
  return violations;
}

export function main(root = REPOSITORY_ROOT, log = console.log, files = trackedFiles(root)) {
  if (files.length === 0) {
    log("::error::no tracked files to search; a check that reads nothing proves nothing");
    return 1;
  }
  const violations = scan(root, files);
  for (const violation of violations) log(`::error::the legacy runtime is named: ${violation}`);
  if (violations.length) return 1;
  log(`legacy-runtime-name: ${files.length} tracked files clean`);
  return 0;
}

if (isEntrypoint(import.meta.url)) {
  process.exitCode = main();
}
