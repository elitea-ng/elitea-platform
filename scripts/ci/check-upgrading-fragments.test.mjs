import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { MIGRATED_THROUGH, checkDiff, checkPointer, checkTree, isDate, main, parseFragment, readTree } from "./check-upgrading-fragments.mjs";

const TODAY = "2026-11-09";
const fragment = ({ title = '"A change (#42)"', issue = "42", date = "2026-11-09", extra = [], body = "Do this.\n" } = {}) =>
  ["---", ...(title === null ? [] : [`title: ${title}`]), ...(issue === null ? [] : [`issue: ${issue}`]), ...(date === null ? [] : [`date: ${date}`]), ...extra, "---", "", body].join("\n");
const problemsOf = (name, source) => parseFragment(name, source, { today: TODAY }).problems;
const POINTER_TEXT = "# Upgrading\n\nThe entries are in [`docs/upgrading/`](upgrading/), one file per change.\n";

test("the repository's own upgrade notes and pointer pass", () => {
  const root = path.resolve(import.meta.dirname, "..", "..");
  const result = checkTree(readTree(root), { today: "9999-12-31" });
  assert.deepEqual(result.problems, []);
  assert.deepEqual(checkPointer(fs.readFileSync(path.join(root, "docs/UPGRADING.md"), "utf8")), []);
});

test("a well-formed fragment passes, with and without adr", () => {
  assert.deepEqual(problemsOf("0042-a-change.md", fragment()), []);
  assert.deepEqual(problemsOf("0042-a-change.md", fragment({ extra: ["adr: ADR-0050"] })), []);
  assert.deepEqual(problemsOf("1833-x.md", fragment({ issue: "1833" })), []);
});

test("a bad file name is refused", () => {
  for (const name of ["42-a-change.md", "0042_a_change.md", "0042-A-Change.md", "0042-a--change.md", "0042-.md", "0042-a-change.mdx", "a-change.md"]) {
    assert.equal(problemsOf(name, fragment()).length, 1, name);
  }
});

test("missing front matter is refused", () => {
  assert.match(problemsOf("0042-a.md", "Do this.\n")[0], /no front matter/);
});

test("each required key is required", () => {
  assert.match(problemsOf("0042-a.md", fragment({ title: null })).join(), /no title/);
  assert.match(problemsOf("0042-a.md", fragment({ issue: null })).join(), /no issue/);
  assert.match(problemsOf("0042-a.md", fragment({ date: null })).join(), /no date/);
});

test("an unquoted, empty or padded title is refused", () => {
  assert.match(problemsOf("0042-a.md", fragment({ title: "A change" })).join(), /double quotes/);
  assert.match(problemsOf("0042-a.md", fragment({ title: "'A change'" })).join(), /double quotes/);
  assert.match(problemsOf("0042-a.md", fragment({ title: '""' })).join(), /empty/);
  assert.match(problemsOf("0042-a.md", fragment({ title: '" A"' })).join(), /outer spaces/);
});

test("an issue that is not the file's number is refused", () => {
  assert.match(problemsOf("0042-a.md", fragment({ issue: "43" })).join(), /issue must be 42/);
  assert.match(problemsOf("0042-a.md", fragment({ issue: "042" })).join(), /positive whole number/);
  assert.match(problemsOf("0042-a.md", fragment({ issue: "abc" })).join(), /positive whole number/);
});

test("an invalid or future date is refused", () => {
  for (const date of ["2026-13-01", "2026-02-30", "26-11-09", "2026/11/09", "yesterday"]) {
    assert.match(problemsOf("0042-a.md", fragment({ date })).join(), /not a YYYY-MM-DD day/, date);
  }
  assert.match(problemsOf("0042-a.md", fragment({ date: "2026-11-10" })).join(), /after today/);
  assert.ok(isDate("2028-02-29") && !isDate("2027-02-29"));
});

test("a bad adr, an unknown key or a repeated key is refused", () => {
  assert.match(problemsOf("0042-a.md", fragment({ extra: ["adr: 50"] })).join(), /ADR-NNNN/);
  assert.match(problemsOf("0042-a.md", fragment({ extra: ["author: me"] })).join(), /unknown front matter key "author"/);
  assert.match(problemsOf("0042-a.md", fragment({ extra: ["adr: ADR-0050", "adr: ADR-0051"] })).join(), /set twice/);
  assert.match(problemsOf("0042-a.md", fragment({ extra: ["nonsense"] })).join(), /is not "key: value"/);
});

test("order is refused on an entry dated after the migration", () => {
  assert.deepEqual(problemsOf("0042-a.md", fragment({ date: MIGRATED_THROUGH, extra: ["order: 3"] })), []);
  assert.match(problemsOf("0042-a.md", fragment({ date: TODAY, extra: ["order: 3"] })).join(), /does not set "order"/);
  assert.match(problemsOf("0042-a.md", fragment({ date: MIGRATED_THROUGH, extra: ["order: 0"] })).join(), /positive whole number/);
});

test("an empty body, or a # or ## heading outside a fence, is refused", () => {
  assert.match(problemsOf("0042-a.md", fragment({ body: "\n" })).join(), /body is empty/);
  assert.match(problemsOf("0042-a.md", fragment({ body: "## Another entry\n\nText.\n" })).join(), /use ### in the body/);
  assert.match(problemsOf("0042-a.md", fragment({ body: "# Title\n" })).join(), /use ### in the body/);
  assert.deepEqual(problemsOf("0042-a.md", fragment({ body: "### Sub\n\n```bash\n## a shell comment\n```\n" })), []);
});

test("two fragments with the same number and slug are refused (a padded number is the same number)", () => {
  const files = new Map([
    ["0042-a.md", fragment()],
    ["00042-a.md", fragment({ title: '"Other"' })],
  ]);
  const { problems } = checkTree(files, { today: TODAY, minimum: 0 });
  assert.match(problems.join("\n"), /0042-a\.md: the number and slug "42-a" is also in docs\/upgrading\/00042-a\.md/);
  assert.match(problems.join("\n"), /00042-a\.md: the file name starts with 00042/);
});

test("a repeated title or sub-heading is refused (one page, one id each)", () => {
  const files = new Map([
    ["0042-a.md", fragment({ body: "### Steps\n\nx\n" })],
    ["0043-b.md", fragment({ issue: "43", title: '"a change (#42)"', body: "### steps\n\nx\n" })],
  ]);
  const { problems } = checkTree(files, { today: TODAY, minimum: 0 });
  assert.equal(problems.length, 2);
  assert.match(problems.join(), /the title "a change \(#42\)"/);
  assert.match(problems.join(), /the sub-heading "steps"/);
});

test("the fragment floor stops a reader that finds nothing", () => {
  assert.match(checkTree(new Map(), { today: TODAY, minimum: 1 }).problems[0], /expected at least 1/);
});

test("the pointer refuses an entry, length and a missing link", () => {
  assert.deepEqual(checkPointer(POINTER_TEXT), []);
  assert.match(checkPointer(`${POINTER_TEXT}\n## New entry (#42)\n\nText.\n`).join(), /has the entry/);
  assert.match(checkPointer(POINTER_TEXT + "x\n".repeat(40)).join(), /fixed pointer/);
  assert.match(checkPointer("# Upgrading\n").join(), /does not point/);
});

test("the diff: adding fragments and editing the README pass", () => {
  const changes = [
    { status: "A", path: "docs/upgrading/0042-a.md" },
    { status: "M", path: "docs/upgrading/README.md" },
    { status: "M", path: "docs/other.md" },
  ];
  assert.deepEqual(checkDiff(changes), []);
});

test("the diff: modifying or deleting a fragment from the base is refused", () => {
  const problems = checkDiff([
    { status: "M", path: "docs/upgrading/0042-a.md" },
    { status: "D", path: "docs/upgrading/0043-b.md" },
    { status: "T", path: "docs/upgrading/0044-c.md" },
  ]);
  assert.equal(problems.length, 3);
  assert.match(problems[0], /modifies an upgrade note that is already on the base branch/);
  assert.match(problems[1], /deletes an upgrade note/);
});

test("the diff: the upgrade-note-edit label allows a correction", () => {
  assert.deepEqual(checkDiff([{ status: "M", path: "docs/upgrading/0042-a.md" }], { editAllowed: true }), []);
});

test("the diff: a directory under docs/upgrading is refused", () => {
  assert.match(checkDiff([{ status: "A", path: "docs/upgrading/sub/0042-a.md" }])[0], /files only/);
});

// End to end: a throwaway repository with a base branch and a pull request
// branch, read the way the workflow reads it (--base <base>).
function repo() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "upgrading-fragments-"));
  const git = (...args) => execFileSync("git", args, { cwd: root, stdio: ["ignore", "pipe", "pipe"] });
  const write = (rel, text) => {
    fs.mkdirSync(path.dirname(path.join(root, rel)), { recursive: true });
    fs.writeFileSync(path.join(root, rel), text);
  };
  git("init", "-q", "-b", "main");
  git("config", "user.email", "t@example.com");
  git("config", "user.name", "t");
  git("config", "commit.gpgsign", "false");
  write("docs/UPGRADING.md", POINTER_TEXT);
  write("docs/upgrading/README.md", "# Upgrade notes\n");
  write("docs/upgrading/0042-a.md", fragment());
  git("add", "-A");
  git("commit", "-qm", "base");
  git("checkout", "-qb", "pr");
  const commit = () => {
    git("add", "-A");
    git("commit", "-qm", "pr", "--allow-empty");
  };
  const run = (env = {}) => {
    const lines = [];
    const log = { log: (l) => lines.push(l), error: (l) => lines.push(l) };
    const status = main(["--base", "main"], { root, env, today: TODAY, minimum: 1, log });
    return { status, out: lines.join("\n") };
  };
  return { root, write, commit, run, rm: (rel) => fs.rmSync(path.join(root, rel)) };
}

test("end to end: a pull request that adds a fragment passes", () => {
  const r = repo();
  r.write("docs/upgrading/0043-b.md", fragment({ issue: "43", title: '"B (#43)"' }));
  r.commit();
  const { status, out } = r.run();
  assert.equal(status, 0, out);
  assert.match(out, /OK: 2 upgrade notes, 1 changed file/);
});

test("end to end: a pull request may edit the fragment it added", () => {
  const r = repo();
  r.write("docs/upgrading/0043-b.md", fragment({ issue: "43", title: '"B (#43)"' }));
  r.commit();
  r.write("docs/upgrading/0043-b.md", fragment({ issue: "43", title: '"B (#43)"', body: "Changed.\n" }));
  r.commit();
  assert.equal(r.run().status, 0);
});

test("end to end: editing or deleting a base fragment fails, the label lets an edit through", () => {
  const r = repo();
  r.write("docs/upgrading/0042-a.md", fragment({ body: "Rewritten.\n" }));
  r.commit();
  const edited = r.run();
  assert.equal(edited.status, 1);
  assert.match(edited.out, /0042-a\.md: this change modifies/);
  assert.equal(r.run({ UPGRADE_NOTE_EDIT_ALLOWED: "true" }).status, 0);
  r.rm("docs/upgrading/0042-a.md");
  r.write("docs/upgrading/0099-z.md", fragment({ issue: "99" }));
  r.commit();
  assert.match(r.run().out, /0042-a\.md: this change deletes/);
});

test("end to end: a rename of a base fragment is a deletion", () => {
  const r = repo();
  r.rm("docs/upgrading/0042-a.md");
  r.write("docs/upgrading/0042-renamed.md", fragment());
  r.commit();
  const { status, out } = r.run();
  assert.equal(status, 1);
  assert.match(out, /0042-a\.md: this change deletes/);
});

test("end to end: an entry prepended to docs/UPGRADING.md fails", () => {
  const r = repo();
  r.write("docs/UPGRADING.md", `${POINTER_TEXT}\n## My change (#43)\n\nText.\n`);
  r.commit();
  const { status, out } = r.run();
  assert.equal(status, 1);
  assert.match(out, /has the entry "## My change \(#43\)"/);
});

test("end to end: a base the check cannot read stops it", () => {
  const r = repo();
  const lines = [];
  const status = main(["--base", "no-such-branch"], { root: r.root, today: TODAY, minimum: 1, log: { log: (l) => lines.push(l), error: (l) => lines.push(l) } });
  assert.equal(status, 2);
  assert.match(lines.join(), /cannot find the merge base/);
});
