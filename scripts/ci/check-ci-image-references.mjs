#!/usr/bin/env node
// The CI-only images stay CI-only.
//
// ci-web-e2e.yml builds each stack image once per commit and pushes it to
// `ghcr.io/elitea-ng/ci-<image>:<sha>`, and the layer cache lives at
// `ghcr.io/elitea-ng/ci-cache-<image>`. Those are built from pull-request code,
// pruned after 7 days (ci-image-cleanup.yml) and never scanned or signed, so
// nothing that ships may name them: not the Helm chart, not a compose file a
// deployment copies. publish.yml, docker-bake.hcl and helm-install-smoke.yml
// may name the LAYER CACHE (`ci-cache-`) and nothing else: a cache ref decides
// how fast a build runs, never what it runs. (The argocd
// deployment lives in a separate repository; it pins the RELEASE images, which
// publish.yml writes under their own names.)
//
// This reads every tracked file and fails on any `ghcr.io/<org>/ci-` reference
// outside the files that build, pull, cache or prune them. It also fails when
// it finds fewer references in those files than the floor below: a reference
// spelled some new way would otherwise make this check pass by seeing nothing.
//
// Run with the runner's node; only the standard library.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { pyRepr, splitlines } from "./py-compat.mjs";

const SELF = fileURLToPath(import.meta.url);
const REPOSITORY_ROOT = path.resolve(path.dirname(SELF), "..", "..");

// `ghcr.io/<org>/ci-` — split so this file does not match itself.
const PATTERN = "ghcr\\.io/[A-Za-z0-9_.-]+/" + "ci-";
const CACHE_PATTERN = "ghcr\\.io/[A-Za-z0-9_.-]+/" + "ci-" + "cache-";

export const ALLOWED = {
  ".github/workflows/ci-web-e2e.yml": "builds and pulls them",
  ".github/workflows/ci-image-cleanup.yml": "prunes them",
  ".github/actions/build-stack-images/action.yml": "pushes them (push mode) and documents the cache",
  ".github/actions/pull-stack-images/action.yml": "pulls them",
  "deploy/ci/bake-cache.hcl": "the CI layer-cache refs",
  "deploy/ci/bake-cache-rust.hcl": "the CI layer-cache ref for the Rust worker",
  "scripts/ci/stack-images.test.mjs": "fixtures",
  "scripts/ci/prune-ci-images.test.mjs": "fixtures",
  "scripts/ci/check-ci-image-references.mjs": "this check's own description",
};

// Files that may name the layer cache, and only the layer cache.
export const CACHE_ONLY = {
  ".github/workflows/publish.yml": "the release's registry layer cache",
  ".github/workflows/helm-install-smoke.yml": "reads the E2E stacks' layer cache",
  "docker-bake.hcl": "reads the release's and the E2E stacks' layer cache",
};

// References the allowed files must hold between them (measured when this was
// written: ci-web-e2e.yml's push prefix, the 14 + 1 cache refs in the HCL).
export const MINIMUM_ALLOWED_REFERENCES = 15;

const count = (pattern, line) => (line.match(new RegExp(pattern, "g")) || []).length;

export function trackedFiles(root) {
  const out = execFileSync("git", ["ls-files", "-z"], { cwd: root, maxBuffer: 1 << 30 });
  return out.toString("utf8").split("\0").filter(Boolean);
}

function readStrictUtf8(file) {
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(fs.readFileSync(file));
  } catch {
    return null; // not UTF-8, a directory (a submodule), or gone
  }
}

export function scan(root, files) {
  const violations = [];
  let allowedHits = 0;
  const single = new RegExp(PATTERN);
  for (const name of files) {
    const text = readStrictUtf8(path.join(root, name));
    if (text === null) continue;
    splitlines(text).forEach((line, index) => {
      if (!single.test(line)) return;
      if (Object.hasOwn(ALLOWED, name)) {
        allowedHits += 1;
      } else if (Object.hasOwn(CACHE_ONLY, name) && count(PATTERN, line) === count(CACHE_PATTERN, line)) {
        allowedHits += 1;
      } else {
        violations.push(`${name}:${index + 1}: ${line.trim()}`);
      }
    });
  }
  return [violations, allowedHits];
}

export function main(root = REPOSITORY_ROOT, log = console.log) {
  const files = trackedFiles(root);
  const tracked = new Set(files);
  const missing = [...Object.keys(ALLOWED), ...Object.keys(CACHE_ONLY)].filter((name) => !tracked.has(name));
  if (missing.length) {
    log(`::error::allowed file(s) no longer tracked: ${pyRepr(missing)}; update ALLOWED in ${SELF}`);
    return 1;
  }
  const [violations, allowedHits] = scan(root, files);
  for (const violation of violations) {
    log(`::error::a CI-only image is referenced outside CI: ${violation}`);
  }
  if (violations.length) return 1;
  if (allowedHits < MINIMUM_ALLOWED_REFERENCES) {
    log(
      `::error::found ${allowedHits} CI-only image reference(s) in the files that use them, expected at least ` +
        `${MINIMUM_ALLOWED_REFERENCES} — the pattern stopped matching, so this check saw nothing`,
    );
    return 1;
  }
  log(
    `CI-only images: ${allowedHits} reference(s), all in the ${Object.keys(ALLOWED).length + Object.keys(CACHE_ONLY).length} files that build, pull, cache or prune them`,
  );
  return 0;
}

if (process.argv[1] && fs.realpathSync(process.argv[1]) === SELF) {
  process.exitCode = main();
}
