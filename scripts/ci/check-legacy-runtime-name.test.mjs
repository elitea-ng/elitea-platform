// Scenarios for check-legacy-runtime-name.mjs. The name is assembled, so this
// file holds no mention of its own.

import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { ALLOWED, FROZEN_FILE_NAMES, isFrozenMigration, main, scan } from "./check-legacy-runtime-name.mjs";

const NAME = "Py" + "lon";

function tree(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "no-legacy-name-"));
  for (const [name, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, name)), { recursive: true });
    fs.writeFileSync(path.join(root, name), text);
  }
  return [root, Object.keys(files)];
}

test("a mention in any other file is a violation with its line, in any case", () => {
  const [root, files] = tree({ "services/x/main.go": `package x\n// the ${NAME.toLowerCase()} way\n` });
  assert.deepEqual(scan(root, files).violations, [`services/x/main.go:2: // the ${NAME.toLowerCase()} way`]);
});

test("frozen migrations are counted, not reported; a new one above the head is reported", () => {
  const frozen = "services/elitea-main/migrations/shared/0090_old.sql";
  const fresh = "services/elitea-main/migrations/shared/0999_new.sql";
  const [root, files] = tree({ [frozen]: `-- ${NAME}\n-- ${NAME}\n`, [fresh]: `-- ${NAME}\n` });
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
});

test("a path that names it is a violation unless it is a frozen migration", () => {
  const frozenFile = `services/elitea-main/migrations/shared/${FROZEN_FILE_NAMES[0]}.sql`;
  const [root, files] = tree({ [frozenFile]: "-- x\n", [`docs/${NAME}-notes.md`]: "clean\n" });
  assert.deepEqual(scan(root, files).violations, [`docs/${NAME}-notes.md: the path names the legacy runtime`]);
});

test("any file may name a frozen migration's file, and nothing else on that line", () => {
  const [root, files] = tree({
    "manifest_test.go": `// 91: shared/${FROZEN_FILE_NAMES[0]}.sql, which withdraws\n// and ${NAME} too: ${FROZEN_FILE_NAMES[1]}\n`,
  });
  assert.deepEqual(scan(root, files).violations, [
    `manifest_test.go:2: // and ${NAME} too: ${FROZEN_FILE_NAMES[1]}`,
  ]);
});

test("an allowed file's mentions are recorded as seen, not reported", () => {
  const allowed = Object.keys(ALLOWED)[0];
  const [root, files] = tree({ [allowed]: `"${NAME}"\n` });
  const { violations, allowedSeen } = scan(root, files);
  assert.deepEqual(violations, []);
  assert.deepEqual([...allowedSeen], [allowed]);
});

test("a file that is not UTF-8 is skipped", () => {
  const [root, files] = tree({ "blob.bin": "" });
  fs.writeFileSync(path.join(root, "blob.bin"), Buffer.from([0xff, 0xfe, ...Buffer.from(NAME)]));
  assert.deepEqual(scan(root, files).violations, []);
});

test("the repository itself passes", () => {
  const lines = [];
  assert.equal(main(undefined, (line) => lines.push(line)), 0, lines.join("\n"));
});
