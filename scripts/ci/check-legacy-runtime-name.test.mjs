// Scenarios for check-legacy-runtime-name.mjs. The name is assembled, so this
// file holds no mention of its own. The workflow step runs the check against
// the repository itself, so no scenario here repeats that scan.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { main, scan } from "./check-legacy-runtime-name.mjs";

const NAME = "Py" + "lon";

function tree(t, files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "legacy-name-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  for (const [name, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, name)), { recursive: true });
    fs.writeFileSync(path.join(root, name), text);
  }
  return [root, Object.keys(files)];
}

function run(root, files) {
  const lines = [];
  return [main(root, (line) => lines.push(line), files), lines.join("\n")];
}

test("a mention in any file is a violation with its line, in any case", (t) => {
  const [root, files] = tree(t, { "services/x/main.go": `package x\n// the ${NAME.toLowerCase()} way\n` });
  assert.deepEqual(scan(root, files), [`services/x/main.go:2: // the ${NAME.toLowerCase()} way`]);
});

test("migrations, fixtures and docs are not exempt", (t) => {
  const [root, files] = tree(t, {
    "services/elitea-main/migrations/shared/0001_x.sql": `-- ${NAME}\n`,
    "testdata/legacy/catalog.json": `{"source": "${NAME.toUpperCase()}_main"}\n`,
    "deploy/CUTOVER.md": `logs ${NAME.toLowerCase()}_main\n`,
  });
  assert.equal(scan(root, files).length, 3);
});

test("a path that names it is a violation", (t) => {
  const [root, files] = tree(t, { [`docs/${NAME}-notes.md`]: "clean\n" });
  assert.deepEqual(scan(root, files), [`docs/${NAME}-notes.md: the path names the legacy runtime`]);
});

test("a file that is not UTF-8 is still searched, and its violation says so", (t) => {
  const [root, files] = tree(t, { "notes.txt": "" });
  fs.writeFileSync(path.join(root, "notes.txt"), Buffer.from([0xff, 0xfe, ...Buffer.from(` ${NAME}\n`)]));
  const [violation] = scan(root, files);
  assert.match(violation, /^notes\.txt:1 \(not UTF-8\): /);
});

test("a clean tree passes, and a tree with one mention fails", (t) => {
  const [root, files] = tree(t, { "a.go": "package a\n", "b.md": `the ${NAME} way\n` });
  assert.equal(run(root, ["a.go"])[0], 0);
  const [status, output] = run(root, files);
  assert.equal(status, 1);
  assert.match(output, /::error::the legacy runtime is named: b\.md:1: /);
});

test("an empty file list fails rather than passing on nothing", () => {
  const [status, output] = run(os.tmpdir(), []);
  assert.equal(status, 1);
  assert.match(output, /no tracked files/);
});
