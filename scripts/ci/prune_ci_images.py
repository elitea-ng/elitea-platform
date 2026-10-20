#!/usr/bin/env python3
"""Prune the CI-only `ci-*` container packages of the organisation.

    prune_ci_images.py --org elitea-ng --days 7 [--dry-run]

ci-web-e2e.yml pushes every stack image it builds to `ci-<image>:<sha>` (one
version per commit) and, on a push to main, the layer cache to
`ci-cache-<image>:linux-amd64` (the tag moves; every move strands the previous
manifest as an untagged version). Nothing that ships references them
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


def gh_api(path: str, method: str = "GET") -> list | dict | None:
    args = ["gh", "api", "-X", method, "-H", "Accept: application/vnd.github+json", path]
    if method == "GET":
        args.insert(2, "--paginate")
        args += ["--jq", ".[]"]
    out = subprocess.run(args, check=True, stdout=subprocess.PIPE, text=True).stdout
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
    packages = gh_api(f"/orgs/{org}/packages?package_type=container&per_page=100")
    ci = sorted(p["name"] for p in packages if is_ci_package(p["name"]))
    print(f"{len(packages)} container package(s) in {args.org}; {len(ci)} are CI-only ({PREFIX}*): {', '.join(ci) or 'none'}")
    deleted = 0
    for name in ci:
        quoted = urllib.parse.quote(name, safe="")
        versions = gh_api(f"/orgs/{org}/packages/container/{quoted}/versions?per_page=100")
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
