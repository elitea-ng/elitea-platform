/*
Assertions on check-landed-line-survival.mjs.

Issue #541. The defect the checker exists for is not hypothetical. Pull request
#502 landed a logout correction. Pull request #516 merged four hours later from
a branch that was cut before #502, and its squash wrote the five files back to
their pre-#502 copies. The check that was run was
`git merge-base --is-ancestor`, and it answered "true", because #502 was still
in the history. The content was gone.

Every scenario below builds a throwaway BARE repository with git plumbing
(`hash-object`, `update-index`, `write-tree`, `commit-tree`). No scenario
touches the repository that holds this file, and none of them needs a work
tree.

Scenario 1 is the regression test. It reproduces that exact shape and asserts
BOTH halves: `--is-ancestor` still answers "true" on it, and the checker still
answers "lost".

Run it with:  node --test scripts/ci/check-landed-line-survival.test.mjs
*/

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const HERE = path.dirname(fileURLToPath(import.meta.url));
// The path is overridable so a proof run can point the same assertions at an
// earlier copy of the checker and observe the red run.
const CHECKER = process.env.LANDED_LINE_SURVIVAL_CHECKER || path.join(HERE, "check-landed-line-survival.mjs");

const GIT_ENV = {
  GIT_AUTHOR_NAME: "Fixture",
  GIT_AUTHOR_EMAIL: "fixture@example.invalid",
  GIT_COMMITTER_NAME: "Fixture",
  GIT_COMMITTER_EMAIL: "fixture@example.invalid",
  GIT_AUTHOR_DATE: "2026-08-17T21:53:00+0000",
  GIT_COMMITTER_DATE: "2026-08-17T21:53:00+0000",
};

// The text #502 put into logout.ts, shortened. Every line is long enough and
// carries enough letters and digits to be measurable.
const BEFORE_502 = `export async function logout(): Promise<void> {
  window.localStorage.removeItem('el.auth.state');
  await fetch('/api/v2/auth/logout', { method: 'POST' });
  window.location.assign('/login');
}
`;

const AFTER_502 = `let loggingOut = false;

export function isLoggingOut(): boolean {
  return loggingOut;
}

export async function logout(): Promise<void> {
  loggingOut = true;
  window.localStorage.removeItem('el.auth.flight.started');
  await fetch('/api/v2/auth/logout', { method: 'POST' });
  window.location.assign('/login');
}
`;

const UNRELATED = `export const budgetCeiling = 1000;
export const budgetWindowDays = 30;
export function overBudget(spend: number): boolean {
  return spend > budgetCeiling;
}
`;

/** Run a command; stdout and stderr together, as the Python suite read them. */
function run(args, { input } = {}) {
  const proc = spawnSync(args[0], args.slice(1), {
    env: { ...process.env, ...GIT_ENV },
    input,
    encoding: "utf8",
    maxBuffer: 1 << 28,
  });
  if (proc.error) throw proc.error;
  return { returncode: proc.status, stdout: `${proc.stdout}${proc.stderr}` };
}

/** A bare repository built one commit at a time. */
class Fixture {
  constructor(root) {
    this.path = path.join(root, "repo.git");
    this.root = root;
    const check = run(["git", "init", "--quiet", "--bare", this.path]);
    assert.equal(check.returncode, 0, check.stdout);
    run(["git", "-C", this.path, "symbolic-ref", "HEAD", "refs/heads/main"]);
    this.head = null;
    this.counter = 0;
  }

  git(args, { index, input } = {}) {
    const env = { ...process.env, ...GIT_ENV };
    if (index) env.GIT_INDEX_FILE = index;
    const proc = spawnSync("git", ["-C", this.path, ...args], { env, input, encoding: "utf8" });
    assert.equal(proc.status, 0, `git ${args.join(" ")}: ${proc.stderr}`);
    return proc.stdout;
  }

  /** Write one commit whose tree is exactly `files` (path -> text). */
  commit(files, subject) {
    this.counter += 1;
    const index = path.join(this.root, `index-${this.counter}`);
    for (const file of Object.keys(files).sort()) {
      const oid = this.git(["hash-object", "-w", "--stdin"], { input: files[file] }).trim();
      this.git(["update-index", "--add", "--cacheinfo", `100644,${oid},${file}`], { index });
    }
    const tree = this.git(["write-tree"], { index }).trim();
    const args = ["commit-tree", tree, "-m", subject];
    if (this.head) args.push("-p", this.head);
    const sha = this.git(args).trim();
    this.git(["update-ref", "refs/heads/main", sha]);
    this.head = sha;
    return sha;
  }

  branch(name, sha) {
    this.git(["update-ref", `refs/heads/${name}`, sha]);
  }

  check(...extra) {
    return run([process.execPath, CHECKER, "--repo", this.path, ...extra]);
  }
}

/** setUp/tearDown: a fresh fixture in a temporary directory per scenario. */
function scenario(body) {
  return () => {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "line-survival-"));
    try {
      return body(new Fixture(root), root);
    } finally {
      fs.rmSync(root, { recursive: true, force: true });
    }
  };
}

function staleSquashHistory(repo) {
  repo.commit({ "src/logout.ts": BEFORE_502 }, "chore: base (#400)");
  repo.commit({ "src/logout.ts": AFTER_502 }, "fix(web): the logout must not start a flight (#502)");
  repo.commit({ "src/logout.ts": BEFORE_502 }, "feat(budgets): add usage dimensions (#516)");
}

// -- scenario 1: the regression test ---------------------------------

// The #502 shape. Ancestry says yes, and the content is gone.
test("a stale squash that restores the old file is a fault", scenario((repo) => {
  const base = repo.commit({ "src/logout.ts": BEFORE_502, "README.md": "elitea\n" }, "chore: base (#400)");
  const landed = repo.commit(
    { "src/logout.ts": AFTER_502, "README.md": "elitea\n" },
    "fix(web): the logout must not start a flight (#502)",
  );
  // The stale squash writes the whole file back to its pre-#502 copy.
  repo.commit(
    { "src/logout.ts": BEFORE_502, "README.md": "elitea\n", "src/budget.ts": UNRELATED },
    "feat(budgets): add usage dimensions (#516)",
  );
  repo.commit(
    {
      "src/logout.ts": BEFORE_502,
      "README.md": "elitea\n",
      "src/budget.ts": UNRELATED,
      "docs/notes.md": "a later unrelated pull request\n",
    },
    "fix(gateway): clear an outage-owned row (#530)",
  );
  assert.notEqual(base, landed);

  // The check that was run in the issue. It passes, and it is not
  // evidence: the blob it reports on is gone.
  const ancestry = run(["git", "-C", repo.path, "merge-base", "--is-ancestor", landed, "HEAD"]);
  assert.equal(
    ancestry.returncode,
    0,
    "the fixture must reproduce the false negative: --is-ancestor has to " +
      "answer 'true' for the pull request whose work was deleted",
  );

  const result = repo.check("--limit", "10");
  assert.equal(result.returncode, 1, result.stdout);
  assert.ok(result.stdout.includes("RESTORED"), result.stdout);
  assert.ok(result.stdout.includes("src/logout.ts"), result.stdout);
  // It must name BOTH pull requests, and the second one is the point.
  assert.ok(result.stdout.includes("added by   PR #502"), result.stdout);
  assert.ok(result.stdout.includes("deleted by PR #516"), result.stdout);
  assert.ok(result.stdout.includes("isLoggingOut"), result.stdout);
}));

// -- scenario 2: the text comes back without a byte-exact blob -------

// The stale branch also edited the file, so no blob matches. The exact-blob
// test then answers "no" at every commit, and the old text is back all the
// same. Ask for the text.
test("a revert that no exact blob records is still a fault", scenario((repo) => {
  repo.commit({ "src/logout.ts": BEFORE_502 }, "chore: base (#400)");
  repo.commit({ "src/logout.ts": AFTER_502 }, "fix(web): the logout must not start a flight (#502)");
  const staleCopy = BEFORE_502 + "\nexport const authModuleVersion = 'v2.1.0';\n";
  repo.commit({ "src/logout.ts": staleCopy }, "feat(budgets): add usage dimensions (#516)");
  repo.commit({ "src/logout.ts": staleCopy, "docs/notes.md": "a later change\n" }, "chore: an unrelated pull request (#540)");
  const result = repo.check("--limit", "10");
  assert.equal(result.returncode, 1, result.stdout);
  assert.ok(result.stdout.includes("REVERTED"), result.stdout);
  assert.ok(result.stdout.includes("added by   PR #502"), result.stdout);
  assert.ok(result.stdout.includes("deleted by PR #516"), result.stdout);
}));

// -- scenario 3: an ordinary rewrite is not a fault ------------------

test("an ordinary rewrite is reported and is not a fault", scenario((repo) => {
  repo.commit({ "src/logout.ts": BEFORE_502 }, "chore: base (#400)");
  repo.commit({ "src/logout.ts": AFTER_502 }, "fix(web): the logout must not start a flight (#502)");
  // Every line #502 added is gone, and the text it removed did NOT come
  // back. This is a rewrite, and a gate that failed on it would fail on
  // most pull requests.
  const rewritten = `export async function logout(): Promise<void> {
  await signOutThroughTheProvider({ returnTo: '/login' });
}
`;
  repo.commit({ "src/logout.ts": rewritten }, "refactor(web): move the logout into the provider (#560)");
  const result = repo.check("--limit", "10");
  assert.equal(result.returncode, 0, result.stdout);
  assert.ok(result.stdout.includes("rewritten (reported, not a fault)"), result.stdout);
  assert.ok(!result.stdout.includes("LOST WORK"), result.stdout);
}));

test("history that kept its work is silent", scenario((repo) => {
  repo.commit({ "src/logout.ts": BEFORE_502 }, "chore: base (#400)");
  repo.commit({ "src/logout.ts": AFTER_502 }, "fix(web): the logout must not start a flight (#502)");
  repo.commit({ "src/logout.ts": AFTER_502, "src/budget.ts": UNRELATED }, "feat(budgets): add usage dimensions (#516)");
  const result = repo.check("--limit", "10");
  assert.equal(result.returncode, 0, result.stdout);
  assert.ok(result.stdout.includes("still in the tree"), result.stdout);
}));

// -- scenario 4: it cannot fail the pull request that runs it --------

// The property that keeps this gate off the pull request that adds it. A pull
// request may legitimately rewrite lines an earlier pull request added. The
// workflow passes the BASE branch head as the tip, so the proposed tree is
// never read.
test("the tree under review stays out of the measurement", scenario((repo) => {
  repo.commit({ "src/logout.ts": BEFORE_502 }, "chore: base (#400)");
  repo.commit({ "src/logout.ts": AFTER_502 }, "fix(web): the logout must not start a flight (#502)");
  const baseHead = repo.commit(
    { "src/logout.ts": AFTER_502, "src/budget.ts": UNRELATED },
    "feat(budgets): add usage dimensions (#516)",
  );
  // The pull request under review rewrites the file wholesale, the way a
  // large branch does.
  const proposed = repo.commit(
    { "src/logout.ts": BEFORE_502, "src/budget.ts": UNRELATED },
    "feat: a large branch that rewrites the auth module (#541)",
  );
  repo.branch("pr", proposed);

  const withBaseTip = repo.check("--limit", "10", "--tip", baseHead);
  assert.equal(withBaseTip.returncode, 0, withBaseTip.stdout);
  assert.ok(withBaseTip.stdout.includes("still in the tree"), withBaseTip.stdout);

  // Reading the proposed tree instead WOULD be red. The tip is the whole
  // difference, so the workflow must keep passing the base.
  const withProposedTip = repo.check("--limit", "10", "--tip", proposed);
  assert.equal(withProposedTip.returncode, 1, withProposedTip.stdout);
}));

// -- scenario 5: a window that measures nothing is red ---------------

test("an empty window is red rather than green", scenario((repo) => {
  repo.commit({ "src/logout.ts": BEFORE_502 }, "chore: base (#400)");
  repo.commit({ "src/logout.ts": AFTER_502 }, "fix(web): a fix (#502)");
  const result = repo.check("--limit", "0");
  assert.equal(result.returncode, 1, result.stdout);
  assert.ok(result.stdout.includes("measured nothing"), result.stdout);
}));

test("subjects without a pull request number are red", scenario((repo) => {
  repo.commit({ "src/logout.ts": BEFORE_502 }, "chore: base");
  repo.commit({ "src/logout.ts": AFTER_502 }, "fix the logout");
  const result = repo.check("--limit", "10");
  assert.equal(result.returncode, 1, result.stdout);
  assert.ok(result.stdout.includes("no audited commit names a pull request"), result.stdout);
}));

test("a shallow clone is red rather than a short walk", scenario((repo) => {
  const head = repo.commit({ "src/logout.ts": BEFORE_502 }, "chore: base (#400)");
  repo.commit({ "src/logout.ts": AFTER_502 }, "fix(web): a fix (#502)");
  // A shallow clone is exactly this file plus a grafted boundary. The
  // walk would then read whatever the clone happens to hold.
  fs.writeFileSync(path.join(repo.path, "shallow"), `${head}\n`);
  const result = repo.check("--limit", "10");
  assert.equal(result.returncode, 1, result.stdout);
  assert.ok(result.stdout.includes("clone is shallow"), result.stdout);
}));

// -- scenario 6: the allowlist, and its own rot ----------------------

test("an allowlist entry accepts a named removal", scenario((repo, root) => {
  staleSquashHistory(repo);
  const allow = path.join(root, "allow.txt");
  fs.writeFileSync(allow, "502 src/logout.ts  # accepted: the flight moved\n");
  const result = repo.check("--limit", "10", "--allowlist", allow);
  assert.equal(result.returncode, 0, result.stdout);
}));

test("an allowlist entry that matches nothing is red", scenario((repo, root) => {
  staleSquashHistory(repo);
  const allow = path.join(root, "allow.txt");
  fs.writeFileSync(allow, "502 src/logout.ts  # accepted\n516 src/nowhere.ts  # nothing here any more\n");
  const result = repo.check("--limit", "10", "--allowlist", allow);
  assert.equal(result.returncode, 1, result.stdout);
  assert.ok(result.stdout.includes("match nothing"), result.stdout);
}));

test("an allowlist entry outside the window is ignored", scenario((repo, root) => {
  staleSquashHistory(repo);
  const allow = path.join(root, "allow.txt");
  fs.writeFileSync(allow, "502 src/logout.ts  # accepted\n11 src/ancient.ts  # merged long before the window\n");
  const result = repo.check("--limit", "10", "--allowlist", allow);
  assert.equal(result.returncode, 0, result.stdout);
}));

// -- scenario 7: a file the tip does not hold is not measured --------

test("a deleted path is not reported as lost work", scenario((repo) => {
  repo.commit({ "src/logout.ts": BEFORE_502 }, "chore: base (#400)");
  repo.commit(
    { "src/logout.ts": AFTER_502, "src/legacy.ts": UNRELATED },
    "fix(web): the logout must not start a flight (#502)",
  );
  // A deliberate deletion. The checker states it cannot tell one from a
  // rename, so it must stay quiet about the path.
  repo.commit({ "src/logout.ts": AFTER_502 }, "chore(web): drop the legacy module (#560)");
  const result = repo.check("--limit", "10");
  assert.equal(result.returncode, 0, result.stdout);
  assert.ok(!result.stdout.includes("src/legacy.ts"), result.stdout);
}));

// -- scenario 8: the command line ------------------------------------

test("the command line takes --x v and --x=v and refuses what it does not know", scenario((repo) => {
  staleSquashHistory(repo);
  const surveyed = repo.check("--limit=10", "--report-only");
  assert.equal(surveyed.returncode, 0, surveyed.stdout);
  assert.ok(surveyed.stdout.includes("LOST WORK"), surveyed.stdout);
  assert.ok(surveyed.stdout.includes("--report-only: exit 0"), surveyed.stdout);
  for (const extra of [["--limit", "x"], ["--bogus"], ["--allow-shallow=yes"], ["--tip"], ["stray"]]) {
    const result = repo.check(...extra);
    assert.equal(result.returncode, 2, `${extra.join(" ")}: ${result.stdout}`);
    assert.ok(result.stdout.startsWith("usage: check-landed-line-survival.mjs"), result.stdout);
    assert.ok(result.stdout.includes("check-landed-line-survival.mjs: error: "), result.stdout);
  }
  const help = repo.check("-h");
  assert.equal(help.returncode, 0, help.stdout);
  assert.ok(help.stdout.includes("--report-only         print the findings"), help.stdout);
}));
