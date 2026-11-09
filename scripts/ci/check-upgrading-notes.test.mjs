// node --test scripts/ci/check-upgrading-notes.test.mjs (run by no-binaries.yml)
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import { check, checkClosedFile, checkNote, CLOSED_ENTRIES } from './check-upgrading-notes.mjs';

const closed = (n) => `# Upgrading\n\n${Array.from({ length: n }, (_, i) => `## Entry ${i}\n\ntext\n`).join('\n')}`;

function tree(upgrading, notes) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'upgrading-'));
  fs.mkdirSync(path.join(root, 'docs/upgrading'), { recursive: true });
  fs.writeFileSync(path.join(root, 'docs/UPGRADING.md'), upgrading);
  for (const [name, text] of Object.entries(notes)) fs.writeFileSync(path.join(root, 'docs/upgrading', name), text);
  return root;
}

test('a well-formed note passes', () => {
  assert.deepEqual(checkNote('2026-11-09-1697-desktop-mdm-packages.md', '# Desktop: MDM packages (#1697)\n\nWhat to do.\n'), []);
});

test('a badly named note fails', () => {
  assert.match(checkNote('desktop.md', '# T\n').join(), /YYYY-MM-DD-<issue>-<slug>/);
  assert.match(checkNote('2026-11-09-desktop.md', '# T\n').join(), /YYYY-MM-DD/);
  assert.match(checkNote('2026-11-09-1697-Desktop.md', '# T\n').join(), /lower-case/);
  assert.match(checkNote('2026-02-30-1697-x.md', '# T\n').join(), /not a date/);
});

test('a note without a "# " title, or with a conflict marker, fails', () => {
  assert.match(checkNote('2026-11-09-1-x.md', '## Only a section\n').join(), /first line/);
  assert.match(checkNote('2026-11-09-1-x.md', '# T\n<<<<<<< HEAD\na\n||||||| base\n=======\nb\n>>>>>>> x\n').join(), /conflict marker/);
});

test('docs/UPGRADING.md stays closed: one more entry fails, the closed count passes', () => {
  assert.deepEqual(checkClosedFile(closed(CLOSED_ENTRIES)), []);
  assert.match(checkClosedFile(closed(CLOSED_ENTRIES + 1)).join(), /1 more than when it was closed/);
  assert.match(checkClosedFile(`${closed(1)}\n||||||| base\n`).join(), /conflict marker/);
});

test('the gate fails when it finds no note, and on a stray file', () => {
  assert.match(check(tree(closed(CLOSED_ENTRIES), {})).problems.join(), /holds no note/);
  assert.match(check(tree(closed(CLOSED_ENTRIES), { '2026-11-09-1-x.md': '# T\n', 'notes.txt': 'x' })).problems.join(), /only \.md/);
});

test('the repository passes', () => {
  const root = path.resolve(path.dirname(new URL(import.meta.url).pathname), '../..');
  const { problems, notes } = check(root);
  assert.deepEqual(problems, []);
  assert.ok(notes >= 1);
});
