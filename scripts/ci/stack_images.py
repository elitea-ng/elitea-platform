#!/usr/bin/env python3
"""Build once, pull everywhere: the stack-image digest map for ci-web-e2e.yml.

ci-web-e2e.yml builds each stack image ONCE per commit (its `*-images` jobs,
through .github/actions/build-stack-images in push mode) and every journey
lane pulls the result by digest (.github/actions/pull-stack-images). This
script is the contract between the two halves, so neither re-derives it:

  plan     <config.json> <workspace> <prefix> <tag> <plan-out> <service>...
           For each compose SERVICE to build: the CI-only image name
           (<prefix><image>, <image> the last path segment of the service's
           `image:`), its build spec, and a bake `--set <service>.tags=...`
           line on stdout.
  record   <plan.json> <bake-metadata.json>
           The digest map fragment for one build job, on stdout:
           {<image>: {"ref": ..., "digest": "sha256:...", "build": {...}}}.
  resolve  <config.json> <workspace> <digests-dir>
           For a lane's compose config: one `<local tag>\\t<ref>@<digest>`
           line per image compose would build. Fails, naming the image, when
           the map has no entry for it or the entry was built from a
           DIFFERENT spec (context, Containerfile, target, args) — a lane
           whose compose names a variant nobody built must not quietly run
           another one.

`docker compose config --format json` produces <config.json>. Its build
contexts are absolute paths; they are compared relative to <workspace>, so a
build job and a lane on different runners agree.
"""

from __future__ import annotations

import json
import pathlib
import sys


class StackImageError(Exception):
    pass


def image_name(image: str) -> str:
    """`ghcr.io/org/elitea-main:standalone` → `elitea-main`."""
    last = image.rsplit("/", 1)[-1]
    return last.split("@", 1)[0].split(":", 1)[0]


def build_spec(build: dict, workspace: str) -> dict:
    context = pathlib.PurePosixPath(build.get("context") or ".")
    root = pathlib.PurePosixPath(workspace)
    try:
        relative = str(context.relative_to(root)) if context.is_absolute() else str(context)
    except ValueError as error:
        raise StackImageError(f"build context {context} is outside the workspace {workspace}") from error
    return {
        "context": relative,
        "dockerfile": build.get("dockerfile") or "Dockerfile",
        "target": build.get("target") or "",
        "args": dict(sorted((build.get("args") or {}).items())),
    }


def built_services(config: dict, workspace: str) -> dict[str, tuple[str, dict]]:
    """service → (image tag, build spec), for every service with `build:`."""
    out = {}
    for name, service in sorted((config.get("services") or {}).items()):
        build = service.get("build")
        if not build:
            continue
        image = service.get("image")
        if not image:
            raise StackImageError(f"service {name} builds an image but declares no `image:` to tag it with")
        out[name] = (image, build_spec(build, workspace))
    return out


def plan(config: dict, workspace: str, prefix: str, tag: str, services: list[str]) -> tuple[list[dict], list[str]]:
    available = built_services(config, workspace)
    entries, sets, seen = [], [], {}
    for service in services:
        if service not in available:
            raise StackImageError(f"{service} is not a `build:` service of these compose files")
        image, spec = available[service]
        name = image_name(image)
        if name in seen:
            raise StackImageError(f"{service} and {seen[name]} are the same image ({name}); build it once")
        seen[name] = service
        ref = f"{prefix}{name}"
        entries.append({"service": service, "image": name, "ref": ref, "tag": tag, "build": spec})
        sets.append(f"{service}.tags={ref}:{tag}")
    return entries, sets


def record(entries: list[dict], metadata: dict) -> dict:
    out = {}
    for entry in entries:
        digest = (metadata.get(entry["service"]) or {}).get("containerimage.digest", "")
        if not digest.startswith("sha256:"):
            raise StackImageError(f"bake reported no pushed digest for {entry['service']} ({entry['image']})")
        out[entry["image"]] = {"ref": entry["ref"], "digest": digest, "build": entry["build"]}
    return out


def merge(fragments: list[tuple[str, dict]]) -> dict:
    merged: dict = {}
    for source, fragment in fragments:
        for name, entry in fragment.items():
            if name in merged and merged[name] != entry:
                raise StackImageError(f"{name} appears twice in the digest map with different contents ({source})")
            merged[name] = entry
    return merged


def resolve(config: dict, workspace: str, digests: dict) -> list[tuple[str, str]]:
    pulls: dict[str, str] = {}
    problems = []
    for service, (image, spec) in built_services(config, workspace).items():
        name = image_name(image)
        entry = digests.get(name)
        if entry is None:
            problems.append(f"{image} (service {service}): no image named {name} was built for this commit")
            continue
        if entry["build"] != spec:
            problems.append(
                f"{image} (service {service}): built from {entry['build']}, but this compose builds {spec}"
            )
            continue
        pulls.setdefault(image, f"{entry['ref']}@{entry['digest']}")
    if problems:
        raise StackImageError("; ".join(problems))
    if not pulls:
        raise StackImageError("the compose files declare no build: services — pulling nothing would be vacuous")
    return sorted(pulls.items())


def _load(path: str):
    return json.loads(pathlib.Path(path).read_text())


def main(argv: list[str]) -> int:
    try:
        if len(argv) >= 8 and argv[1] == "plan":
            entries, sets = plan(_load(argv[2]), argv[3], argv[4], argv[5], argv[7:])
            pathlib.Path(argv[6]).write_text(json.dumps(entries, indent=2, sort_keys=True))
            print("\n".join(sets))
        elif len(argv) == 4 and argv[1] == "record":
            print(json.dumps(record(_load(argv[2]), _load(argv[3])), indent=2, sort_keys=True))
        elif len(argv) == 5 and argv[1] == "resolve":
            files = sorted(pathlib.Path(argv[4]).glob("*.json"))
            if not files:
                raise StackImageError(f"no digest files in {argv[4]}")
            digests = merge([(str(f), _load(str(f))) for f in files])
            for local, remote in resolve(_load(argv[2]), argv[3], digests):
                print(f"{local}\t{remote}")
        else:
            print(__doc__, file=sys.stderr)
            return 2
    except StackImageError as error:
        print(f"::error::{error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
