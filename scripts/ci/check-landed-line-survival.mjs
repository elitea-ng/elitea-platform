#!/usr/bin/env node
/*
Check that the work of a merged pull request is still in the tree.

Issue #541. A squash merge writes whole files. A squash from a branch that was
cut before an earlier pull request landed therefore DELETES that pull request's
work, and it leaves the deleted pull request in the history. Pull request #502
corrected the logout flight and its journey. Pull request #516 merged four
hours later from a stale branch and restored the five files to their pre-#502
copies, byte for byte.

The check that was run then was:

    git merge-base --is-ancestor <the pull request> <the head>   -> true

That answer is correct and it is not evidence. Ancestry reports the commit.
It says nothing about the content. This checker reads the content.

WHAT IT DOES

It walks the first-parent chain of a tip commit. Each commit on that chain is
one merged pull request. For each of them it takes the diff against the
commit's own parent, which is the base the pull request merged onto, and it
looks for the lines the pull request added in the tree at the tip.

It reports three states per file, and only the first two are faults:

  restored  A later commit set the file back to its EXACT byte content from
            before the pull request, and lines the pull request added are
            still missing at the tip. This is the #502 signature. It needs no
            threshold and no judgement: the blob object id matches.

  reverted  At the tip NONE of the lines the pull request added are present,
            and EVERY measurable line of the file AS IT WAS BEFORE the pull
            request is present again. The old text came back whole, without a
            byte-exact match, because a later commit edited the file as well.

  rewritten The added lines are gone, and the removed lines did not come back.
            An ordinary rewrite, a regeneration or a deliberate removal looks
            like this. The checker PRINTS these and does not fail on them.

For each fault it names the pull request that added the work AND the pull
request that deleted it, because the second one is the one to fix.

WHAT IT CANNOT SEE

- A file that the tip does not hold. A rename and a deletion both look like a
  total loss, and both are usually deliberate, so the checker skips the path.
- A binary file, and a file that does not decode as UTF-8.
- Short lines, blank lines and comment-only lines. A brace or an import is
  common text, so its presence at the tip proves nothing.
- A loss inside a pull request that never reached the trunk as its own commit.
  This repository squashes, and it has also landed several pull requests in one
  trunk commit. The checker measures what the trunk holds.

WHY IT CANNOT FAIL THE PULL REQUEST THAT RUNS IT

The tip is a COMMIT, and the workflow passes the base branch head. The tree
under review never enters the measurement, so a pull request that legitimately
rewrites lines an earlier pull request added cannot make this checker red. The
checker therefore reports a fault that ALREADY merged. It turns a silent
deletion into a red check on the next pull request, and it names the merge that
made it.

Ported from check_landed_line_survival.py (2026-10-20, CI scripts run with the
runner's node). Text is handled the way Python handled it: lines split as
str.splitlines() splits them, whitespace stripped as str.strip() strips it,
lengths and slices counted in code points.
*/

import { spawn, spawnSync } from "node:child_process";
import fs from "node:fs";
import { fileURLToPath } from "node:url";

// A line must carry this much text before its absence means anything. A brace,
// a bracket or a one-word statement appears everywhere.
export const MIN_LINE_LENGTH = 12;
export const MIN_ALNUM_CHARS = 3;

// A "reverted" verdict needs this many measurable added lines. Below it the
// sample is too small to separate a stale squash from an edit.
export const MIN_ADDED_FOR_REVERT = 3;

// A "reverted" verdict also needs this much of the file from BEFORE the pull
// request back at the tip. A first version asked only that the lines the pull
// request REMOVED came back, and that rule called one honest replacement a
// fault: pull request #202 replaced a 160-line end-to-end stub with the real
// 56-line admin entry, and the single line #82 had removed was in it. Ask for
// the whole old file instead. Only a revert brings that back.
export const MIN_OLD_TEXT_BACK = 1.0;

// A comment carries no behaviour, and comment text repeats across files.
const COMMENT_PREFIXES = ["//", "#", "*", "/*", "--", "<!--", ";", '"""', "'''"];

export const DEFAULT_LIMIT = 40;
export const DEFAULT_ALLOWLIST = "scripts/ci/landed-line-survival-allowlist.txt";

const PR_NUMBER = /#(\d+)/g;

const DESCRIPTION = "Check that the work of a merged pull request is still in the tree.";

// ── Python's string semantics ────────────────────────────────────────────

// str.isspace() code points, the set str.strip() and str.split() use.
const PY_SPACE = "\\t\\n\\x0b\\x0c\\r\\x1c-\\x1f \\x85\\xa0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const STRIP = new RegExp(`^[${PY_SPACE}]+|[${PY_SPACE}]+$`, "gu");
const SPLIT_WS = new RegExp(`[${PY_SPACE}]+`, "u");
const LINE_BREAK = /\r\n|[\n\r\x0b\x0c\x1c\x1d\x1e\x85\u2028\u2029]/;

export function pyStrip(text) {
  return text.replace(STRIP, "");
}

export function pySplitlines(text) {
  if (text === "") return [];
  const lines = text.split(LINE_BREAK);
  if (lines[lines.length - 1] === "") lines.pop();
  return lines;
}

/** text[:n], counted in code points. */
function head(text, n) {
  return Array.from(text).slice(0, n).join("");
}

function codePointLength(text) {
  let n = 0;
  for (const _ of text) n += 1;
  return n;
}

const ALNUM = /[\p{L}\p{N}]/u;

function pyStr(value) {
  return value === null || value === undefined ? "None" : String(value);
}

// ── git ──────────────────────────────────────────────────────────────────

export class GitError extends Error {}

/**
 * Run git and return stdout. Throw on a non-zero exit.
 *
 * Decode with replacement. A tracked file can hold bytes that are not UTF-8
 * while git still treats it as text, and a diff that carries one of those
 * bytes must not stop the whole walk. A replaced byte can only make a line
 * look unfamiliar, which this checker already treats as a rewrite and not as
 * a fault.
 */
export function git(repo, ...args) {
  const proc = spawnSync("git", ["-C", repo, ...args], {
    stdio: ["ignore", "pipe", "pipe"],
    maxBuffer: 1 << 30,
  });
  if (proc.error) throw proc.error;
  if (proc.status !== 0) {
    throw new GitError(`git ${args.join(" ")} failed: ${pyStrip(proc.stderr.toString("utf8"))}`);
  }
  return proc.stdout.toString("utf8");
}

/** Run git and report only whether it succeeded. */
export function gitOk(repo, ...args) {
  const proc = spawnSync("git", ["-C", repo, ...args], { stdio: "ignore" });
  return proc.status === 0;
}

const STRICT_UTF8 = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });

/**
 * Read blobs through one long-lived `git cat-file --batch` process.
 *
 * The checker reads the same files many times. One process removes several
 * thousand `git show` invocations.
 */
export class BlobReader {
  constructor(repo) {
    this._proc = spawn("git", ["-C", repo, "cat-file", "--batch"], {
      stdio: ["pipe", "pipe", "inherit"],
    });
    this._cache = new Map();
    this._chunks = [];
    this._length = 0;
    this._ended = false;
    this._wake = null;
    this._exited = new Promise((resolve) => this._proc.on("close", resolve));
    this._proc.stdout.on("data", (chunk) => {
      this._chunks.push(chunk);
      this._length += chunk.length;
      this._notify();
    });
    this._proc.stdout.on("end", () => {
      this._ended = true;
      this._notify();
    });
  }

  _notify() {
    const wake = this._wake;
    this._wake = null;
    if (wake) wake();
  }

  _wait() {
    return new Promise((resolve) => {
      this._wake = resolve;
    });
  }

  _take(n) {
    const all = this._chunks.length === 1 ? this._chunks[0] : Buffer.concat(this._chunks);
    const out = all.subarray(0, n);
    const rest = all.subarray(n);
    this._chunks = rest.length ? [rest] : [];
    this._length = rest.length;
    return out;
  }

  async _readLine() {
    for (;;) {
      let offset = 0;
      for (const chunk of this._chunks) {
        const at = chunk.indexOf(10);
        if (at !== -1) return this._take(offset + at + 1);
        offset += chunk.length;
      }
      if (this._ended) return this._take(this._length);
      await this._wait();
    }
  }

  async _read(n) {
    while (this._length < n && !this._ended) await this._wait();
    return this._take(Math.min(n, this._length));
  }

  async close() {
    this._proc.stdin.end();
    await this._exited;
  }

  /** Return the blob as text. Return null for a missing or binary blob. */
  async text(oid) {
    if (this._cache.has(oid)) return this._cache.get(oid);
    this._proc.stdin.write(`${oid}\n`);
    const header = pyStrip((await this._readLine()).toString("utf8"));
    if (header.endsWith(" missing") || !header.includes(" blob ")) {
      // Another object type still prints its content: read past it.
      const size = Number.parseInt(header.slice(header.lastIndexOf(" ") + 1), 10);
      if (!header.endsWith(" missing") && Number.isFinite(size) && header.split(" ").length === 3) {
        await this._read(size + 1);
      }
      this._cache.set(oid, null);
      return null;
    }
    const size = Number.parseInt(header.slice(header.lastIndexOf(" ") + 1), 10);
    const payload = await this._read(size);
    await this._read(1); // the trailing newline cat-file adds
    let value;
    try {
      value = STRICT_UTF8.decode(payload);
    } catch {
      value = null;
    }
    this._cache.set(oid, value);
    return value;
  }
}

// ── the measurement ──────────────────────────────────────────────────────

/** Report whether the absence of this line means anything. */
export function measurable(line) {
  const text = pyStrip(line);
  if (codePointLength(text) < MIN_LINE_LENGTH) return false;
  if (COMMENT_PREFIXES.some((prefix) => text.startsWith(prefix))) return false;
  let alnum = 0;
  for (const char of text) if (ALNUM.test(char)) alnum += 1;
  if (alnum < MIN_ALNUM_CHARS) return false;
  return true;
}

/**
 * Return the pull request that merged the commit.
 *
 * A subject can name several issues and then its own pull request, as in
 * "... (#602, #603, #604) (#605)". The merging pull request is the last one.
 */
export function pullRequestNumber(subject) {
  const found = [...subject.matchAll(PR_NUMBER)].map((m) => m[1]);
  return found.length ? found[found.length - 1] : null;
}

/**
 * Return [path, source blob, destination blob] for each changed file.
 *
 * `git diff --raw -z` is the authoritative list. It never quotes a path, so a
 * path that holds a space or a quote survives it intact.
 *
 * `--abbrev=40` is load-bearing. `--raw` abbreviates a blob object id by
 * default, and an abbreviated id never equals the full id that `rev-parse`
 * returns. The exact-blob comparison that finds a restored file then always
 * answers "no", and so does the test for the empty object id.
 */
export function rawChanges(repo, commit) {
  const out = git(repo, "diff", "--raw", "--abbrev=40", "--no-renames", "--no-ext-diff", "-z", `${commit}^`, commit);
  const fields = out.split("\0");
  const changes = [];
  let index = 0;
  while (index < fields.length) {
    const meta = fields[index];
    if (!meta.startsWith(":")) {
      index += 1;
      continue;
    }
    const parts = meta.slice(1).split(" ");
    if (parts.length < 5) {
      index += 2;
      continue;
    }
    const [sourceBlob, destinationBlob] = [parts[2], parts[3]];
    const file = index + 1 < fields.length ? fields[index + 1] : "";
    if (file) changes.push([file, sourceBlob, destinationBlob]);
    index += 2;
  }
  return changes;
}

/**
 * Report whether the raw diff wrote the empty object id.
 *
 * The empty id marks a file the commit created or deleted. It is all zeroes
 * at whatever width the caller asked for.
 */
export function isNullOid(oid) {
  return oid !== "" && /^0+$/.test(oid);
}

/**
 * Return the added and removed lines of each file the commit changed.
 *
 * One diff covers the whole commit. The path comes out of the `+++`/`---`
 * header, and a path the raw list does not hold is dropped rather than
 * guessed: git quotes an unusual path in that header.
 */
export function diffLines(repo, commit, knownPaths) {
  const out = git(
    repo,
    "-c",
    "core.quotepath=false",
    "diff",
    "--unified=0",
    "--no-renames",
    "--no-color",
    "--no-ext-diff",
    `${commit}^`,
    commit,
  );
  const wanted = new Set(knownPaths);
  const result = new Map();
  let current = null;
  for (const line of pySplitlines(out)) {
    if (line.startsWith("diff --git ")) {
      current = null;
      continue;
    }
    if (line.startsWith("--- ") || line.startsWith("+++ ")) {
      let candidate = line.slice(4);
      if (candidate === "/dev/null") continue;
      if (candidate.startsWith("a/") || candidate.startsWith("b/")) candidate = candidate.slice(2);
      if (wanted.has(candidate)) {
        current = candidate;
        if (!result.has(current)) result.set(current, [[], []]);
      }
      continue;
    }
    if (current === null) continue;
    if (line.startsWith("+++") || line.startsWith("---")) continue;
    if (line.startsWith("+")) result.get(current)[0].push(line.slice(1));
    else if (line.startsWith("-")) result.get(current)[1].push(line.slice(1));
  }
  return result;
}

/** Return the blob object id of every file in the tip tree. */
export function tipBlobs(repo, tip) {
  const out = git(repo, "ls-tree", "-r", "-z", tip);
  const blobs = new Map();
  for (const record of out.split("\0")) {
    if (!record) continue;
    const tab = record.indexOf("\t");
    const meta = tab === -1 ? record : record.slice(0, tab);
    const file = tab === -1 ? "" : record.slice(tab + 1);
    const parts = meta.split(" ");
    if (parts.length >= 3 && parts[1] === "blob") blobs.set(file, parts[2]);
  }
  return blobs;
}

export class Finding {
  constructor(verdict, pullRequest, commit, subject, file, missing, addedCount) {
    this.verdict = verdict;
    this.pullRequest = pullRequest;
    this.commit = commit;
    this.subject = subject;
    this.path = file;
    this.missing = missing;
    this.addedCount = addedCount;
    this.culpritCommit = null;
    this.culpritSubject = "";
    this.culpritPullRequest = null;
  }
}

function partition(text, separator) {
  const at = text.indexOf(separator);
  return at === -1 ? [text, ""] : [text.slice(0, at), text.slice(at + separator.length)];
}

/**
 * Name the commit that deleted the work.
 *
 * It walks forward from the pull request to the tip over the commits that
 * touched the file, and it stops at the first one that holds none of the
 * missing lines. That commit is the merge to fix.
 */
export async function findCulprit(repo, reader, finding, tip, preBlob) {
  const out = git(repo, "log", "--first-parent", "--reverse", "--format=%H%x1f%s", `${finding.commit}..${tip}`, "--", finding.path);
  for (const record of pySplitlines(out)) {
    if (!record) continue;
    const [sha, subject] = partition(record, "\x1f");
    let blob;
    try {
      blob = pyStrip(git(repo, "rev-parse", "--verify", `${sha}:${finding.path}`));
    } catch (error) {
      if (error instanceof GitError) continue;
      throw error;
    }
    let gone;
    if (blob === preBlob) {
      gone = true;
    } else {
      const content = await reader.text(blob);
      if (content === null) continue;
      gone = finding.missing.every((line) => !content.includes(line));
    }
    if (gone) {
      finding.culpritCommit = sha;
      finding.culpritSubject = subject;
      finding.culpritPullRequest = pullRequestNumber(subject);
      return;
    }
  }
}

/**
 * Report whether the file as it was BEFORE the pull request is back.
 *
 * It reads the pre-pull-request blob and looks for every measurable line of
 * it in the tree at the tip. A file the pull request created has no such
 * blob, and it cannot be reverted, so the answer is false.
 */
export async function oldTextIsBack(reader, sourceBlob, tipText) {
  if (!sourceBlob) return false;
  const old = await reader.text(sourceBlob);
  if (old === null) return false;
  const oldLines = pySplitlines(old).filter(measurable).map(pyStrip);
  if (!oldLines.length) return false;
  const back = oldLines.filter((line) => tipText.includes(line)).length;
  return back >= MIN_OLD_TEXT_BACK * oldLines.length;
}

export class AllowlistError extends Error {}

function allowKey(number, file) {
  return `${number}\0${file}`;
}

/** os.path.join(a, b) for two parts. */
function pyJoin(a, b) {
  if (b.startsWith("/")) return b;
  if (a === "" || a.endsWith("/")) return a + b;
  return `${a}/${b}`;
}

/**
 * Read the accepted (pull request, path) pairs, as a Map from key to the raw
 * line; each key is `<pull request>\0<path>`.
 *
 * A missing file means an empty allowlist. That is the normal state.
 */
export function loadAllowlist(file) {
  const entries = new Map();
  if (!fs.existsSync(file)) return entries;
  const text = fs.readFileSync(file, "utf8");
  // Python's text mode reads universal newlines, one line per record.
  const records = text.split(/\r\n|\r|\n/);
  if (records[records.length - 1] === "") records.pop();
  records.forEach((raw, i) => {
    const line = pyStrip(partition(raw, "#")[0]);
    if (!line) return;
    // line.split(None, 1): the first word, and the rest.
    const gap = line.search(SPLIT_WS);
    if (gap === -1) {
      throw new AllowlistError(`${file}:${i + 1}: expected '<pull request> <path>  # reason'`);
    }
    const first = line.slice(0, gap);
    const rest = pyStrip(line.slice(gap));
    entries.set(allowKey(first.replace(/^#+/, ""), rest), pyStrip(raw));
  });
  return entries;
}

/**
 * Measure the last `limit` merged pull requests below the tip.
 *
 * `seen` holds the (pull request, path) key of EVERY finding, including the
 * ones the allowlist silenced. The caller needs it to tell a silenced entry
 * from an entry that matches nothing.
 */
export async function audit(repo, tip, limit, allowlist) {
  const chain = pySplitlines(git(repo, "log", "--first-parent", "--format=%H%x1f%s", `-${limit}`, tip)).filter((line) => line);
  const reader = new BlobReader(repo);
  const faults = [];
  const rewrites = [];
  const auditedNumbers = [];
  const seen = new Set();
  try {
    const atTip = tipBlobs(repo, tip);
    for (const record of chain) {
      const [sha, subject] = partition(record, "\x1f");
      if (!gitOk(repo, "rev-parse", "--verify", "--quiet", `${sha}^`)) {
        continue; // the root commit has no base to compare against
      }
      const number = pullRequestNumber(subject);
      if (number) auditedNumbers.push(number);
      const changes = rawChanges(repo, sha);
      if (!changes.length) continue;
      const paths = changes.map(([file]) => file);
      const perFile = diffLines(repo, sha, paths);
      for (let [file, sourceBlob, destinationBlob] of changes) {
        if (!atTip.has(file)) continue; // renamed or deleted at the tip; see the header
        if (isNullOid(sourceBlob)) sourceBlob = "";
        if (isNullOid(destinationBlob)) continue; // the pull request deleted the file
        const lines = perFile.get(file);
        if (lines === undefined) continue; // binary, or a header this parser did not trust
        const added = lines[0].filter(measurable);
        if (!added.length) continue;
        const tipText = await reader.text(atTip.get(file));
        if (tipText === null) continue; // binary at the tip
        // Compare stripped text. Indentation moves for reasons that are not a
        // loss, and every later comparison uses this same form.
        const missing = added.map(pyStrip).filter((line) => !tipText.includes(line));
        if (!missing.length) continue;

        let restored = false;
        if (sourceBlob) {
          const out = git(repo, "log", "--first-parent", "--format=%H", `${sha}..${tip}`, "--", file);
          for (const later of pySplitlines(out)) {
            if (!later) continue;
            let blob;
            try {
              blob = pyStrip(git(repo, "rev-parse", "--verify", `${later}:${file}`));
            } catch (error) {
              if (error instanceof GitError) continue;
              throw error;
            }
            if (blob === sourceBlob) {
              restored = true;
              break;
            }
          }
        }

        let verdict;
        if (restored) {
          verdict = "restored";
        } else if (
          missing.length === added.length &&
          added.length >= MIN_ADDED_FOR_REVERT &&
          (await oldTextIsBack(reader, sourceBlob, tipText))
        ) {
          verdict = "reverted";
        } else if (missing.length === added.length) {
          verdict = "rewritten";
        } else {
          continue; // part of the work survives; an ordinary edit
        }

        const finding = new Finding(verdict, number, sha, subject, file, missing, added.length);
        const key = allowKey(pyStr(number), file);
        seen.add(key);
        if (verdict === "rewritten") {
          rewrites.push(finding);
          continue;
        }
        if (allowlist.has(key)) continue;
        await findCulprit(repo, reader, finding, tip, sourceBlob);
        faults.push(finding);
      }
    }
  } finally {
    await reader.close();
  }
  return { faults, rewrites, auditedNumbers, walked: chain.length, seen };
}

function report(print, finding) {
  const label = finding.pullRequest ? `PR #${finding.pullRequest}` : finding.commit.slice(0, 8);
  print(`  ${finding.verdict.toUpperCase()}  ${finding.path}`);
  print(`    added by   ${label} (${finding.commit.slice(0, 8)}) ${head(finding.subject, 80)}`);
  if (finding.culpritCommit) {
    const culprit = finding.culpritPullRequest ? `PR #${finding.culpritPullRequest}` : finding.culpritCommit.slice(0, 8);
    print(`    deleted by ${culprit} (${finding.culpritCommit.slice(0, 8)}) ${head(finding.culpritSubject, 80)}`);
  } else {
    print("    deleted by an unnamed commit; the file changed outside the chain");
  }
  print(`    ${finding.missing.length} of ${finding.addedCount} measurable added line(s) are gone, for example:`);
  for (const line of finding.missing.slice(0, 3)) {
    print(`      ${head(pyStrip(line), 110)}`);
  }
}

// ── the command line ─────────────────────────────────────────────────────

const USAGE =
  "usage: check-landed-line-survival.mjs [-h] [--repo REPO] [--tip TIP] [--limit LIMIT]\n" +
  "                                      [--allowlist ALLOWLIST] [--allow-shallow]\n" +
  "                                      [--report-only]";

const HELP = `${USAGE}

${DESCRIPTION}

options:
  -h, --help            show this help message and exit
  --repo REPO           the repository to read
  --tip TIP             the commit whose tree holds the answer. Pass the BASE
                        branch head on a pull request, so the tree under review
                        stays out of the measurement.
  --limit LIMIT         how many merged pull requests to read
  --allowlist ALLOWLIST
  --allow-shallow       read a shallow clone. The window is then whatever the
                        clone holds.
  --report-only         print the findings and exit 0. Use it to survey
                        history.`;

class UsageError extends Error {}

const VALUE_OPTIONS = { "--repo": "repo", "--tip": "tip", "--limit": "limit", "--allowlist": "allowlist" };
const FLAG_OPTIONS = { "--allow-shallow": "allowShallow", "--report-only": "reportOnly" };

/** argparse's reading of this command line, including unique-prefix options. */
export function parseArgs(argv) {
  const args = {
    repo: ".",
    tip: "HEAD",
    limit: DEFAULT_LIMIT,
    allowlist: DEFAULT_ALLOWLIST,
    allowShallow: false,
    reportOnly: false,
    help: false,
  };
  const all = [...Object.keys(VALUE_OPTIONS), ...Object.keys(FLAG_OPTIONS), "--help"];
  for (let i = 0; i < argv.length; i += 1) {
    const word = argv[i];
    if (word === "-h") {
      args.help = true;
      continue;
    }
    if (!word.startsWith("--") || word === "--") {
      throw new UsageError(`unrecognized arguments: ${argv.slice(i).join(" ")}`);
    }
    const [flag, inline] = word.includes("=") ? [word.slice(0, word.indexOf("=")), word.slice(word.indexOf("=") + 1)] : [word, undefined];
    let name = all.includes(flag) ? flag : null;
    if (!name) {
      const matches = all.filter((option) => option.startsWith(flag));
      if (matches.length > 1) {
        throw new UsageError(`ambiguous option: ${flag} could match ${matches.join(", ")}`);
      }
      if (matches.length === 0) throw new UsageError(`unrecognized arguments: ${word}`);
      name = matches[0];
    }
    if (name === "--help") {
      args.help = true;
      continue;
    }
    if (name in FLAG_OPTIONS) {
      if (inline !== undefined) throw new UsageError(`argument ${name}: ignored explicit argument '${inline}'`);
      args[FLAG_OPTIONS[name]] = true;
      continue;
    }
    let value = inline;
    if (value === undefined) {
      if (i + 1 >= argv.length || (argv[i + 1].startsWith("-") && !/^-\d/.test(argv[i + 1]))) {
        throw new UsageError(`argument ${name}: expected one argument`);
      }
      i += 1;
      value = argv[i];
    }
    if (name === "--limit") {
      if (!/^[\s]*[+-]?\d+(?:_\d+)*[\s]*$/.test(value)) {
        throw new UsageError(`argument --limit: invalid int value: '${value}'`);
      }
      args.limit = Number.parseInt(value.trim().replace(/_/g, ""), 10);
    } else {
      args[VALUE_OPTIONS[name]] = value;
    }
  }
  return args;
}

export async function main(argv = process.argv.slice(2), out = (line) => process.stdout.write(`${line}\n`)) {
  const print = (line = "") => out(line);
  let args;
  try {
    args = parseArgs(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    process.stderr.write(`${USAGE}\ncheck-landed-line-survival.mjs: error: ${error.message}\n`);
    return 2;
  }
  if (args.help) {
    print(HELP);
    return 0;
  }

  const repo = args.repo;
  let tip;
  try {
    tip = pyStrip(git(repo, "rev-parse", "--verify", `${args.tip}^{commit}`));
  } catch (error) {
    if (!(error instanceof GitError)) throw error;
    print(`::error::cannot resolve --tip ${args.tip}: ${error.message}`);
    return 1;
  }

  if (pyStrip(git(repo, "rev-parse", "--is-shallow-repository")) === "true") {
    if (!args.allowShallow) {
      print(
        "::error::the clone is shallow, so the trunk this check walks is not " +
          "present. Check out with fetch-depth: 0, or pass --allow-shallow.",
      );
      return 1;
    }
  }

  let allowlist;
  try {
    allowlist = loadAllowlist(pyJoin(repo, args.allowlist));
  } catch (error) {
    if (!(error instanceof AllowlistError)) throw error;
    process.stderr.write(`${error.message}\n`);
    return 1;
  }
  const { faults, rewrites, auditedNumbers, walked, seen } = await audit(repo, tip, args.limit, allowlist);

  print(`tip            ${tip.slice(0, 8)} ${head(pyStrip(git(repo, "log", "-1", "--format=%s", tip)), 70)}`);
  print(`audited        ${walked} trunk commit(s), ${auditedNumbers.length} of them name a pull request`);

  // A window of zero measures nothing and would pass every run. Fail instead.
  if (walked === 0) {
    print("::error::the walk found no commit below the tip, so this check measured nothing");
    return 1;
  }
  if (!auditedNumbers.length) {
    print(
      "::error::no audited commit names a pull request, so no merge can be " +
        "named. The subject format changed, and this check measured nothing.",
    );
    return 1;
  }

  // An allowlist entry rots. An entry whose pull request is still inside the
  // window, and which now matches nothing, states a removal that is no longer
  // there, so it is an error. An entry whose pull request left the window is
  // silent: the walk cannot see it any more, and that is not the entry's
  // fault.
  const stale = [];
  const audited = new Set(auditedNumbers);
  for (const [key, raw] of allowlist) {
    if (audited.has(key.slice(0, key.indexOf("\0"))) && !seen.has(key)) stale.push(raw);
  }

  if (rewrites.length) {
    print();
    print(`rewritten (reported, not a fault): ${rewrites.length} file(s)`);
    for (const finding of rewrites.slice(0, 20)) {
      print(`  ${finding.path}  added by PR #${pyStr(finding.pullRequest)}, ${finding.missing.length} line(s) gone`);
    }
    if (rewrites.length > 20) print(`  ... and ${rewrites.length - 20} more`);
  }

  if (faults.length) {
    print();
    print(`LOST WORK: ${faults.length} file(s)`);
    for (const finding of faults) report(print, finding);
    print();
    print(
      `::error::${faults.length} file(s) lost the work of a merged pull request. A squash ` +
        "from a stale branch writes whole files, so the merge named above " +
        "deleted a fix that had already landed. Restore the work, then re-merge. " +
        `Record an accepted removal in ${args.allowlist} as '<pull request> <path>  # reason'.`,
    );
  }

  if (stale.length) {
    print();
    print(
      `::error::${stale.length} allowlist entry/entries match nothing while their pull ` +
        `request is inside the window: ${stale.join("; ")}`,
    );
  }

  if (args.reportOnly) {
    print();
    print("--report-only: exit 0");
    return 0;
  }
  if (faults.length || stale.length) return 1;
  print("every measurable line the audited pull requests added is still in the tree");
  return 0;
}

function invokedDirectly() {
  if (!process.argv[1]) return false;
  try {
    return fs.realpathSync(process.argv[1]) === fs.realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (invokedDirectly()) {
  main().then(
    (code) => {
      process.exitCode = code;
    },
    (error) => {
      process.stderr.write(`${error && error.stack ? error.stack : error}\n`);
      process.exitCode = 1;
    },
  );
}
