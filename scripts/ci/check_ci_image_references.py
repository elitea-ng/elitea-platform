#!/usr/bin/env python3
"""The CI-only images stay CI-only.

ci-web-e2e.yml builds each stack image once per commit and pushes it to
`ghcr.io/elitea-ng/ci-<image>:<sha>`, and the layer cache lives at
`ghcr.io/elitea-ng/ci-cache-<image>`. Those are built from pull-request code,
pruned after 7 days (ci-image-cleanup.yml) and never scanned or signed, so
nothing that ships may name them: not the Helm chart, not a compose file a
deployment copies. publish.yml, docker-bake.hcl and helm-install-smoke.yml
may name the LAYER CACHE (`ci-cache-`) and nothing else: a cache ref decides
how fast a build runs, never what it runs. (The argocd
deployment lives in a separate repository; it pins the RELEASE images, which
publish.yml writes under their own names.)

This reads every tracked file and fails on any `ghcr.io/<org>/ci-` reference
outside the files that build, pull, cache or prune them. It also fails when
it finds fewer references in those files than the floor below: a reference
spelled some new way would otherwise make this check pass by seeing nothing.
"""

from __future__ import annotations

import pathlib
import re
import subprocess
import sys

REPOSITORY_ROOT = pathlib.Path(__file__).resolve().parents[2]

# `ghcr.io/<org>/ci-` — split so this file does not match itself.
PATTERN = re.compile(r"ghcr\.io/[A-Za-z0-9_.-]+/" + "ci-")

ALLOWED = {
    ".github/workflows/ci-web-e2e.yml": "builds and pulls them",
    ".github/workflows/ci-image-cleanup.yml": "prunes them",
    ".github/actions/build-stack-images/action.yml": "pushes them (push mode) and documents the cache",
    ".github/actions/pull-stack-images/action.yml": "pulls them",
    "deploy/ci/bake-cache.hcl": "the CI layer-cache refs",
    "deploy/ci/bake-cache-rust.hcl": "the CI layer-cache ref for the Rust worker",
    "scripts/ci/test_stack_images.py": "fixtures",
    "scripts/ci/test_prune_ci_images.py": "fixtures",
    "scripts/ci/check_ci_image_references.py": "this check's own description",
}

# Files that may name the layer cache, and only the layer cache.
CACHE_ONLY = {
    ".github/workflows/publish.yml": "the release's registry layer cache",
    ".github/workflows/helm-install-smoke.yml": "reads the E2E stacks' layer cache",
    "docker-bake.hcl": "reads the release's and the E2E stacks' layer cache",
}
CACHE_PATTERN = re.compile(r"ghcr\.io/[A-Za-z0-9_.-]+/" + "ci-" + "cache-")

# References the allowed files must hold between them (measured when this was
# written: ci-web-e2e.yml's push prefix, the 14 + 1 cache refs in the HCL).
MINIMUM_ALLOWED_REFERENCES = 15


def tracked_files(root: pathlib.Path) -> list[str]:
    out = subprocess.run(["git", "ls-files", "-z"], cwd=root, check=True, stdout=subprocess.PIPE).stdout
    return [name for name in out.decode().split("\0") if name]


def scan(root: pathlib.Path, files: list[str]) -> tuple[list[str], int]:
    violations: list[str] = []
    allowed_hits = 0
    for name in files:
        path = root / name
        try:
            text = path.read_text(errors="strict")
        except (UnicodeDecodeError, IsADirectoryError, FileNotFoundError):
            continue
        for number, line in enumerate(text.splitlines(), 1):
            if not PATTERN.search(line):
                continue
            if name in ALLOWED:
                allowed_hits += 1
            elif name in CACHE_ONLY and len(PATTERN.findall(line)) == len(CACHE_PATTERN.findall(line)):
                allowed_hits += 1
            else:
                violations.append(f"{name}:{number}: {line.strip()}")
    return violations, allowed_hits


def main() -> int:
    files = tracked_files(REPOSITORY_ROOT)
    missing = [name for name in [*ALLOWED, *CACHE_ONLY] if name not in files]
    if missing:
        print(f"::error::allowed file(s) no longer tracked: {missing}; update ALLOWED in {__file__}")
        return 1
    violations, allowed_hits = scan(REPOSITORY_ROOT, files)
    for violation in violations:
        print(f"::error::a CI-only image is referenced outside CI: {violation}")
    if violations:
        return 1
    if allowed_hits < MINIMUM_ALLOWED_REFERENCES:
        print(
            f"::error::found {allowed_hits} CI-only image reference(s) in the files that use them, expected at least "
            f"{MINIMUM_ALLOWED_REFERENCES} — the pattern stopped matching, so this check saw nothing"
        )
        return 1
    print(f"CI-only images: {allowed_hits} reference(s), all in the {len(ALLOWED) + len(CACHE_ONLY)} files that build, pull, cache or prune them")
    return 0


if __name__ == "__main__":
    sys.exit(main())
