#!/usr/bin/env node
// A content address for a CI image: a hash of everything that goes INTO it.
//
// ci-web-e2e.yml's build-once jobs used to rebuild every stack image on every
// push, although most pushes change one service. They now tag each image they
// push `ci-<image>:cas-<hash>` too, and skip the build when that tag already
// exists (.github/actions/build-stack-images, `reuse`). This computes <hash>.
//
// WHAT GOES INTO IT. The image's build spec (context, Containerfile, target,
// build args), and for the stages the target runs, every COPY/ADD source: the
// content (git blob ids) of each tracked file under it, the Containerfile and
// the `.dockerignore` beside it. That is the input set BuildKit's own layer
// cache keys on, read the way scripts/ci/check-e2e-paths.mjs reads it for the
// path filter. Two commits with the same hash build the same image.
//
// WHAT IT DOES NOT COVER, and why that is acceptable. A floating base-image tag
// (`FROM debian:13`) can move between two builds; ci-* images live seven days
// (ci-image-cleanup.yml) and a reused image is re-tagged for the run, so a
// moved base is at most a week stale in a CI image that ships nothing. Every
// Containerfile in the stack pins its Rust and Go bases by digest. A COPY
// source that is generated (untracked) is not hashed; none of the build-once
// jobs generates one before the build.
//
// Reads the repository with `git ls-files`, so it needs the checkout. The
// `yaml` package is needed only through check-e2e-paths.mjs (npm ci --prefix
// scripts/ci; .github/actions/ci-scripts-deps).
//
//   node scripts/ci/stack-image-hash.mjs <workspace> <context> <dockerfile> <target> [<build-arg>=<value>...]
//   node scripts/ci/stack-image-hash.mjs plan <workspace> <plan.json>
//
// The first form prints the hash of one build; `context` and `dockerfile` are
// relative to <workspace> (the dockerfile to the context, as compose has it).
// The second adds `hash` and `cas` (the tag) to each entry of a stack-images.mjs
// plan, in place, and prints one `<ref>:<cas>` line per entry.

import { spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";

import { containerfileInputs } from "./check-e2e-paths.mjs";
import { isEntrypoint } from "./py-compat.mjs";

export const HASH_VERSION = "cas-v1";
export const HASH_LENGTH = 24;

const sha256 = (text) => crypto.createHash("sha256").update(text).digest("hex");

/** `git ls-files -s` lines for a path: mode, blob id and path of each tracked file under it. */
function trackedBlobs(workspace, repoPath, run) {
  const result = run("git", ["ls-files", "-s", "-z", "--", repoPath], { cwd: workspace, encoding: "utf8", maxBuffer: 1 << 28 });
  if (result.status !== 0) throw new Error(`git ls-files ${repoPath} failed: ${result.stderr}`);
  return String(result.stdout).split("\0").filter(Boolean).map((line) => line.replace(/\s0\t/, " "));
}

/**
 * @param {{workspace: string, context: string, dockerfile?: string, target?: string, args?: Record<string,string>, run?: Function}} spec
 */
export function contentHash({ workspace, context, dockerfile = "Dockerfile", target = "", args = {}, run = spawnSync }) {
  const absContext = path.resolve(workspace, context);
  const absDockerfile = path.resolve(absContext, dockerfile);
  const inputs = new Set(containerfileInputs(absContext, absDockerfile, target || undefined));
  for (const ignore of [path.join(absContext, ".dockerignore"), `${absDockerfile}.dockerignore`]) {
    if (fs.existsSync(ignore)) inputs.add(path.relative(workspace, ignore));
  }
  const lines = [];
  for (const input of [...inputs].sort()) {
    const blobs = trackedBlobs(workspace, input, run);
    // An input that no tracked file matches is part of the hash as "absent",
    // so adding the file later changes it.
    lines.push(`${input}: ${blobs.length ? sha256(blobs.sort().join("\n")) : "absent"}`);
  }
  const spec = { v: HASH_VERSION, context: path.relative(workspace, absContext) || ".", dockerfile, target, args: Object.fromEntries(Object.entries(args).sort()), inputs: lines };
  return sha256(JSON.stringify(spec)).slice(0, HASH_LENGTH);
}

/** Adds `hash` and `cas` to every entry of a stack-images plan. */
export function addHashes(entries, workspace, run = spawnSync) {
  return entries.map((entry) => {
    const hash = contentHash({
      workspace,
      context: entry.build.context,
      dockerfile: entry.build.dockerfile,
      target: entry.build.target,
      args: entry.build.args,
      run,
    });
    return { ...entry, hash, cas: `cas-${hash}` };
  });
}

export function main(argv = process.argv.slice(2), out = (s) => process.stdout.write(s), err = (s) => process.stderr.write(s)) {
  try {
    if (argv[0] === "plan" && argv.length === 3) {
      const [, workspace, planFile] = argv;
      const entries = addHashes(JSON.parse(fs.readFileSync(planFile, "utf8")), workspace);
      fs.writeFileSync(planFile, JSON.stringify(entries, null, 2));
      for (const e of entries) out(`${e.ref}:${e.cas}\n`);
      return 0;
    }
    if (argv.length >= 4) {
      const [workspace, context, dockerfile, target, ...args] = argv;
      const parsed = Object.fromEntries(args.map((a) => [a.slice(0, a.indexOf("=")), a.slice(a.indexOf("=") + 1)]));
      out(`${contentHash({ workspace, context, dockerfile, target, args: parsed })}\n`);
      return 0;
    }
    err("usage: stack-image-hash.mjs <workspace> <context> <dockerfile> <target> [arg=value...] | plan <workspace> <plan.json>\n");
    return 2;
  } catch (error) {
    err(`::error::${error.message}\n`);
    return 1;
  }
}

if (isEntrypoint(import.meta.url)) process.exitCode = main();
