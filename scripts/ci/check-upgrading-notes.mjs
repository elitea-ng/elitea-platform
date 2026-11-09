#!/usr/bin/env node
// Upgrade notes are one file per change (docs/upgrading/), never a shared list.
//
// THE RULE (owner, 2026-11-09). Every pull request used to add its entry at the
// top of docs/UPGRADING.md, so any two open pull requests conflicted there, and
// hand-resolved conflicts left diff3 markers on main (#1642). Now a pull request
// ADDS docs/upgrading/YYYY-MM-DD-<issue>-<slug>.md. docs/UPGRADING.md keeps the
// history (chart guards and values comments name its entries by title) and is
// closed: it holds exactly CLOSED_ENTRIES `## ` entries, so an added one fails.
//
// It also fails, rather than passes, when docs/upgrading/ holds no note at all:
// "nothing found, therefore correct" is how a gate stops gating in silence.
//
//   node scripts/ci/check-upgrading-notes.mjs [repo-root]

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

/** The `## ` entries docs/UPGRADING.md held when it was closed (2026-11-09). */
export const CLOSED_ENTRIES = 110;
export const NOTE_NAME = /^(\d{4})-(\d{2})-(\d{2})-(\d+)-[a-z0-9]+(?:-[a-z0-9]+)*\.md$/;
const CONFLICT = /^(<{7}|\|{7}|={7}|>{7})( |$)/m;

/** The problems in one note file (`name`, its `text`); [] when it is fine. */
export function checkNote(name, text) {
  const problems = [];
  const m = NOTE_NAME.exec(name);
  if (!m) {
    problems.push(`${name}: name it YYYY-MM-DD-<issue>-<slug>.md (lower-case slug)`);
  } else {
    const [, y, mo, d] = m.map(Number);
    const date = new Date(Date.UTC(y, mo - 1, d));
    if (date.getUTCFullYear() !== y || date.getUTCMonth() !== mo - 1 || date.getUTCDate() !== d) problems.push(`${name}: ${m[1]}-${m[2]}-${m[3]} is not a date`);
  }
  const first = text.split('\n').find((line) => line.trim() !== '');
  if (!first?.startsWith('# ')) problems.push(`${name}: the first line must be a "# " title`);
  if (/^## /m.test(text) && !/^# /m.test(text)) problems.push(`${name}: a "## " heading without a "# " title`);
  if (CONFLICT.test(text)) problems.push(`${name}: holds a merge conflict marker`);
  return problems;
}

/** The problems in docs/UPGRADING.md's text; [] when it is still closed. */
export function checkClosedFile(text) {
  const problems = [];
  const entries = (text.match(/^## /gm) ?? []).length;
  if (entries > CLOSED_ENTRIES) {
    problems.push(
      `docs/UPGRADING.md has ${entries} entries, ${entries - CLOSED_ENTRIES} more than when it was closed: add docs/upgrading/YYYY-MM-DD-<issue>-<slug>.md instead (docs/upgrading/README.md)`,
    );
  }
  if (CONFLICT.test(text)) problems.push('docs/UPGRADING.md: holds a merge conflict marker');
  return problems;
}

export function check(root) {
  const problems = checkClosedFile(fs.readFileSync(path.join(root, 'docs/UPGRADING.md'), 'utf8'));
  const dir = path.join(root, 'docs/upgrading');
  const notes = fs.readdirSync(dir).filter((n) => n.endsWith('.md') && n !== 'README.md');
  if (notes.length === 0) problems.push('docs/upgrading/ holds no note: the gate found nothing to check');
  for (const name of notes) problems.push(...checkNote(name, fs.readFileSync(path.join(dir, name), 'utf8')));
  for (const name of fs.readdirSync(dir)) {
    if (!name.endsWith('.md')) problems.push(`docs/upgrading/${name}: only .md notes belong here`);
  }
  return { problems, notes: notes.length };
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const root = process.argv[2] ?? path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
  const { problems, notes } = check(root);
  for (const p of problems) console.log(`::error::${p}`);
  if (problems.length) process.exit(1);
  console.log(`upgrade notes: ${notes} in docs/upgrading/, docs/UPGRADING.md closed at ${CLOSED_ENTRIES} entries`);
}
