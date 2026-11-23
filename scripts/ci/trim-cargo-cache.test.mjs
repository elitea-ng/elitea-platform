import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";

import { main, parseArgs, pinnedCommits, trimBuildObjects, trimRepo } from "./trim-cargo-cache.mjs";

const tmp = () => fs.mkdtempSync(path.join(os.tmpdir(), "trim-cargo-cache-"));
const quiet = { log: () => {} };
const run = (dir, ...args) => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: { ...process.env, GIT_AUTHOR_NAME: "t", GIT_AUTHOR_EMAIL: "t@t", GIT_COMMITTER_NAME: "t", GIT_COMMITTER_EMAIL: "t@t" } }).trim();

/** A bare repository like cargo's database: two commits, a big blob only in the first. */
function database() {
  const work = tmp();
  run(work, "init", "-q");
  fs.writeFileSync(path.join(work, "parser.c"), "x".repeat(200_000) + Math.random());
  run(work, "add", ".");
  run(work, "commit", "-q", "-m", "old");
  const old = run(work, "rev-parse", "HEAD");
  const oldBlob = run(work, "rev-parse", "HEAD:parser.c");
  fs.writeFileSync(path.join(work, "parser.c"), "new");
  run(work, "commit", "-q", "-am", "pinned");
  const pinned = run(work, "rev-parse", "HEAD");
  const db = path.join(tmp(), "tree-sitter-x-0123456789abcdef");
  execFileSync("git", ["clone", "-q", "--bare", work, db]);
  return { db, old, oldBlob, pinned };
}
const has = (db, obj) => {
  try {
    run(db, "cat-file", "-e", obj);
    return true;
  } catch {
    return false;
  }
};

test("the lockfile's git sources give their locked commits, nothing else", () => {
  const c = "8b5b49eb196bec7040441bee33b2c9a4838d6967";
  const lock = `[[package]]\nname = "tree-sitter-cpp"\nsource = "git+https://github.com/tree-sitter/tree-sitter-cpp?rev=${c}#${c}"\n\n[[package]]\nname = "serde"\nsource = "registry+https://github.com/rust-lang/crates.io-index"\nchecksum = "${"a".repeat(64)}"\n`;
  assert.deepEqual([...pinnedCommits(lock)], [c]);
});

test("the arguments name lockfiles and target directories, and nothing unnamed", () => {
  assert.deepEqual(parseArgs(["--lock", "a/Cargo.lock", "b/Cargo.lock", "--target", "t"]), { locks: ["a/Cargo.lock", "b/Cargo.lock"], targets: ["t"] });
  assert.throws(() => parseArgs(["a/Cargo.lock"]), /--lock or --target/);
});

test("a database keeps its locked commit and its tree, and loses the history before it", () => {
  const { db, old, oldBlob, pinned } = database();
  trimRepo(db, new Set([pinned]), quiet);
  assert.ok(has(db, `${pinned}^{tree}`), "the locked commit's tree stays");
  assert.ok(!has(db, oldBlob), "the older commit's blob is pruned");
  assert.ok(!has(db, old), "the older commit is pruned");
  assert.equal(fs.readFileSync(path.join(db, "shallow"), "utf8"), `${pinned}\n`);
  assert.equal(run(db, "rev-parse", `refs/commit/${pinned}`), pinned);
  // A clone of it, as cargo makes its checkout, reads the locked commit.
  const checkout = path.join(tmp(), "checkout");
  execFileSync("git", ["clone", "-q", `file://${db}`, checkout]);
  run(checkout, "checkout", "-q", pinned);
  assert.equal(fs.readFileSync(path.join(checkout, "parser.c"), "utf8"), "new");
});

test("a database that holds no locked commit is stale and removed", () => {
  const { db } = database();
  trimRepo(db, new Set(["f".repeat(40)]), quiet);
  assert.ok(!fs.existsSync(db));
});

test("build-script objects go, the static library and every other output stay", () => {
  const target = tmp();
  const out = path.join(target, "debug", "build", "aws-lc-sys-0a1b", "out");
  fs.mkdirSync(path.join(out, "build", "CMakeFiles"), { recursive: true });
  fs.writeFileSync(path.join(out, "build", "CMakeFiles", "a.c.o"), "obj");
  fs.writeFileSync(path.join(out, "b.o"), "obj");
  fs.writeFileSync(path.join(out, "libaws_lc.a"), "lib");
  const deps = path.join(target, "debug", "deps");
  fs.mkdirSync(deps, { recursive: true });
  fs.writeFileSync(path.join(deps, "keep.o"), "not a build-script output");
  assert.equal(trimBuildObjects(target, quiet), 6);
  assert.ok(fs.existsSync(path.join(out, "libaws_lc.a")));
  assert.ok(!fs.existsSync(path.join(out, "b.o")));
  assert.ok(!fs.existsSync(path.join(out, "build", "CMakeFiles", "a.c.o")));
  assert.ok(fs.existsSync(path.join(deps, "keep.o")));
});

test("main trims every database under CARGO_HOME against the lockfiles, and a missing target is no failure", () => {
  const { db, pinned } = database();
  const home = tmp();
  fs.mkdirSync(path.join(home, "git", "db"), { recursive: true });
  fs.renameSync(db, path.join(home, "git", "db", path.basename(db)));
  const lock = path.join(tmp(), "Cargo.lock");
  fs.writeFileSync(lock, `source = "git+https://example.invalid/x?rev=${pinned}#${pinned}"\n`);
  const lines = [];
  const code = main({ argv: ["--lock", lock, "--target", path.join(home, "none")], env: { CARGO_HOME: home }, log: (l) => lines.push(l), warn: (l) => lines.push(l) });
  assert.equal(code, 0);
  assert.match(lines.join("\n"), /1 locked commit\(s\)/);
  assert.match(lines.join("\n"), /no target directory/);
  assert.equal(main({ argv: [], warn: () => {} }), 2);
});
