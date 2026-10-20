// Scenarios for check-e2e-paths.mjs (new with the port: the Python version
// had none). The matcher and the Containerfile reader decide what the check
// believes the E2E lanes depend on, so they are held here.

import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

import { containerfileInputs, covered, scriptReferences } from "./check-e2e-paths.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..", "..");

test("a directory input is covered only by a pattern that takes the whole directory", () => {
  assert.equal(covered("libs/go", ["libs/go/**"]), true);
  assert.equal(covered("libs/go", ["libs/**"]), true);
  assert.equal(covered("libs/go", ["libs/go/*.go"]), false);
  assert.equal(covered("scripts/ci/e2e-lane-filter.sh", ["scripts/ci/*.sh"]), true);
  assert.equal(covered("scripts/ci/e2e-lane-filter.sh", ["scripts/ci/e2e-lane-filter.sh"]), true);
  assert.equal(covered("scripts/ci/e2e-lane-filter.sh", ["scripts/**/other.sh"]), false);
});

test("COPY and ADD sources are inputs; --from, heredocs and URLs are not", () => {
  const dir = fs.mkdtempSync(path.join(ROOT, "deploy", ".e2e-paths-test-"));
  try {
    const file = path.join(dir, "Containerfile");
    fs.writeFileSync(
      file,
      [
        "FROM debian AS base",
        "COPY a.txt \\",
        "     b/ /dst/",
        "copy --chown=1:1 c.txt /dst/",
        "COPY --from=base /x /y",
        "ADD https://example.invalid/z.tgz /z",
        "COPY <<EOF /etc/x",
        "EOF",
        "RUN echo COPY d.txt",
      ].join("\n"),
    );
    const rel = path.relative(ROOT, dir);
    assert.deepEqual([...containerfileInputs(dir, file)].sort(), [
      `${rel}/Containerfile`,
      `${rel}/a.txt`,
      `${rel}/b`,
      `${rel}/c.txt`,
    ]);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

test("script references are scripts/ paths ending .sh, .py or .mjs", () => {
  const text = 'run: bash scripts/ci/x.sh && node "${GITHUB_WORKSPACE}/scripts/ci/y.mjs" scripts/z.py scripts/ci/notes.md';
  assert.deepEqual([...scriptReferences(text)].sort(), ["scripts/ci/x.sh", "scripts/ci/y.mjs", "scripts/z.py"]);
});
