#!/usr/bin/env python3
"""ci-web-e2e.yml starts on every change that can change what its lanes run.

The workflow is path-filtered, and a path filter is a list someone has to
keep complete: a change to an input it does not name never starts the lanes
that would have caught it, and the checks list goes green by absence. This
derives the inputs instead of trusting the list:

  * every `build:` service of the compose files the lanes run: its
    Containerfile and every COPY source (from the build context), and
  * every repository script the workflow or the local actions it uses call
    (`scripts/...` references in their YAML),

and fails, naming the path, when the `pull_request` filter covers none of
them — or when the `push` filter differs from the `pull_request` one. It
also runs scripts/ci/e2e-lane-filter.sh (the `model-path-changes` job's
skip rule) over each input and fails on any it would call outside the stack:
the two lists must agree that a stack input starts the stack lanes.

Patterns are matched the way GitHub matches them for this list's shapes:
`dir/**` is everything under dir/, anything else is an exact path or an
fnmatch glob. A negated pattern (`!x`) is not used here and is refused, so
the matcher never has to model one.
"""

from __future__ import annotations

import fnmatch
import pathlib
import re
import subprocess
import sys

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github" / "workflows" / "ci-web-e2e.yml"
COMPOSE_FILES = [
    "deploy/docker-compose.e2e-standalone.yml",
    "deploy/docker-compose.standalone-full.yml",
    "deploy/docker-compose.standalone-rust-agent.yml",
]
# Floors: a parser that stopped finding things must not pass.
MINIMUM_BUILD_SERVICES = 15
MINIMUM_INPUTS = 40


class _Loader(yaml.SafeLoader):
    pass


# Compose's merge tags (`!reset`, `!override`) carry no meaning for this read.
_Loader.add_multi_constructor("!", lambda loader, suffix, node: loader.construct_object(node) if not isinstance(node, yaml.ScalarNode) else loader.construct_scalar(node))


def covered(path: str, patterns: list[str]) -> bool:
    # A COPY of a directory is covered only by a pattern that takes the whole
    # directory: `dir/**` itself or one of its parents'.
    probe = path + "/" if (ROOT / path).is_dir() else path
    for pattern in patterns:
        if pattern.endswith("/**"):
            if probe.startswith(pattern[:-2]):
                return True
        elif path == pattern or fnmatch.fnmatchcase(path, pattern):
            return True
    return False


def containerfile_inputs(context: pathlib.Path, dockerfile: pathlib.Path) -> set[str]:
    inputs = {str(dockerfile.relative_to(ROOT))}
    text = dockerfile.read_text()
    joined = re.sub(r"\\\n", " ", text)
    for line in joined.splitlines():
        words = line.split()
        if not words or words[0].upper() not in ("COPY", "ADD"):
            continue
        args = [w for w in words[1:] if not w.startswith("--")]
        if any(w.startswith("--from") for w in words[1:]) or any(a.startswith("<<") for a in args):
            continue
        for source in args[:-1]:
            if "://" in source:
                continue  # ADD of a URL: not a repository input
            path = (context / source).resolve()
            inputs.add(str(path.relative_to(ROOT)))
    return inputs


def script_references(text: str) -> set[str]:
    return set(re.findall(r"\b(scripts/[A-Za-z0-9_./-]+\.(?:sh|py|mjs))\b", text))


def main() -> int:
    workflow = yaml.safe_load(WORKFLOW.read_text())
    on = workflow.get(True, workflow.get("on"))
    pr = list(on["pull_request"]["paths"])
    push = list(on["push"]["paths"])
    errors = []
    if any(p.startswith("!") for p in pr + push):
        errors.append("negated path patterns are not modelled by this check")
    if sorted(pr) != sorted(push):
        errors.append(f"the push paths differ from the pull_request paths: only in pull_request {sorted(set(pr) - set(push))}, only in push {sorted(set(push) - set(pr))}")

    inputs: dict[str, str] = {}
    services = 0
    for compose in COMPOSE_FILES:
        doc = yaml.load((ROOT / compose).read_text(), Loader=_Loader) or {}
        for name, service in (doc.get("services") or {}).items():
            build = (service or {}).get("build")
            if not isinstance(build, dict):
                continue
            services += 1
            context = (ROOT / "deploy" / build.get("context", ".")).resolve()
            dockerfile = context / build.get("dockerfile", "Dockerfile")
            for path in containerfile_inputs(context, dockerfile):
                inputs.setdefault(path, f"{compose} service {name}")
    texts = {str(WORKFLOW.relative_to(ROOT)): WORKFLOW.read_text()}
    for action in re.findall(r"uses: \./(\.github/actions/[A-Za-z0-9_-]+)", WORKFLOW.read_text()):
        path = ROOT / action / "action.yml"
        texts[str(path.relative_to(ROOT))] = path.read_text()
        for nested in re.findall(r"uses: \./(\.github/actions/[A-Za-z0-9_-]+)", path.read_text()):
            texts[f"{nested}/action.yml"] = (ROOT / nested / "action.yml").read_text()
    for where, text in texts.items():
        for script in script_references(text):
            if (ROOT / script).exists():
                inputs.setdefault(script, f"called from {where}")

    if services < MINIMUM_BUILD_SERVICES or len(inputs) < MINIMUM_INPUTS:
        errors.append(f"found {services} build service(s) and {len(inputs)} input(s), below the floors {MINIMUM_BUILD_SERVICES}/{MINIMUM_INPUTS}: the reader stopped finding things")
    for path, why in sorted(inputs.items()):
        if not covered(path, pr):
            errors.append(f"{path} ({why}) is an input of the E2E lanes and no paths pattern covers it")
    lane_filter = ROOT / "scripts" / "ci" / "e2e-lane-filter.sh"
    for path, why in sorted(inputs.items()):
        # A directory input stands for any file under it; probe with one the
        # filter cannot treat as prose.
        probe = path + "/probe.rs" if (ROOT / path).is_dir() else path
        out = subprocess.run(["bash", str(lane_filter)], input=probe + "\n", capture_output=True, text=True, cwd=ROOT).stdout
        if "reaches-stack=true" not in out:
            errors.append(f"{path} ({why}) is an input of the E2E lanes, but e2e-lane-filter.sh says a change to it skips them: {out.strip()!r}")
    for error in errors:
        print(f"::error::{error}")
    if errors:
        return 1
    print(f"ci-web-e2e.yml paths: {len(inputs)} input(s) of {services} build service(s) and the scripts, all covered by {len(pr)} pattern(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
