#!/usr/bin/env python3
"""Fail a workflow cache step that can WRITE from a pull request.

THE RULE: pull requests read the Actions cache, only a push to main writes it.

WHY (measured 2026-10-16). The repository's Actions cache is capped at 10 GB
and evicted least-recently-used. It held 10.7 GB in 809 entries. The largest
writers were pull-request refs: `setup-go` entries of 0.3-1.2 GB per pull
request and ~2.9 GB of pull-request BuildKit blobs. A pull request can read
main's entries, but its own writes are copies nothing else reads, and every
one of them evicted something main had written. Main held no Go cache at all
at the time, and a layer one run wrote was gone by the next; `ignore-error`
on the BuildKit exports made every miss silent.

WHAT IT CHECKS, in every .github/workflows/*.yml and .github/actions/*/action.yml:

  actions/setup-go        `cache: false` (the cache is .github/actions/go-cache)
  actions/setup-node      no `cache:` input, and `package-manager-cache: false`
                          (the cache is .github/actions/npm-cache)
  actions/setup-python    no `cache:` input
  actions/cache           (the full action, which saves in a post step) and
  actions/cache/save      the step's `if:` holds BOTH
                          `github.ref == 'refs/heads/main'` and
                          `github.event_name == 'push'`
  Swatinem/rust-cache     `save-if:` names `github.ref == 'refs/heads/main'`,
                          or is a literal false (restore only)
  golangci-lint-action    `skip-cache: true`, or `skip-save-cache:` gated on
                          a push to main
  trivy-action            `cache: "false"` (its DB cache is a full actions/cache)
  cache-to (BuildKit)     on docker/build-push-action, as a bake-action `set`
                          line, or in a `docker buildx bake` run line: empty
                          unless a push to main, i.e. an expression holding the
                          main ref, `github.event_name == 'push'` and an
                          `|| ''` fallback. A `docker buildx bake` run that
                          builds (not `--print`) must clear `*.cache-to=`,
                          because docker-bake.hcl declares mode=max exports.
  build-stack-images      its `cache-write` default is a push to main, and no
                          caller passes anything but "false"; `mount-cache-writer`
                          is "true" or "false", "true" on at most ONE job per
                          mount-cache-suffix across ALL workflows (the key is
                          repository-wide), and never on every leg of a
                          matrix job (one job, not N legs, saves the mounts:
                          N writers raced for one key, measured 2026-10-20).
                          A matrix job names its writer legs with
                          `${{ matrix.<key> }}` over an include-only matrix
                          whose entries give the literal; each "true" leg
                          counts as one writer of that leg's suffix.
  deploy/ci/*.hcl         declares no cache-to (it is merged into every bake)

A workflow whose ONLY trigger is a push to main/next (publish.yml, the release)
is exempt from the cache-to, actions/cache and trivy rules: it cannot run for
a pull request. Every other rule still applies to it.

This reads YAML with PyYAML. It fails, rather than passes, when it finds fewer
workflows or cache steps than its floors: "found nothing, therefore clean" is
how a guard stops guarding in silence.
"""

from __future__ import annotations

import argparse
import pathlib
import re
import sys

import yaml

REPOSITORY_ROOT = pathlib.Path(__file__).resolve().parents[2]

MINIMUM_WORKFLOWS = 10
MINIMUM_CACHE_STEPS = 20

MAIN_REF = "github.ref == 'refs/heads/main'"
PUSH_EVENT = "github.event_name == 'push'"
RELEASE_BRANCHES = {"main", "next"}


def _norm(value) -> str:
    """Collapse whitespace so `github.ref=='x'` and spacing variants compare."""
    text = "" if value is None else str(value)
    text = re.sub(r"\s+", " ", text)
    text = re.sub(r"\s*==\s*", " == ", text)
    text = re.sub(r"\s*!=\s*", " != ", text)
    return text.replace('"', "'")


def _is_false(value) -> bool:
    return value is False or str(value).strip().lower() == "false"


def _gated_on_main_push(expr) -> bool:
    text = _norm(expr)
    return MAIN_REF in text and PUSH_EVENT in text


def _negated_main_push(expr) -> bool:
    """`!(main && push)` — the skip-side spelling of the same gate."""
    text = _norm(expr).replace(" ", "")
    return _gated_on_main_push(expr) and "!(" in text


MATRIX_REF = re.compile(r"\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}")


def _include_only_legs(job: dict) -> list[dict] | None:
    """The legs of a matrix made of `include:` entries alone, else None.

    With any other axis, an include entry can merge into several legs, so a
    per-entry value would not be a per-leg one.
    """
    matrix = (job.get("strategy") or {}).get("matrix")
    if not isinstance(matrix, dict) or set(matrix) != {"include"}:
        return None
    include = matrix["include"]
    if not isinstance(include, list) or not all(isinstance(e, dict) for e in include):
        return None
    return include


def _cache_to_gated(value) -> bool:
    """A cache-to that is empty unless a push to main."""
    text = _norm(value).strip()
    if text == "":
        return True
    if "${{" not in text:
        return False
    return _gated_on_main_push(text) and re.search(r"\|\|\s*''", text) is not None


def _triggers(doc) -> dict:
    on = doc.get(True, doc.get("on"))
    if isinstance(on, str):
        return {on: None}
    if isinstance(on, list):
        return {k: None for k in on}
    return on or {}


def _release_only(doc) -> bool:
    on = _triggers(doc)
    if set(on) != {"push"}:
        return False
    push = on["push"] or {}
    branches = set(push.get("branches") or [])
    if not branches and not push.get("tags"):
        return False  # every branch
    return branches <= RELEASE_BRANCHES


def _steps(doc):
    """(job id, step) for a workflow; ("<action>", step) for a composite."""
    if "runs" in doc:
        for step in (doc["runs"] or {}).get("steps") or []:
            yield "<action>", step
        return
    for job_id, job in (doc.get("jobs") or {}).items():
        for step in (job or {}).get("steps") or []:
            yield job_id, step


def _uses(step) -> str:
    return str(step.get("uses") or "")


def _action(step) -> str:
    return _uses(step).split("@", 1)[0]


class Report:
    def __init__(self):
        self.errors: list[str] = []
        self.cache_steps = 0
        # mount-cache-suffix -> ["<file> [<job>]", ...], across every workflow.
        self.mount_writers: dict[str, list[str]] = {}

    def fail(self, where, message):
        self.errors.append(f"{where}: {message}")


def check_document(name: str, doc: dict, report: Report) -> None:
    release_only = _release_only(doc) if "runs" not in doc else False
    for job_id, step in _steps(doc):
        if not isinstance(step, dict):
            continue
        action = _action(step)
        with_ = step.get("with") or {}
        label = step.get("name") or _uses(step) or "run step"
        where = f"{name} [{job_id}] {label!r}"
        cond = step.get("if")

        if action == "actions/setup-go":
            report.cache_steps += 1
            if not _is_false(with_.get("cache", True)):
                report.fail(where, "actions/setup-go without `cache: false` saves its Go cache from every ref, pull requests included; use .github/actions/go-cache after it")
        elif action == "actions/setup-node":
            report.cache_steps += 1
            if "cache" in with_:
                report.fail(where, "actions/setup-node `cache:` saves from every ref; drop it and use .github/actions/npm-cache")
            if not _is_false(with_.get("package-manager-cache", True)):
                report.fail(where, "actions/setup-node needs `package-manager-cache: false`, or its automatic cache saves from every ref")
        elif action == "actions/setup-python":
            report.cache_steps += 1
            if "cache" in with_:
                report.fail(where, "actions/setup-python `cache:` saves from every ref, pull requests included")
        elif action in ("actions/cache", "actions/cache/save"):
            report.cache_steps += 1
            if not release_only and not _gated_on_main_push(cond):
                report.fail(where, f"{action} saves the cache; its `if:` must require {MAIN_REF} && {PUSH_EVENT} (use actions/cache/restore to read)")
        elif action == "actions/cache/restore":
            report.cache_steps += 1
        elif action == "Swatinem/rust-cache":
            report.cache_steps += 1
            # A literal `false` (restore only) is the strongest form.
            if not _is_false(with_.get("save-if")) and MAIN_REF not in _norm(with_.get("save-if")):
                report.fail(where, f"Swatinem/rust-cache needs `save-if: ${{{{ {MAIN_REF} }}}}`; without it every pull request saves its target directory")
        elif action == "golangci/golangci-lint-action":
            report.cache_steps += 1
            skip_all = str(with_.get("skip-cache", "")).strip().lower() == "true"
            if not skip_all and not _negated_main_push(with_.get("skip-save-cache")):
                report.fail(where, "golangci-lint-action saves its cache from every ref; set `skip-save-cache: ${{ !(github.ref == 'refs/heads/main' && github.event_name == 'push') }}`")
        elif action == "aquasecurity/trivy-action":
            report.cache_steps += 1
            if not release_only and not _is_false(with_.get("cache", True)):
                report.fail(where, "trivy-action caches its DB with a full actions/cache from every ref; set `cache: \"false\"` and restore it through .github/actions/main-write-cache")
        elif action.endswith("/build-stack-images"):
            report.cache_steps += 1
            if "cache-write" in with_ and not _is_false(with_["cache-write"]):
                report.fail(where, "build-stack-images `cache-write` may only be omitted (a push to main writes) or \"false\"")
            writer = str(with_.get("mount-cache-writer", "false")).strip().lower()
            job = (doc.get("jobs") or {}).get(job_id) or {}
            legs = _include_only_legs(job)
            if MATRIX_REF.fullmatch(writer) and legs is not None:
                # One writer per LEG, named in an include-only matrix: every
                # leg is exactly one include entry, so the literal each entry
                # gives is the whole truth about that leg. The suffix is read
                # per leg the same way, and each "true" leg counts as one
                # writer below, so two legs of one suffix are still refused.
                writer_key = MATRIX_REF.fullmatch(writer).group(1)
                suffix_raw = str(with_.get("mount-cache-suffix", ""))
                suffix_key = MATRIX_REF.fullmatch(suffix_raw.strip())
                for index, leg in enumerate(legs):
                    value = str(leg.get(writer_key, "")).strip().lower()
                    if value not in ("true", "false"):
                        report.fail(where, f"matrix leg {index} gives `{writer_key}` = {leg.get(writer_key)!r}; each include entry must give the literal \"true\" or \"false\"")
                    elif value == "true":
                        suffix = str(leg.get(suffix_key.group(1), "")) if suffix_key else suffix_raw
                        report.mount_writers.setdefault(suffix, []).append(f"{name} [{job_id} leg {index}]")
            elif writer not in ("true", "false"):
                report.fail(where, f"build-stack-images `mount-cache-writer` must be the literal \"true\" or \"false\", or `${{{{ matrix.<key> }}}}` of an include-only matrix whose entries give the literal; got {with_.get('mount-cache-writer')!r}; an expression can make every leg a writer")
            elif writer == "true":
                if (job.get("strategy") or {}).get("matrix"):
                    report.fail(where, "build-stack-images `mount-cache-writer: \"true\"` on a matrix job makes every leg a writer racing for one cache key; designate one non-matrix job, or one leg through an include-only matrix")
                suffix = str(with_.get("mount-cache-suffix", ""))
                report.mount_writers.setdefault(suffix, []).append(f"{name} [{job_id}]")

        # BuildKit layer exports.
        if not release_only:
            if "cache-to" in with_:
                report.cache_steps += 1
                if not _cache_to_gated(with_["cache-to"]):
                    report.fail(where, f"cache-to must be empty unless a push to main (`${{{{ {MAIN_REF} && {PUSH_EVENT} && '<spec>' || '' }}}}`); got {with_['cache-to']!r}")
            if action.startswith("docker/bake-action"):
                files = str(with_.get("files") or "")
                sets = [ln.strip() for ln in str(with_.get("set") or "").splitlines() if ln.strip()]
                cache_sets = [s for s in sets if re.match(r"^[^=\s]+\.cache-to=", s)]
                for s in cache_sets:
                    report.cache_steps += 1
                    if not _cache_to_gated(s.split("=", 1)[1]):
                        report.fail(where, f"bake `set` {s!r} writes the layer cache from every run; make it empty unless a push to main")
                if "docker-bake.hcl" in files:
                    targets = [t.strip() for t in str(with_.get("targets") or "").split() if t.strip()]
                    covered = {s.split(".cache-to=", 1)[0] for s in cache_sets}
                    for t in targets:
                        if t not in covered and "*" not in covered:
                            report.fail(where, f"bake target {t!r} from docker-bake.hcl keeps its mode=max cache-to; override `{t}.cache-to=` gated on a push to main")
            run = str(step.get("run") or "")
            for line in run.splitlines():
                if re.search(r"\bbuildx\s+bake\b", line) and "--print" not in line:
                    report.cache_steps += 1
                    if not re.search(r"--set[= ]['\"]?\*\.cache-to=['\"]?(\s|$)", line):
                        report.fail(where, "a `docker buildx bake` run inherits docker-bake.hcl's mode=max cache-to; add `--set '*.cache-to='`")
                    break

    # The composite action's own write default.
    if "runs" in doc and name.endswith("build-stack-images/action.yml"):
        default = ((doc.get("inputs") or {}).get("cache-write") or {}).get("default")
        if not _gated_on_main_push(default):
            report.fail(name, f"the `cache-write` default must be {MAIN_REF} && {PUSH_EVENT}; got {default!r}")


def check_hcl(name: str, text: str, report: Report) -> None:
    for number, line in enumerate(text.splitlines(), 1):
        if re.match(r"^\s*cache-to\s*=", line):
            report.fail(f"{name}:{number}", "deploy/ci HCL is merged into every CI bake, so a cache-to here writes from pull requests; the write side belongs to build-stack-images")


def run(root: pathlib.Path) -> Report:
    report = Report()
    workflows = sorted((root / ".github" / "workflows").glob("*.yml")) + sorted((root / ".github" / "workflows").glob("*.yaml"))
    actions = sorted((root / ".github" / "actions").glob("*/action.yml")) + sorted((root / ".github" / "actions").glob("*/action.yaml"))
    for path in workflows + actions:
        rel = str(path.relative_to(root))
        doc = yaml.safe_load(path.read_text()) or {}
        check_document(rel, doc, report)
    # The mount-cache key is buildkit-mounts-v3-<os>-<suffix>-…: one key space
    # per suffix for the whole REPOSITORY, so two writers in two different
    # workflows race exactly as two in one workflow did.
    for suffix, jobs in sorted(report.mount_writers.items()):
        if len(jobs) > 1:
            report.fail("build-stack-images", f"{len(jobs)} jobs ({', '.join(jobs)}) pass mount-cache-writer: \"true\" for mount-cache-suffix {suffix!r}; exactly one job in the repository may save those mounts")
    for path in sorted((root / "deploy" / "ci").glob("*.hcl")):
        check_hcl(str(path.relative_to(root)), path.read_text(), report)
    report.workflows = len(workflows)
    return report


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--root", type=pathlib.Path, default=REPOSITORY_ROOT)
    parser.add_argument("--min-workflows", type=int, default=MINIMUM_WORKFLOWS)
    parser.add_argument("--min-cache-steps", type=int, default=MINIMUM_CACHE_STEPS)
    args = parser.parse_args(argv)
    report = run(args.root)
    if report.workflows < args.min_workflows:
        print(f"::error::found {report.workflows} workflow file(s), expected at least {args.min_workflows} — the glob stopped matching, so this check read nothing")
        return 1
    if report.cache_steps < args.min_cache_steps:
        print(f"::error::found {report.cache_steps} cache step(s), expected at least {args.min_cache_steps} — the step matching stopped working, so this check measured nothing")
        return 1
    for error in report.errors:
        print(f"::error::{error}")
    if report.errors:
        print(f"{len(report.errors)} cache step(s) can write from a pull request")
        return 1
    print(f"workflow caches: {report.cache_steps} cache step(s) in {report.workflows} workflow(s) and the composite actions; none writes from a pull request")
    return 0


if __name__ == "__main__":
    sys.exit(main())
