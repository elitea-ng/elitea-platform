#!/usr/bin/env python3
"""Every push arm that names `main` must also name the release staging branch.

Issue #518. `helm-lint.yml` guarded `deploy/helm/**`, and its push arm named
`main` only. The release staging branch is where pull requests land first, and
that branch becomes main later. So the chart on the staging branch was
unrenderable and no push run reported it. A measurement made on 2026-08-18 gives
the size of the hole: `actions/runs?branch=claude/issue-248-c5b380&event=push`
returned `total_count: 0` for EVERY workflow in this repository. Not one push
run has ever started on the staging branch.

The same audit found a second shape of the same defect. Four workflows named
their OWN file in the pull_request paths and not in the push paths, so a push
that edited only the gate could not start the gate it edited. This check holds
that too.

The gate is not the wiring. `helm-lint.yml` was correct in its own file, and it
could not start where it was needed. So this check holds the wiring, and it
reads two artifacts:

  .github/staging-branch.txt   one line, the branch name, or the file is absent
  .github/workflows/*.yml      the push arm of each workflow

Two states, and both are checked:

  1. The file names a branch. That branch must exist on `origin`, and every
     workflow whose push arm names `main` must also name it, unless the
     exemption table below names the workflow with a reason.
  2. The file is absent or blank. The staging branch is retired, so NO workflow
     may keep a push arm for it. A dead branch name in a trigger is a gate that
     stopped starting in silence — the same defect in the other direction.

This file reads only the standard library, so no job needs to install anything.
"""

from __future__ import annotations

import pathlib
import re
import subprocess
import sys

REPOSITORY_ROOT = pathlib.Path(__file__).resolve().parents[2]
WORKFLOW_DIRECTORY = REPOSITORY_ROOT / ".github" / "workflows"
STAGING_BRANCH_FILE = REPOSITORY_ROOT / ".github" / "staging-branch.txt"

# A count below this floor means the glob stopped matching, not that the
# repository got smaller. "Nothing found, therefore correct" is how
# check-playwright-image-tag stopped gating in silence.
MINIMUM_WORKFLOW_COUNT = 10

# Branch names a push arm may hold that are not the staging branch. `next` is
# publish.yml's second release line.
PERMANENT_BRANCHES = {"main", "next"}

# Workflows that must NOT carry the staging arm, each with the reason. An entry
# here is a decision, not a silence.
EXEMPT_WORKFLOWS = {
    "publish.yml": (
        "It cuts a release: it tags, builds and pushes images, and writes to "
        "the registry. A staging arm would publish release artifacts from a "
        "release candidate."
    ),
    "deepwiki-real-engine.yml": (
        "It has no push arm at all, and no pull_request arm either. It builds "
        "the ~2 GB DeepWiki `-engine` image (torch, faiss, tree-sitter) and "
        "runs a real analysis pass over the standalone stack; the per-change "
        "gate for the same product surface is the `deepwiki-stack` job in "
        "ci-web-e2e.yml, which uses the provider's fixture engine. Weekly "
        "cadence plus manual dispatch, like ci-web-mutation.yml."
    ),
    "nightly-real-llm.yml": (
        "No push arm and no pull_request arm: it is the real-model chat lane's "
        "exploratory superset (the two tool-call-offering specs the PR lane "
        "leaves out, on both runtimes) on a nightly cadence plus manual "
        "dispatch. The per-change gate for the same surface is the "
        "`chat-stream-real` job in ci-web-e2e.yml."
    ),
    "engine-closure-refresh.yml": (
        "It has no push arm and no pull_request arm, deliberately: it WRITES. "
        "It re-resolves the two frozen analysis-engine dependency closures as a "
        "set and opens a draft pull request with the result, so a push arm "
        "would mean a fresh proposal on every merge to main. Weekly cadence "
        "plus manual dispatch, the same shape as deepwiki-real-engine.yml. The "
        "per-change gate for the same subject is the `engine-closures` job in "
        "dependency-scanning.yml, which refuses a closure that does not "
        "resolve."
    ),
    "ci-web-mutation.yml": (
        "It has no push arm at all. It is a weekly cadence job plus manual "
        "dispatch, not a per-merge gate."
    ),
    # Issue #526 removed the time-limited `ci-web.yml` entry. Commit 98fb8cff
    # (#549) retired the release staging branch: it deleted
    # `.github/staging-branch.txt`, and origin holds no branch of that name any
    # more. So the entry excused a coverage rule that state 2 already switches
    # off, and it kept `ci-web.yml` — the largest gate set here — silent in the
    # one state where the rule speaks again. Do NOT add a staging arm to
    # `ci-web.yml`: with the branch retired, the stale-name rule below rejects
    # it.
}

PUSH_BLOCK = re.compile(
    r"^  push:\n(?P<body>(?:(?:    .*)?\n)*?)(?=^  \S|^\S|\Z)",
    re.MULTILINE,
)
BRANCHES_BLOCK = re.compile(
    r"^    branches:\n(?P<body>(?:(?:      .*)?\n)*?)(?=^    \S|\Z)",
    re.MULTILINE,
)
BRANCH_ITEM = re.compile(r'^      - "?(?P<name>[^"\n#]+?)"?\s*$', re.MULTILINE)
PATHS_BLOCK = re.compile(
    r"^    paths:\n(?P<body>(?:(?:      .*)?\n)*?)(?=^    \S|\Z)",
    re.MULTILINE,
)


def read_staging_branch() -> str:
    if not STAGING_BRANCH_FILE.exists():
        return ""
    return STAGING_BRANCH_FILE.read_text(encoding="utf-8").strip()


def push_branches(text: str) -> list[str] | None:
    """Return the branch names of the push arm, or None when there is no arm.

    An empty list is a parse failure, never a pass: a push arm always names at
    least one branch, so zero names means this reader stopped reading.
    """
    push_match = PUSH_BLOCK.search(text)
    if push_match is None:
        return None
    branches_match = BRANCHES_BLOCK.search(push_match.group("body"))
    if branches_match is None:
        return []
    return [
        item.group("name").strip()
        for item in BRANCH_ITEM.finditer(branches_match.group("body"))
    ]


def push_paths(text: str) -> list[str] | None:
    """Return the path filters of the push arm, or None when there are none.

    None means the arm selects every file, which is the strongest state and
    needs no self-reference.
    """
    push_match = PUSH_BLOCK.search(text)
    if push_match is None:
        return None
    paths_match = PATHS_BLOCK.search(push_match.group("body"))
    if paths_match is None:
        return None
    return [
        item.group("name").strip()
        for item in BRANCH_ITEM.finditer(paths_match.group("body"))
    ]


def branch_exists_on_origin(branch: str) -> tuple[bool, str]:
    """Ask origin. Report the unreachable case apart from the absent case."""
    completed = subprocess.run(
        ["git", "ls-remote", "--heads", "origin", f"refs/heads/{branch}"],
        cwd=REPOSITORY_ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    if completed.returncode != 0:
        return False, (
            f"git ls-remote could not read origin (exit {completed.returncode}): "
            f"{completed.stderr.strip()}"
        )
    if not completed.stdout.strip():
        return False, f"origin holds no branch named {branch!r}"
    return True, completed.stdout.strip().splitlines()[0]


def main() -> int:
    workflows = sorted(WORKFLOW_DIRECTORY.glob("*.yml"))
    if len(workflows) < MINIMUM_WORKFLOW_COUNT:
        print(
            f"::error::found {len(workflows)} workflow file(s) in "
            f"{WORKFLOW_DIRECTORY}, expected at least {MINIMUM_WORKFLOW_COUNT} — "
            "the glob stopped matching, so this check measured nothing",
            file=sys.stderr,
        )
        return 1

    staging_branch = read_staging_branch()
    failures: list[str] = []

    if staging_branch:
        reachable, detail = branch_exists_on_origin(staging_branch)
        if not reachable:
            failures.append(
                f".github/staging-branch.txt names {staging_branch!r}, and {detail}. "
                "Either correct the name, or delete the file and remove the "
                "staging arm from every workflow push trigger. A trigger that "
                "names a branch nobody pushes to is a gate that stopped "
                "starting in silence."
            )
        else:
            print(f"staging branch: {staging_branch} -> {detail}")
    else:
        print(
            ".github/staging-branch.txt is absent or blank: no workflow may "
            "keep a staging push arm."
        )

    for workflow in workflows:
        name = workflow.name
        text = workflow.read_text(encoding="utf-8")
        branches = push_branches(text)

        if branches is None:
            print(f"  {name}: no push trigger")
            if name not in EXEMPT_WORKFLOWS:
                failures.append(
                    f"{name} has no push trigger and no entry in the exemption "
                    "table. Add the trigger, or record the reason it has none."
                )
            continue

        if not branches:
            failures.append(
                f"{name} has a push trigger and this check read no branch name "
                "out of it. The reader stopped reading; correct the reader "
                "rather than the workflow."
            )
            continue

        print(f"  {name}: push branches = {', '.join(branches)}")

        stale = [b for b in branches if b not in PERMANENT_BRANCHES and b != staging_branch]
        if stale:
            failures.append(
                f"{name} names {', '.join(stale)} in its push trigger, and "
                f".github/staging-branch.txt names "
                f"{staging_branch or '(nothing)'}. Remove the stale name or "
                "correct the file."
            )

        paths = push_paths(text)
        if paths is not None:
            if not paths:
                failures.append(
                    f"{name} has a `paths:` key on its push arm and this check "
                    "read no path out of it. The reader stopped reading."
                )
            elif f".github/workflows/{name}" not in paths:
                failures.append(
                    f"{name} filters its push arm by path and does not name "
                    f".github/workflows/{name}. A push that edits only the gate "
                    "then cannot start the gate it edited."
                )

        if name in EXEMPT_WORKFLOWS:
            if staging_branch and staging_branch in branches:
                failures.append(
                    f"{name} is in the exemption table and names "
                    f"{staging_branch} anyway. Remove the exemption entry."
                )
            else:
                print(f"    exempt: {EXEMPT_WORKFLOWS[name]}")
            continue

        if "main" in branches and staging_branch and staging_branch not in branches:
            failures.append(
                f"{name} names `main` in its push trigger and does not name "
                f"{staging_branch}. A workflow that runs only on main cannot "
                "guard the branch that becomes main. Add the branch, or add an "
                "entry to EXEMPT_WORKFLOWS in this script with the reason."
            )

    unknown = sorted(set(EXEMPT_WORKFLOWS) - {w.name for w in workflows})
    if unknown:
        failures.append(
            "the exemption table names workflows that do not exist: "
            + ", ".join(unknown)
            + ". An exemption for a deleted workflow hides the next one."
        )

    if failures:
        for failure in failures:
            print(f"::error::{failure}", file=sys.stderr)
        return 1

    print(f"workflow branch coverage: {len(workflows)} workflow(s) checked, all correct")
    return 0


if __name__ == "__main__":
    sys.exit(main())
