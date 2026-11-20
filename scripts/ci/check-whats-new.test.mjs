// node --test scripts/ci/check-whats-new.test.mjs (run by no-binaries.yml)
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { check, checkEntry, checkPage, ENTRIES, PAGE } from './check-whats-new.mjs';

const REPO = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const PAGE_OK = '---\ntitle: "What\'s new"\ndescription: "d"\n---\n\nIntro.\n\n<WhatsNew />\n\n<Info title="x">\ny\n</Info>\n';
const entry = (front, body = '## A change\n\nText.\n') => `---\n${front}\n---\n\n${body}`;

function tree(page, entries) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'whats-new-'));
  fs.mkdirSync(path.join(root, ENTRIES), { recursive: true });
  fs.mkdirSync(path.dirname(path.join(root, PAGE)), { recursive: true });
  fs.writeFileSync(path.join(root, PAGE), page);
  for (const [name, text] of Object.entries(entries)) fs.writeFileSync(path.join(root, ENTRIES, name), text);
  return root;
}

test('a well-formed entry passes, with or without order and with a ### subsection', () => {
  assert.deepEqual(checkEntry('2026-11-20-a-change.md', entry('date: 2026-11-20')), []);
  assert.deepEqual(checkEntry('2026-11-20-a-change.md', entry('date: 2026-11-20\norder: 2', '## A change\n\n### Detail\n\n```md\n## not a heading\n```\n')), []);
});

test('a badly named entry fails', () => {
  assert.match(checkEntry('change.md', entry('date: 2026-11-20')).join(), /YYYY-MM-DD-<slug>/);
  assert.match(checkEntry('2026-11-20-A-Change.md', entry('date: 2026-11-20')).join(), /lower-case/);
  assert.match(checkEntry('2026-02-30-x.md', entry('date: 2026-02-30')).join(), /not a date/);
});

test('front matter: date required, matching the name; order an integer; nothing else', () => {
  assert.match(checkEntry('2026-11-20-x.md', '## A change\n').join(), /front matter/);
  assert.match(checkEntry('2026-11-20-x.md', entry('order: 1')).join(), /no date/);
  assert.match(checkEntry('2026-11-20-x.md', entry('date: 2026-11-19')).join(), /differs from the file name/);
  assert.match(checkEntry('2026-11-20-x.md', entry('date: "2026-11-20"')).join(), /not YYYY-MM-DD/);
  assert.match(checkEntry('2026-11-20-x.md', entry('date: 2026-11-20\norder: first')).join(), /not an integer/);
  assert.match(checkEntry('2026-11-20-x.md', entry('date: 2026-11-20\ntitle: A change')).join(), /unknown front matter key "title"/);
  assert.match(checkEntry('2026-11-20-x.md', entry('date: 2026-11-20\ndate: 2026-11-20')).join(), /set twice/);
});

test('the body is one "## " section of plain Markdown', () => {
  assert.match(checkEntry('2026-11-20-x.md', entry('date: 2026-11-20', 'Text first.\n\n## A change\n')).join(), /must start with/);
  assert.match(checkEntry('2026-11-20-x.md', entry('date: 2026-11-20', '## One\n\n## Two\n')).join(), /one "## " heading/);
  assert.match(checkEntry('2026-11-20-x.md', entry('date: 2026-11-20', '## One\n\n<Note>x</Note>\n')).join(), /MDX components/);
  assert.deepEqual(checkEntry('2026-11-20-x.md', entry('date: 2026-11-20', '## One\n\nAn `<untrusted>` envelope and `<Note>` in code.\n')), []);
  assert.match(checkEntry('2026-11-20-x.md', entry('date: 2026-11-20', '## One\n<<<<<<< HEAD\na\n||||||| base\n=======\nb\n>>>>>>> x\n')).join(), /conflict marker/);
});

test('what-is-new.mdx fails when it gains a section or stops rendering the entries', () => {
  assert.deepEqual(checkPage(PAGE_OK), []);
  assert.match(checkPage(PAGE_OK.replace('<WhatsNew />', '## New thing\n\nText.\n\n<WhatsNew />')).join(), /"## New thing" is a section on the page/);
  assert.match(checkPage(PAGE_OK.replace('<WhatsNew />', '')).join(), /no longer renders <WhatsNew \/>/);
  assert.deepEqual(checkPage(PAGE_OK.replace('Intro.', 'Intro.\n\n```md\n## sample\n```')), []);
});

test('check() reads the tree: an empty folder, a stray file and a repeated heading fail', () => {
  assert.match(check(tree(PAGE_OK, {})).problems.join(), /holds no entry/);
  const stray = check(tree(PAGE_OK, { '2026-11-20-x.md': entry('date: 2026-11-20'), 'notes.txt': 'x' }));
  assert.match(stray.problems.join(), /only \.md entries/);
  const twice = check(tree(PAGE_OK, { '2026-11-20-x.md': entry('date: 2026-11-20'), '2026-11-19-y.md': entry('date: 2026-11-19') }));
  assert.match(twice.problems.join(), /same heading/);
  const ok = check(tree(PAGE_OK, { '2026-11-20-x.md': entry('date: 2026-11-20'), '2026-11-19-y.md': entry('date: 2026-11-19', '## Another\n') }));
  assert.deepEqual(ok, { problems: [], entries: 2 });
});

test('this repository passes', () => {
  const { problems, entries } = check(REPO);
  assert.deepEqual(problems, []);
  assert.ok(entries > 100, `only ${entries} entries`);
});
