#!/usr/bin/env node
// What's new entries are one file per change, never a section of the page.
//
// THE RULE (2026-11-20). Every feature pull request used to add a `## ` section
// at the top of apps/elitea-docs/src/content/docs/what-is-new.mdx, so every
// merge gave every other open pull request a conflict there. Now a pull request
// ADDS apps/elitea-docs/src/content/whats-new/YYYY-MM-DD-<slug>.md and never
// edits what-is-new.mdx, which renders the folder through <WhatsNew />.
//
// An entry is:
//
//   ---
//   date: 2026-11-20      # the file name's date; newest first on the page
//   order: 1              # optional integer: higher first among one date's entries
//   ---
//
//   ## The heading, as it shows on the page
//
//   Markdown paragraphs (`### ` subsections allowed, no MDX components).
//
// The page sorts by date, then order, then file name (src/whats-new.ts), not
// by merge order; `order` lives in the entry's own file, so it shares nothing. It
// fails, rather than passes, when the folder holds no entry: "nothing found,
// therefore correct" is how a gate stops gating in silence.
//
//   node scripts/ci/check-whats-new.mjs [repo-root]

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

export const PAGE = 'apps/elitea-docs/src/content/docs/what-is-new.mdx';
export const ENTRIES = 'apps/elitea-docs/src/content/whats-new';
export const ENTRY_NAME = /^(\d{4})-(\d{2})-(\d{2})-[a-z0-9]+(?:-[a-z0-9]+)*\.md$/;
// No `=======` alternative: a setext heading underline is that line too, and
// a real conflict always carries the `<<<<<<<` and `>>>>>>>` lines.
const CONFLICT = /^(<{7}|\|{7}|>{7})( |$)/m;
const FRONT = /^---\n([\s\S]*?)\n---(?:\n|$)/;
const KEYS = new Set(['date', 'order']);

/** CRLF and LF read alike, as the Astro loader reads them. */
const lf = (text) => text.replace(/\r\n?/g, '\n');

/**
 * Lines outside code fences, so a code sample's `## ` is not a heading. A
 * fence closes only on its own character, at least as long (CommonMark), so
 * a ``` line inside a ~~~ block stays code.
 */
function proseLines(text) {
  let fence;
  return text.split('\n').filter((line) => {
    const m = /^ {0,3}(`{3,}|~{3,})/.exec(line);
    if (m && !fence) {
      fence = m[1];
      return false;
    }
    if (m && fence && m[1][0] === fence[0] && m[1].length >= fence.length && line.trim() === m[1]) {
      fence = undefined;
      return false;
    }
    return !fence;
  });
}

/**
 * The anchor id a heading gets: github-slugger's rule, which the Markdown
 * pipeline uses (lower case, drop everything but letters, marks, numbers,
 * connector punctuation, spaces and hyphens, spaces to hyphens). Each entry
 * renders on its own, so no slugger de-duplicates across entries: two equal
 * slugs would be one anchor twice.
 */
export function slugOf(heading) {
  return heading.toLowerCase().replace(/[^\p{L}\p{M}\p{N}\p{Pc} -]/gu, '').replace(/ /g, '-');
}

function validDate(y, m, d) {
  const date = new Date(Date.UTC(y, m - 1, d));
  return date.getUTCFullYear() === y && date.getUTCMonth() === m - 1 && date.getUTCDate() === d;
}

/** The problems in one entry file (`name`, its `text`); [] when it is fine. */
export function checkEntry(name, raw) {
  const text = lf(raw);
  const problems = [];
  const where = `${ENTRIES}/${name}`;
  const m = ENTRY_NAME.exec(name);
  if (!m) problems.push(`${where}: name it YYYY-MM-DD-<slug>.md (lower-case slug)`);
  else if (!validDate(Number(m[1]), Number(m[2]), Number(m[3]))) problems.push(`${where}: ${m[1]}-${m[2]}-${m[3]} is not a date`);
  if (CONFLICT.test(text)) problems.push(`${where}: holds a merge conflict marker`);

  const front = FRONT.exec(text);
  if (!front) {
    problems.push(`${where}: starts without a "---" front matter block holding date:`);
    return problems;
  }
  const fields = new Map();
  for (const line of front[1].split('\n')) {
    if (line.trim() === '' || line.trimStart().startsWith('#')) continue;
    const kv = /^([A-Za-z_][\w-]*):\s*(.*?)\s*$/.exec(line);
    if (!kv) {
      problems.push(`${where}: front matter line "${line}" is not "key: value"`);
      continue;
    }
    if (!KEYS.has(kv[1])) problems.push(`${where}: unknown front matter key "${kv[1]}" (only date and order)`);
    if (fields.has(kv[1])) problems.push(`${where}: front matter key "${kv[1]}" is set twice`);
    fields.set(kv[1], kv[2]);
  }
  const date = fields.get('date');
  if (date === undefined) problems.push(`${where}: front matter has no date:`);
  else if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) problems.push(`${where}: date "${date}" is not YYYY-MM-DD (unquoted)`);
  else if (m && date !== `${m[1]}-${m[2]}-${m[3]}`) problems.push(`${where}: date ${date} differs from the file name's ${m[1]}-${m[2]}-${m[3]}`);
  const order = fields.get('order');
  if (order !== undefined && !/^-?\d+$/.test(order)) problems.push(`${where}: order "${order}" is not an integer`);

  const body = text.slice(front[0].length);
  const lines = proseLines(body);
  const first = lines.find((line) => line.trim() !== '');
  if (!first?.startsWith('## ')) problems.push(`${where}: the body must start with its "## " heading`);
  const top = lines.filter((line) => /^#{1,2} /.test(line));
  if (top.length > 1 || top.some((line) => line.startsWith('# '))) {
    problems.push(`${where}: one "## " heading per entry (use "### " inside it, or add a second entry file)`);
  }
  const prose = lines.join('\n').replace(/`[^`\n]*`/g, '');
  if (/<\/?[A-Z][A-Za-z0-9]*[\s/>]/.test(prose)) {
    problems.push(`${where}: a <Tag> outside backticks: an entry is plain Markdown, so an MDX component does not render and a type such as Vec<String> vanishes as HTML; wrap it in backticks`);
  }
  return problems;
}

/** The heading of an entry, for the duplicate-anchor check. */
export function headingOf(text) {
  const body = lf(text).replace(FRONT, '');
  return proseLines(body).find((line) => line.startsWith('## '))?.slice(3).trim();
}

/** The problems in what-is-new.mdx's text; [] when it holds no section. */
export function checkPage(raw) {
  const text = lf(raw);
  const problems = [];
  const sections = proseLines(text.replace(FRONT, '')).filter((line) => /^#{1,2} /.test(line));
  for (const line of sections) {
    problems.push(`${PAGE}: "${line}" is a section on the page; add ${ENTRIES}/YYYY-MM-DD-<slug>.md instead and leave the page as it is`);
  }
  if (!/<WhatsNew\s*\/>/.test(text)) problems.push(`${PAGE}: no longer renders <WhatsNew />, so no entry shows`);
  if (CONFLICT.test(text)) problems.push(`${PAGE}: holds a merge conflict marker`);
  return problems;
}

export function check(root) {
  const problems = checkPage(fs.readFileSync(path.join(root, PAGE), 'utf8'));
  const dir = path.join(root, ENTRIES);
  const names = fs.readdirSync(dir).sort();
  const entries = names.filter((n) => n.endsWith('.md'));
  if (entries.length === 0) problems.push(`${ENTRIES}/ holds no entry: the gate found nothing to check`);
  for (const name of names) {
    if (!name.endsWith('.md')) problems.push(`${ENTRIES}/${name}: only .md entries belong here`);
  }
  const headings = new Map();
  for (const name of entries) {
    const text = fs.readFileSync(path.join(dir, name), 'utf8');
    problems.push(...checkEntry(name, text));
    const heading = headingOf(text);
    if (heading === undefined) continue;
    const slug = slugOf(heading);
    if (headings.has(slug)) problems.push(`${ENTRIES}/${name}: its heading's anchor #${slug} is ${headings.get(slug)}'s too; reword one heading`);
    else headings.set(slug, name);
  }
  return { problems, entries: entries.length };
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const root = process.argv[2] ?? path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
  const { problems, entries } = check(root);
  for (const p of problems) console.log(`::error::${p}`);
  if (problems.length) process.exit(1);
  console.log(`what's new: ${entries} entries in ${ENTRIES}/, ${PAGE} holds no section`);
}
