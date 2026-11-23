#!/usr/bin/env node
// Trim what a Rust cache entry saves to what a later build reads, before it is saved.
//
//     node scripts/ci/trim-cargo-cache.mjs --lock <Cargo.lock>... [--target <target dir>...]
//
// Run as the last step of a job that saves a cache on main (a rust-cache post
// step, or an actions/cache/save after it). Two trims:
//
// 1. CARGO'S GIT DATABASE (measured 2026-11-23). libs/rust/code-parsers pins two
//    tree-sitter grammars by git rev (tree-sitter-c-sharp, tree-sitter-cpp: no
//    crates.io release has the grammar the language pack builds), and the
//    worker, the engines and the desktop host link it. Cargo clones a git
//    dependency's whole history to read one commit: 144 MB and 96 MB of packs,
//    a generated parser.c of 20+ MB per commit. On a pool pod the clone took
//    4m10s of every cold libs/rust job (job 137376497651), and the 240 MB rode
//    in every cache entry that holds ~/.cargo/git/db. For every repository
//    under $CARGO_HOME/git/db, the commits a `source = "git+...#<commit>"` line
//    of the lockfiles names become its shallow boundary (git's own `shallow`
//    file), every other ref goes, and the repository is repacked and pruned:
//    tree-sitter-cpp went from 96 MB to 1.8 MB. HEAD stays unborn, as in the
//    database cargo creates (`git init --bare`, then a fetch into
//    refs/commit/<sha>); pointing it at refs/commit/<sha> broke cargo's
//    checkout ("the remote's default branch does not fit the refspec
//    configuration"). Cargo reads a locked commit
//    from the database when it is there and fetches only one it cannot find:
//    checked 2026-11-23 with cargo itself (libgit2) on the trimmed cpp
//    database, a deleted checkout directory and `cargo fetch --locked` then
//    `cargo build`: no network fetch, the checkout rebuilt from the database.
//    A commit that is not there is fetched into it as before, and cargo
//    re-initialises a database it cannot fetch into. A repository that holds
//    none of the commits is stale and removed.
//
// 2. BUILD-SCRIPT OBJECT FILES. A `-sys` crate's build script compiles C into
//    OUT_DIR (`<target>/<profile>/build/<pkg>-<hash>/out`) and archives the
//    objects into the static library it links (`cargo:rustc-link-lib=static`).
//    The `.o` files are left beside it and never read again: cargo does not
//    rerun a build script whose inputs did not change, and one that does rerun
//    compiles them anew. aws-lc-sys's CMake tree, sqlite, zstd and the twelve
//    tree-sitter grammars leave theirs in every target directory. Host builds
//    only, which is what the two cache writers build: a `--target <triple>`
//    build's outputs (`<target>/<triple>/<profile>/build`) are not walked.
//
// A trim that fails warns and leaves what it could not trim as it was: the
// entry is then larger, never wrong.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { isEntrypoint } from "./py-compat.mjs";

/** The pinned commits of a lockfile's git sources. */
export function pinnedCommits(lockText) {
  const out = new Set();
  for (const m of lockText.matchAll(/^source = "git\+[^"#]+#([0-9a-f]{40})"$/gm)) out.add(m[1]);
  return out;
}

/** {locks, targets} from the command line. */
export function parseArgs(argv) {
  const out = { locks: [], targets: [] };
  let into = null;
  for (const a of argv) {
    if (a === "--lock") into = out.locks;
    else if (a === "--target") into = out.targets;
    else if (into) into.push(a);
    else throw new Error(`unexpected argument ${a}: name it after --lock or --target`);
  }
  return out;
}

const git = (dir, args) => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] });

function sizeOf(p) {
  const st = fs.lstatSync(p);
  if (!st.isDirectory()) return st.isFile() ? st.size : 0;
  return fs.readdirSync(p).reduce((s, n) => s + sizeOf(path.join(p, n)), 0);
}
const mib = (b) => `${(b / 2 ** 20).toFixed(1)} MiB`;

/** Make one cargo git database hold only the given commits. */
export function trimRepo(dir, commits, { log = console.log } = {}) {
  const held = [...commits].filter((c) => {
    try {
      git(dir, ["cat-file", "-e", `${c}^{commit}`]);
      return true;
    } catch {
      return false;
    }
  });
  const before = sizeOf(dir);
  if (held.length === 0) {
    fs.rmSync(dir, { recursive: true, force: true });
    log(`git db ${path.basename(dir)}: holds no locked commit, removed (${mib(before)})`);
    return before;
  }
  const keep = new Set(held);
  for (const line of git(dir, ["for-each-ref", "--format=%(objectname) %(refname)"]).split("\n").filter(Boolean)) {
    const [sha, ref] = line.split(" ");
    if (!keep.has(sha)) git(dir, ["update-ref", "-d", ref]);
  }
  // Every kept commit has a ref, so the repack keeps it whatever ref cargo gave it.
  for (const c of held) git(dir, ["update-ref", `refs/commit/${c}`, c]);
  fs.writeFileSync(path.join(dir, "shallow"), `${held.sort().join("\n")}\n`);
  git(dir, ["reflog", "expire", "--expire=now", "--all"]);
  git(dir, ["repack", "-a", "-d", "-q"]);
  git(dir, ["prune", "--expire=now"]);
  const after = sizeOf(dir);
  log(`git db ${path.basename(dir)}: ${held.length} locked commit(s), ${mib(before)} -> ${mib(after)}`);
  return before - after;
}

/** Delete the `.o` files under every `<target>/<profile>/build/<pkg>/out`. */
export function trimBuildObjects(target, { log = console.log } = {}) {
  let freed = 0;
  let files = 0;
  const walk = (dir) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.isFile() && e.name.endsWith(".o")) {
        freed += fs.statSync(p).size;
        files += 1;
        fs.rmSync(p);
      }
    }
  };
  if (!fs.existsSync(target)) {
    log(`${target}: no target directory`);
    return 0;
  }
  for (const profile of fs.readdirSync(target, { withFileTypes: true })) {
    if (!profile.isDirectory()) continue;
    const build = path.join(target, profile.name, "build");
    if (!fs.existsSync(build)) continue;
    for (const pkg of fs.readdirSync(build, { withFileTypes: true })) {
      const out = path.join(build, pkg.name, "out");
      if (pkg.isDirectory() && fs.existsSync(out)) walk(out);
    }
  }
  log(`${target}: ${files} build-script object file(s), ${mib(freed)} removed`);
  return freed;
}

export function main({ argv = process.argv.slice(2), env = process.env, log = console.log, warn = console.warn } = {}) {
  let args;
  try {
    args = parseArgs(argv);
  } catch (error) {
    warn(error.message);
    return 2;
  }
  if (args.locks.length === 0 && args.targets.length === 0) {
    warn("usage: trim-cargo-cache.mjs --lock <Cargo.lock>... [--target <dir>...]");
    return 2;
  }
  let freed = 0;
  if (args.locks.length) {
    const commits = new Set();
    for (const f of args.locks) for (const c of pinnedCommits(fs.readFileSync(f, "utf8"))) commits.add(c);
    const db = path.join(env.CARGO_HOME || path.join(os.homedir(), ".cargo"), "git", "db");
    if (!fs.existsSync(db)) log(`${db}: no git database`);
    else {
      for (const name of fs.readdirSync(db)) {
        const dir = path.join(db, name);
        if (!fs.statSync(dir).isDirectory()) continue;
        try {
          freed += trimRepo(dir, commits, { log });
        } catch (error) {
          warn(`::warning::git db ${name}: not trimmed (${String(error.message).split("\n")[0]}); saved as it is`);
        }
      }
    }
  }
  for (const t of args.targets) {
    try {
      freed += trimBuildObjects(t, { log });
    } catch (error) {
      warn(`::warning::${t}: build objects not trimmed (${error.message}); saved as they are`);
    }
  }
  log(`trimmed ${mib(freed)} before the cache save`);
  return 0;
}

if (isEntrypoint(import.meta.url)) process.exit(main());
