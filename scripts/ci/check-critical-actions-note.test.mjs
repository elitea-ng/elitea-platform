// node --test scripts/ci/check-critical-actions-note.test.mjs (run by no-binaries.yml)
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { BEGIN, END, GOLDEN, NOTE, check, criticalEntries, renderBlock, write } from './check-critical-actions-note.mjs';

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const HEADER = '# method\troute\taction\tdanger\treason\televation\ttoken\n';
const golden = (...lines) => HEADER + lines.join('\n') + '\n';
const SET_ROLE = 'POST\t/api/v2/admin/auth_users/{mode}\tset_admin_role\tcritical\ttrue\tflag\tnever';
const DELETE = 'POST\t/api/v2/admin/auth_users/{mode}\tdelete\tdangerous\ttrue\tflag\tallowed';
const TRUST = 'DELETE\t/api/v2/admin/identity_providers/administration/{key}\t-\tcritical\ttrue\tsealed\tallowed';
const note = (block) => `# A note\n\nText.\n\n${block}\n\nMore.\n`;

test('only the critical entries are listed, with their token rule', () => {
  const entries = criticalEntries(golden(SET_ROLE, DELETE, TRUST));
  assert.deepEqual(entries.map((e) => e.action), ['set_admin_role', '-']);
  const block = renderBlock(entries);
  assert.match(block, /`set_admin_role` \| refused \|/);
  assert.match(block, /\/identity_providers\/administration\/\{key\}` \|  \| inherit token of a standing global administrator only \|/);
  assert.doesNotMatch(block, /delete/);
});

test('a current note passes', () => {
  const g = golden(SET_ROLE, TRUST);
  assert.deepEqual(check(note(renderBlock(criticalEntries(g))), g), []);
});

test('the guard fails when the registry gains a critical entry the note does not list', () => {
  const before = golden(SET_ROLE);
  const after = golden(SET_ROLE, TRUST);
  const problems = check(note(renderBlock(criticalEntries(before))), after);
  assert.match(problems.join('\n'), /does not list the critical actions/);
  assert.match(problems.join('\n'), /missing: .*identity_providers/);
});

test('the guard fails on a row the registry no longer has, and on a missing block', () => {
  const problems = check(note(renderBlock(criticalEntries(golden(SET_ROLE, TRUST)))), golden(SET_ROLE));
  assert.match(problems.join('\n'), /not in the registry: .*identity_providers/);
  assert.match(check('# A note\n', golden(SET_ROLE)).join(), /no generated block/);
});

test('--write regenerates the block and keeps the rest of the note', () => {
  const stale = note(`${BEGIN}\nold\n${END}`);
  const fresh = write(stale, golden(SET_ROLE));
  assert.deepEqual(check(fresh, golden(SET_ROLE)), []);
  assert.match(fresh, /^# A note\n\nText\.\n\n/);
  assert.match(fresh, /\n\nMore\.\n$/);
});

test('the note in this tree lists every critical entry of the registry', () => {
  const g = fs.readFileSync(path.join(REPO, GOLDEN), 'utf8');
  assert.deepEqual(check(fs.readFileSync(path.join(REPO, NOTE), 'utf8'), g), []);
  assert.ok(criticalEntries(g).length > 0);
});
