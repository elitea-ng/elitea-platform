// Scenarios for check-legacy-runtime-name.mjs. The name is assembled, so this
// file holds no mention of its own. The workflow step runs the check against
// the repository itself, so no scenario here repeats that scan.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import {
  ALLOWED,
  FROZEN_FILE_NAMES,
  FROZEN_MENTIONS,
  isFrozenMigration,
  main,
  scan,
} from "./check-legacy-runtime-name.mjs";

const NAME = "Py" + "lon";
const FROZEN = "services/elitea-main/migrations/shared/0090_old.sql";

function tree(t, files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "legacy-name-"));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  for (const [name, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, name)), { recursive: true });
    fs.writeFileSync(path.join(root, name), text);
  }
  return [root, Object.keys(files)];
}

// A tree that passes main(): every allowed file at its recorded count, and the
// frozen total in one migration. A scenario then breaks exactly one thing.
function passingTree(t, overrides = {}) {
  const files = { [FROZEN]: `-- ${NAME}\n`.repeat(FROZEN_MENTIONS) };
  for (const [name, [count]] of Object.entries(ALLOWED)) files[name] = `"${NAME}"\n`.repeat(count);
  return tree(t, { ...files, ...overrides });
}

function run(root, files) {
  const lines = [];
  return [main(root, (line) => lines.push(line), files), lines.join("\n")];
}

test("a mention in any other file is a violation with its line, in any case", (t) => {
  const [root, files] = tree(t, { "services/x/main.go": `package x\n// the ${NAME.toLowerCase()} way\n` });
  assert.deepEqual(scan(root, files).violations, [`services/x/main.go:2: // the ${NAME.toLowerCase()} way`]);
});

test("frozen migrations are counted, not reported; a new one above the head is reported", (t) => {
  const fresh = "services/elitea-main/migrations/shared/0999_new.sql";
  const [root, files] = tree(t, { [FROZEN]: `-- ${NAME}\n-- ${NAME}\n`, [fresh]: `-- ${NAME}\n` });
  const { violations, frozenMentions } = scan(root, files);
  assert.equal(frozenMentions, 2);
  assert.deepEqual(violations, [`${fresh}:1: -- ${NAME}`]);
});

test("the frozen heads separate old migrations from new ones in both scopes", () => {
  assert.equal(isFrozenMigration("services/elitea-main/migrations/shared/0172_x.sql"), true);
  assert.equal(isFrozenMigration("services/elitea-main/migrations/shared/0173_x.sql"), false);
  assert.equal(isFrozenMigration("services/elitea-main/migrations/tenant/0154_x.sql"), true);
  assert.equal(isFrozenMigration("services/elitea-main/migrations/tenant/0155_x.sql"), false);
  assert.equal(isFrozenMigration("services/elitea-main/migrations/shared/0090_x_test.go"), false);
  assert.equal(isFrozenMigration("services/elitea-main/migrations/shared/sub/0090_x.sql"), false);
});

test("a path that names it is a violation unless it is a frozen migration", (t) => {
  const frozenFile = `services/elitea-main/migrations/shared/${FROZEN_FILE_NAMES[0]}.sql`;
  const [root, files] = tree(t, { [frozenFile]: "-- x\n", [`docs/${NAME}-notes.md`]: "clean\n" });
  assert.deepEqual(scan(root, files).violations, [`docs/${NAME}-notes.md: the path names the legacy runtime`]);
});

test("any file may name a frozen migration's file, and nothing else on that line", (t) => {
  const [root, files] = tree(t, {
    "manifest_test.go": `// 91: shared/${FROZEN_FILE_NAMES[0]}.sql, which withdraws\n// and ${NAME} too: ${FROZEN_FILE_NAMES[1]}\n`,
  });
  assert.deepEqual(scan(root, files).violations, [`manifest_test.go:2: // and ${NAME} too: ${FROZEN_FILE_NAMES[1]}`]);
});

test("a file that is not UTF-8 is still searched, and its violation says so", (t) => {
  const [root, files] = tree(t, { "notes.txt": "" });
  fs.writeFileSync(path.join(root, "notes.txt"), Buffer.from([0xff, 0xfe, ...Buffer.from(` ${NAME}\n`)]));
  const [violation] = scan(root, files).violations;
  assert.match(violation, /^notes\.txt:1 \(not UTF-8\): /);
});

test("a tree that matches every recorded count passes", (t) => {
  const [root, files] = passingTree(t);
  assert.deepEqual(run(root, files), [0, run(root, files)[1]]);
});

test("one more line in an allowed file fails: the exemption is a count, not a licence", (t) => {
  const [allowed, [count]] = Object.entries(ALLOWED)[0];
  const [root, files] = passingTree(t, { [allowed]: `"${NAME}"\n`.repeat(count + 1) });
  const [status, output] = run(root, files);
  assert.equal(status, 1);
  assert.match(output, new RegExp(`${allowed} holds ${count + 1} mentioning line\\(s\\), ALLOWED records ${count}`));
});

test("an allowed file that lost its mentions fails as stale", (t) => {
  const [allowed, [count]] = Object.entries(ALLOWED)[0];
  const [root, files] = passingTree(t, { [allowed]: "clean\n" });
  const [status, output] = run(root, files);
  assert.equal(status, 1);
  assert.match(output, new RegExp(`${allowed} holds 0 mentioning line\\(s\\), ALLOWED records ${count}`));
});

test("an allowed file that is no longer tracked fails", (t) => {
  const [allowed] = Object.keys(ALLOWED);
  const [root, files] = passingTree(t);
  const [status, output] = run(root, files.filter((name) => name !== allowed));
  assert.equal(status, 1);
  assert.match(output, /allowed file\(s\) no longer tracked/);
});

test("a frozen total that moved fails, in either direction", (t) => {
  for (const total of [FROZEN_MENTIONS - 1, FROZEN_MENTIONS + 1]) {
    const [root, files] = passingTree(t, { [FROZEN]: `-- ${NAME}\n`.repeat(total) });
    const [status, output] = run(root, files);
    assert.equal(status, 1);
    assert.match(output, new RegExp(`hold ${total} mentioning line\\(s\\), expected exactly ${FROZEN_MENTIONS}`));
  }
});
