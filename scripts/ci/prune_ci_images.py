#!/usr/bin/env python3
"""Prune the CI-only `ci-*` container packages of the organisation.

    prune_ci_images.py --org elitea-ng --days 7 [--dry-run]

WHICH PACKAGES. Not an org listing: `GET /orgs/{org}/packages` needs an
org-level read:packages token, and the job's GITHUB_TOKEN cannot enumerate
the organisation's packages (measured: the first run of this workflow
failed there). The names come from the image list itself instead — every
`ci-cache-<image>` ref in deploy/ci/*.hcl (the one-ref-per-image list the
stack images are built from) and in docker-bake.hcl (the release images,
whose builds share the same cache packages under `release-<platform>`) — giving `ci-<image>` and `ci-cache-<image>`
for each. Those packages are linked to this repository (each push labels
`org.opencontainers.image.source`), so the token may read and delete their
versions. A package that does not exist yet (a cache main has not written)
is skipped.

ci-web-e2e.yml pushes every stack image it builds to `ci-<image>:<sha>` (one
version per commit) and, on a push to main, the layer cache to
`ci-cache-<image>:linux-amd64` (the tag moves; every move strands the previous
manifest as an untagged version); publish.yml does the same with
`ci-cache-<image>:release-<platform>`. Nothing that ships references them
(scripts/ci/check_ci_image_references.py), so they are disposable:

  ci-cache-*   delete UNTAGGED versions older than --days. The tagged version
               is the live cache every build reads; it is kept however old.
  ci-*         delete every version older than --days, tagged or not. A lane
               re-run after that fails its pull, loudly, and a fresh push of
               the commit (or a re-run of the whole workflow) rebuilds.

Never touched: any package whose name does not start with `ci-` — the
release images (elitea-main, elitea-worker-rust, ...), the buildkit mirror,
the charts. The rule is checked twice, at selection and again immediately
before each DELETE. A package's last remaining version is never deleted (the
registry refuses it, and an empty package would lose its visibility setting).

Age is `updated_at`: a version re-pushed under the same digest is fresh.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import pathlib
import re
import subprocess
import sys
import urllib.parse

PREFIX = "ci-"
CACHE_PREFIX = "ci-cache-"


def is_ci_package(name: str) -> bool:
    return name.startswith(PREFIX)


def select(name: str, versions: list[dict], now: dt.datetime, days: int) -> list[dict]:
    """The versions of package `name` to delete under the policy above."""
    if not is_ci_package(name):
        return []
    cutoff = now - dt.timedelta(days=days)
    doomed = []
    for version in versions:
        updated = dt.datetime.fromisoformat(version["updated_at"].replace("Z", "+00:00"))
        if updated >= cutoff:
            continue
        tags = ((version.get("metadata") or {}).get("container") or {}).get("tags") or []
        if name.startswith(CACHE_PREFIX) and tags:
            continue
        doomed.append(version)
    if len(doomed) >= len(versions):
        # Keep the newest: never empty a package.
        newest = max(versions, key=lambda v: v["updated_at"])
        doomed = [v for v in doomed if v["id"] != newest["id"]]
    return doomed


CACHE_REF = re.compile(r"ghcr\.io/[A-Za-z0-9_.-]+/" + CACHE_PREFIX + r"([A-Za-z0-9_.-]+):")


def ci_packages(hcl_texts: list[str]) -> list[str]:
    """`ci-<image>` and `ci-cache-<image>` for every cache ref in the HCL."""
    images = sorted({m.group(1) for text in hcl_texts for m in CACHE_REF.finditer(text)})
    return sorted([PREFIX + image for image in images] + [CACHE_PREFIX + image for image in images])


def gh_api(path: str, method: str = "GET") -> list | dict | None:
    args = ["gh", "api", "-X", method, "-H", "Accept: application/vnd.github+json", path]
    if method == "GET":
        args.insert(2, "--paginate")
        args += ["--jq", ".[]"]
    result = subprocess.run(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    if result.returncode != 0:
        if "404" in result.stderr or "Not Found" in result.stderr:
            return None
        raise SystemExit(f"gh api {method} {path} failed: {result.stderr.strip()}")
    out = result.stdout
    if method != "GET":
        return None
    return [json.loads(line) for line in out.splitlines() if line.strip()]


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--org", required=True)
    parser.add_argument("--days", type=int, default=7)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args(argv)
    if args.days < 1:
        print("::error::--days must be at least 1")
        return 2

    now = dt.datetime.now(dt.timezone.utc)
    org = urllib.parse.quote(args.org)
    root = pathlib.Path(__file__).resolve().parents[2]
    sources = [*sorted((root / "deploy" / "ci").glob("*.hcl")), root / "docker-bake.hcl"]
    ci = ci_packages([p.read_text() for p in sources])
    if len(ci) < 20:
        print(f"::error::derived {len(ci)} CI package name(s) from deploy/ci/*.hcl and docker-bake.hcl, expected at least 20 — the ref pattern stopped matching")
        return 1
    print(f"{len(ci)} CI-only package name(s) from deploy/ci/*.hcl and docker-bake.hcl: {', '.join(ci)}")
    deleted = 0
    for name in ci:
        quoted = urllib.parse.quote(name, safe="")
        versions = gh_api(f"/orgs/{org}/packages/container/{quoted}/versions?per_page=100")
        if versions is None:
            print(f"{name}: no such package yet; skipped")
            continue
        doomed = select(name, versions, now, args.days)
        print(f"{name}: {len(versions)} version(s), {len(doomed)} older than {args.days} day(s) to delete")
        for version in doomed:
            if not is_ci_package(name):  # the rule, again, at the point of no return
                raise SystemExit(f"refusing to delete from non-CI package {name}")
            tags = ((version.get("metadata") or {}).get("container") or {}).get("tags") or []
            label = f"{name}@{version['name'][:19]} tags={tags} updated={version['updated_at']}"
            if args.dry_run:
                print(f"  would delete {label}")
                continue
            gh_api(f"/orgs/{org}/packages/container/{quoted}/versions/{version['id']}", method="DELETE")
            print(f"  deleted {label}")
            deleted += 1
    print(f"{'dry run: nothing deleted' if args.dry_run else f'{deleted} version(s) deleted'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
