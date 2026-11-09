#!/usr/bin/env node
// Upgrade notes are one file per change, in docs/upgrading/ (#1833).
//
// Every pull request used to add its entry to the top of docs/UPGRADING.md,
// so any two open pull requests with an upgrade note had a merge conflict. A
// new file per change cannot conflict, and this check keeps it that way:
//
//   * docs/UPGRADING.md stays the short pointer: no `## ` entry, at most
//     POINTER_MAX_LINES lines, and it names docs/upgrading/;
//   * every docs/upgrading/<number>-<slug>.md has valid front matter
//     (`title`, `issue` equal to the file's number, `date`, optional `adr`
//     and `order`, nothing else), a body, no `#`/`##` heading in the body,
//     and a title and `###` sub-headings no other entry has (the docs page
//     puts every entry on one page, so a repeated heading is a repeated id);
//   * with --base (a pull request), the change ADDS fragment files and edits
//     only the ones it added: a fragment that is on the base branch is not
//     modified, deleted or renamed. UPGRADE_NOTE_EDIT_ALLOWED=true (the pull
//     request has the `upgrade-note-edit` label) allows a correction of a
//     merged entry.
//
// `order` only keeps the order the entries moved from docs/UPGRADING.md had
// within one day, so it is refused on an entry dated after MIGRATED_THROUGH.
//
// Usage (repository root, the runner's node, no dependencies):
//   node scripts/ci/check-upgrading-fragments.mjs              # tree only
//   node scripts/ci/check-upgrading-fragments.mjs --base <ref> # + the diff from <ref>
// On a pull request the checkout is the merge commit, so `--base HEAD^1`
// is the base branch head and the diff is exactly the pull request.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const SELF = fileURLToPath(import.meta.url);
const ROOT = path.resolve(path.dirname(SELF), "..", "..");

export const DIR = "docs/upgrading";
export const POINTER = "docs/UPGRADING.md";
export const README = `${DIR}/README.md`;
export const POINTER_MAX_LINES = 30;
// The 110 entries moved from docs/UPGRADING.md: a reader that stopped
// finding them must not pass.
export const MINIMUM_FRAGMENTS = 110;
// The newest date among the entries moved from docs/UPGRADING.md.
export const MIGRATED_THROUGH = "2026-11-08";

export const NAME = /^(\d{4,})-([a-z0-9]+(?:-[a-z0-9]+)*)\.md$/;
const KEYS = new Set(["title", "issue", "date", "adr", "order"]);

// A real calendar day, YYYY-MM-DD.
export function isDate(value) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) return false;
  const d = new Date(`${value}T00:00:00Z`);
  return !Number.isNaN(d.getTime()) && d.toISOString().slice(0, 10) === value;
}

// The front matter is a flat `key: value` list. `title` is a JSON string in
// double quotes (YAML reads the same text the same way), the others are bare.
export function parseFragment(name, source, { today } = {}) {
  const problems = [];
  const where = `${DIR}/${name}`;
  const match = NAME.exec(name);
  if (!match) {
    problems.push(`${where}: the name is not <number>-<slug>.md (at least four digits; lowercase letters, digits and single hyphens)`);
    return { problems };
  }
  const [, number, slug] = match;
  const fm = /^---\n([\s\S]*?)\n---\n/.exec(source);
  if (!fm) {
    problems.push(`${where}: no front matter (a block between two --- lines at the top)`);
    return { problems };
  }
  const data = {};
  for (const line of fm[1].split("\n")) {
    const kv = /^([a-z]+):[ ](.+)$/.exec(line);
    if (!kv) {
      problems.push(`${where}: front matter line ${JSON.stringify(line)} is not "key: value"`);
      continue;
    }
    const [, key, value] = kv;
    if (!KEYS.has(key)) problems.push(`${where}: unknown front matter key "${key}" (allowed: ${[...KEYS].join(", ")})`);
    else if (key in data) problems.push(`${where}: "${key}" is set twice`);
    else data[key] = value.trim();
  }
  let title;
  if (data.title === undefined) problems.push(`${where}: no title`);
  else {
    try {
      title = JSON.parse(data.title);
    } catch {
      title = undefined;
    }
    if (typeof title !== "string" || !data.title.startsWith('"')) {
      problems.push(`${where}: the title must be one string in double quotes`);
      title = undefined;
    } else if (title.trim() === "" || title !== title.trim()) problems.push(`${where}: the title is empty or has outer spaces`);
  }
  let issue;
  if (data.issue === undefined) problems.push(`${where}: no issue`);
  else if (!/^[1-9]\d*$/.test(data.issue)) problems.push(`${where}: issue "${data.issue}" is not a positive whole number`);
  else {
    issue = Number(data.issue);
    if (String(issue).padStart(4, "0") !== number) {
      problems.push(`${where}: the file name starts with ${number}, so issue must be ${Number(number)} and the name ${String(issue).padStart(4, "0")}-${slug}.md`);
    }
  }
  if (data.date === undefined) problems.push(`${where}: no date`);
  else if (!isDate(data.date)) problems.push(`${where}: date "${data.date}" is not a YYYY-MM-DD day`);
  else if (today && data.date > today) problems.push(`${where}: date ${data.date} is after today (${today})`);
  if (data.adr !== undefined && !/^ADR-\d{4}$/.test(data.adr)) problems.push(`${where}: adr "${data.adr}" is not ADR-NNNN`);
  if (data.order !== undefined) {
    if (!/^[1-9]\d*$/.test(data.order)) problems.push(`${where}: order "${data.order}" is not a positive whole number`);
    else if (data.date !== undefined && data.date > MIGRATED_THROUGH) {
      problems.push(`${where}: a new entry does not set "order"; its date and number place it (${README})`);
    }
  }

  const body = source.slice(fm[0].length);
  if (body.trim() === "") problems.push(`${where}: the body is empty`);
  const subheadings = [];
  let fence = null;
  for (const line of body.split("\n")) {
    const f = /^\s*(`{3,}|~{3,})/.exec(line);
    if (f) {
      if (fence === null) fence = f[1][0];
      else if (f[1][0] === fence) fence = null;
      continue;
    }
    if (fence !== null) continue;
    if (/^#{1,2}\s/.test(line)) problems.push(`${where}: the body has the heading ${JSON.stringify(line)}; the title is the entry's heading, use ### in the body`);
    const sub = /^#{3,6}\s+(.+?)\s*#*\s*$/.exec(line);
    if (sub) subheadings.push(sub[1]);
  }
  return {
    problems,
    fragment: { name, issue, slug, title, date: data.date, adr: data.adr, order: data.order === undefined ? undefined : Number(data.order), subheadings },
  };
}

export function checkPointer(source) {
  const problems = [];
  const lines = source.split("\n");
  if (lines.length > POINTER_MAX_LINES) problems.push(`${POINTER}: ${lines.length} lines; it is the fixed pointer (at most ${POINTER_MAX_LINES}). Put the entry in a new file in ${DIR}/ (${README})`);
  const entry = lines.find((l) => /^##\s/.test(l));
  if (entry) problems.push(`${POINTER}: has the entry ${JSON.stringify(entry)}; entries are files in ${DIR}/ (${README}), never in ${POINTER}`);
  if (!source.includes("docs/upgrading/") && !source.includes("](upgrading/")) problems.push(`${POINTER}: does not point to ${DIR}/`);
  return problems;
}

// files: Map<name, source> of docs/upgrading/ (README.md excluded).
export function checkTree(files, { today, minimum = MINIMUM_FRAGMENTS } = {}) {
  const problems = [];
  const fragments = [];
  for (const [name, source] of [...files].sort()) {
    const result = parseFragment(name, source, { today });
    problems.push(...result.problems);
    if (result.fragment) fragments.push(result.fragment);
  }
  if (files.size < minimum) problems.push(`${DIR}: ${files.size} fragment(s), expected at least ${minimum}; the reader stopped finding them`);
  const seen = new Map();
  const claim = (kind, value, name) => {
    const key = `${kind}\u0000${value.toLowerCase()}`;
    if (seen.has(key)) problems.push(`${DIR}/${name}: the ${kind} ${JSON.stringify(value)} is also in ${DIR}/${seen.get(key)}; the docs page gives each one id`);
    else seen.set(key, name);
  };
  for (const f of fragments) {
    if (f.issue !== undefined) claim("number and slug", `${f.issue}-${f.slug}`, f.name);
    if (f.title !== undefined) claim("title", f.title, f.name);
    for (const s of f.subheadings) claim("sub-heading", s, f.name);
  }
  return { problems, fragments };
}

// changes: [{ status: 'A'|'M'|'D'|..., path }] from `git diff --name-status --no-renames`.
export function checkDiff(changes, { editAllowed = false } = {}) {
  const problems = [];
  for (const { status, path: p } of changes) {
    if (!p.startsWith(`${DIR}/`) || p === README) continue;
    const name = p.slice(DIR.length + 1);
    if (name.includes("/")) {
      problems.push(`${p}: ${DIR}/ holds files only, no directories`);
      continue;
    }
    if (status === "A") continue;
    if (editAllowed) continue;
    const verb = status === "D" ? "deletes" : "modifies";
    problems.push(`${p}: this change ${verb} an upgrade note that is already on the base branch. Add a new file instead; to correct a merged entry, add the upgrade-note-edit label and push again (${README})`);
  }
  return problems;
}

function git(root, args) {
  return execFileSync("git", args, { cwd: root, encoding: "utf8", maxBuffer: 1 << 26, stdio: ["ignore", "pipe", "pipe"] });
}

export function readTree(root = ROOT) {
  const dir = path.join(root, DIR);
  const files = new Map();
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === "README.md") continue;
    if (entry.isDirectory()) files.set(`${entry.name}/`, "");
    else files.set(entry.name, fs.readFileSync(path.join(dir, entry.name), "utf8"));
  }
  return files;
}

// The options are for the tests: a throwaway repository, its environment,
// a fixed day and a smaller floor.
// The latest date a note may carry: tomorrow in UTC, because the runner's day
// is UTC and an author east of it writes their own day.
const latestDate = () => new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString().slice(0, 10);

export function main(argv, { root = ROOT, env = process.env, today = latestDate(), minimum = MINIMUM_FRAGMENTS, log = console } = {}) {
  const baseAt = argv.indexOf("--base");
  const base = baseAt === -1 ? undefined : argv[baseAt + 1];
  if (baseAt !== -1 && !base) {
    log.error("check-upgrading-fragments: --base needs a ref");
    return 2;
  }
  const problems = [];
  const files = readTree(root);
  problems.push(...checkPointer(fs.readFileSync(path.join(root, POINTER), "utf8")));
  const tree = checkTree(files, { today, minimum });
  problems.push(...tree.problems);
  let changed = 0;
  if (base) {
    let from;
    try {
      from = git(root, ["merge-base", base, "HEAD"]).trim();
    } catch (error) {
      log.error(`check-upgrading-fragments: cannot find the merge base of ${base} and HEAD (${error.message.split("\n")[0]}). A base it cannot read is a base it cannot compare against.`);
      return 2;
    }
    const changes = git(root, ["diff", "--name-status", "--no-renames", from, "HEAD", "--", DIR])
      .split("\n")
      .filter(Boolean)
      .map((line) => {
        const [status, p] = line.split("\t");
        return { status: status[0], path: p };
      });
    changed = changes.length;
    problems.push(...checkDiff(changes, { editAllowed: env.UPGRADE_NOTE_EDIT_ALLOWED === "true" }));
  }
  if (problems.length > 0) {
    for (const p of problems) log.error(`::error::${p}`);
    log.error(`check-upgrading-fragments: FAIL, ${problems.length} problem(s)`);
    return 1;
  }
  log.log(`check-upgrading-fragments: OK: ${tree.fragments.length} upgrade notes${base ? `, ${changed} changed file(s) since ${base}` : ""}.`);
  return 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === SELF) process.exitCode = main(process.argv.slice(2));
