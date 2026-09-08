#!/usr/bin/env python3
"""Re-resolve a frozen engine's dependency closure as a SET, and re-stamp it.

WHY THIS EXISTS
===============

`services/elitea-deepwiki` and `services/elitea-inventory` each vendor a
byte-frozen copy of an analysis engine (`src/*/engine/`, verified against
`COPY_MANIFEST.json` by each package's `tools/refresh_engine_copy.py`), and each
pins that copy's resolved dependency closure in an `engine` extra.

Ten Dependabot bumps moved single pins inside those closures and made them
unsatisfiable — #677-#679 and #757-#761 for DeepWiki, #740 and #741 for
Inventory. `.github/dependabot.yml` therefore gives both packages their own pip
entry with `ignore: "*"` and `open-pull-requests-limit: 0`.

That silence has a cost, and it is the reason this file exists: the closure
carries open security advisories (aiohttp among them) with no automated route
to a fix. Turning the scanner back on is not that route — the scanner is what
broke it, because it moves one pin at a time. This tool is the route: it moves
the WHOLE SET at once, which is the only way a closure of ~92 mutually-bounded
pins can move at all.

THE INVARIANT, STATED ONCE
==========================

    The pins in an `engine` extra ARE the resolution of a PARTICULAR frozen
    copy. They move together with that copy, and never one at a time.

Both halves are enforced rather than documented:

  * one at a time -> `scripts/ci/check-engine-closures.sh` resolves both extras
    on every pull request and every day, and a single moved pin is a
    ResolutionImpossible it reports in seconds.
  * together with the copy -> each pyproject carries
    `# closure-stamp: COPY_MANIFEST.json sha256 <digest>`. This tool writes it;
    that gate refuses a tree where the copy moved and the stamp did not.

WHAT IT DOES
============

    python3 scripts/ci/refresh-engine-closure.py                 # both packages
    python3 scripts/ci/refresh-engine-closure.py PACKAGE ...     # named ones

    --check       resolve and report drift; change nothing; exit 3 when the
                  closure would move. This is what the scheduled workflow runs
                  to decide whether there is anything to open a pull request
                  about. Exit 1 stays "the tool failed".
    --stamp-only  rewrite only the closure-stamp. For a re-copy that genuinely
                  does not move the resolution: the stamp still has to follow
                  the copy, or the gate is red for a true statement.

For each package it takes every requirement in the `engine` extra, STRIPS the
version constraint, and asks `uv` to resolve the whole set at once against the
package's own base dependencies and `requires-python`. What comes back is the
newest version of each member that is simultaneously compatible with every
other member. Then it writes those versions back over the same package set.

WHAT IT DOES NOT DO, SAID PLAINLY
=================================

It resolves METADATA. Nothing here proves the frozen copy's own imports still
work against the versions it lands on — #677 (anthropic 0.84 -> 1.2.0) resolved
perfectly well and would have changed an API the copy calls. That question is
answered by running the thing, and the two gates that run it are named in the
checklist this tool prints:

  * the `-engine` image build, and
  * `.github/workflows/deepwiki-real-engine.yml`
    (`gh workflow run deepwiki-real-engine.yml`).

A refresh is a PROPOSAL until both are green. The scheduled workflow opens it
as a draft pull request carrying that checklist, and never merges it.

Only fully-pinned closures are rewritten. `services/elitea-inventory`'s `engine`
extra is deliberately a mix of exact pins and ranges, each with paragraphs of
measured reasoning attached (`langsmith`'s h11 bound, and the I8 ledger beside
each exact pin recording what it unlocked), and mechanically
overwriting it would delete the reasons along with the versions. For that shape
this tool REPORTS the drift and refuses to rewrite, which is a smaller lie than
a rewrite that silently discards why a bound is there.

Only the standard library plus `uv`, which it installs at the pinned version if
it is not already present.
"""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

REPOSITORY_ROOT = pathlib.Path(__file__).resolve().parents[2]

# The version services/elitea-inventory/Containerfile installs and
# scripts/ci/check-engine-closures.sh pins. A resolver is a program: "the
# latest one" is not a reproducible answer to "what does this set resolve to".
UV_VERSION = "0.9.5"
UV_PYTHON_VERSION = "3.12"
UV_PLATFORM = "x86_64-manylinux_2_28"

STAMP_PATTERN = re.compile(
    r"^# closure-stamp: COPY_MANIFEST\.json sha256 (?P<digest>[0-9a-f]{64})$",
    re.MULTILINE,
)

#: A requirement line inside an `engine = [ ... ]` block.
REQUIREMENT_LINE = re.compile(
    r'^(?P<indent>\s*)"(?P<name>[A-Za-z0-9._-]+)'
    r'(?P<extras>\[[^\]]*\])?'
    r'(?P<constraint>[^"]*)",\s*$'
)

#: A resolved `name==version` line out of `uv pip compile`.
RESOLVED_LINE = re.compile(r"^(?P<name>[A-Za-z0-9._-]+)==(?P<version>[^\s;]+)")


class Package:
    def __init__(
        self,
        path: str,
        manifest: str,
        extras: list,
        rewritable: bool,
        rewritable_note: str = "",
    ) -> None:
        self.path = REPOSITORY_ROOT / path
        self.name = path
        self.manifest = REPOSITORY_ROOT / manifest
        #: Every extra the image installs. The closure must resolve WITH them,
        #: because that is what the Containerfile asks pip for — resolving the
        #: engine extra alone would miss a conflict against psycopg.
        self.extras = extras
        self.rewritable = rewritable
        self.rewritable_note = rewritable_note

    @property
    def pyproject(self) -> pathlib.Path:
        return self.path / "pyproject.toml"


PACKAGES = [
    Package(
        "services/elitea-deepwiki",
        "services/elitea-deepwiki/src/elitea_deepwiki/engine/COPY_MANIFEST.json",
        ["engine", "storage-postgres"],
        rewritable=True,
    ),
    Package(
        "services/elitea-inventory",
        "services/elitea-inventory/src/elitea_inventory/engine/COPY_MANIFEST.json",
        ["engine"],
        rewritable=False,
        rewritable_note=(
            "its `engine` extra mixes exact pins with ranges, and both kinds "
            "carry paragraphs of measured reasoning that a mechanical rewrite "
            "would delete along with the version: langsmith's h11 bound, and "
            "the I8 ledger saying what each exact pin unlocked. "
            "Drift is reported; the edit is a person's."
        ),
    ),
]


def digest(path: pathlib.Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def uv_command() -> list:
    """Return the argv prefix that runs the pinned uv."""
    found = shutil.which("uv")
    if found:
        version = subprocess.run(
            [found, "--version"], capture_output=True, text=True, check=True
        ).stdout.split()
        if len(version) > 1 and version[1] == UV_VERSION:
            return [found]
    subprocess.run(
        [
            sys.executable,
            "-m",
            "pip",
            "install",
            "--quiet",
            "--disable-pip-version-check",
            f"uv=={UV_VERSION}",
        ],
        check=True,
    )
    return [sys.executable, "-m", "uv"]


def engine_block(text: str) -> tuple:
    """Return (start, end) character offsets of the `engine = [ ... ]` body.

    The body is what lies between the opening bracket's line and the closing
    `]` at column 0. Returns None when the block is not found, which is a
    failure and never a silent skip: a reader that stops reading must say so.
    """
    match = re.search(r"^engine = \[\n", text, re.MULTILINE)
    if match is None:
        return None
    start = match.end()
    end = text.find("\n]\n", start)
    if end == -1:
        return None
    return start, end + 1


def parse_requirements(body: str) -> list:
    """Return [(line, name, extras, constraint)] for each requirement line."""
    parsed = []
    for line in body.splitlines(keepends=True):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        match = REQUIREMENT_LINE.match(line.rstrip("\n"))
        if match is None:
            raise SystemExit(
                "this reader did not understand a requirement line in the "
                "`engine` extra, so it cannot claim to have re-resolved the "
                "whole set:\n    %s" % line.rstrip()
            )
        parsed.append(
            (
                line,
                match.group("name"),
                match.group("extras") or "",
                match.group("constraint"),
            )
        )
    return parsed


def canonical(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name).lower()


def resolve(package: Package, unconstrained: bool, uv: list) -> dict:
    """Resolve the package's extras and return {canonical name: version}.

    With `unconstrained=True` the `engine` extra's own version constraints are
    stripped first, so the answer is the newest SET that still satisfies every
    member's requirements on every other member — which is what "move the whole
    closure" means. Everything else (requires-python, the base dependencies,
    the other extras) is left exactly as it is, because those bounds are the
    ones the service actually ships behind.
    """
    text = package.pyproject.read_text(encoding="utf-8")

    if unconstrained:
        span = engine_block(text)
        if span is None:
            raise SystemExit(
                "%s: no `engine = [` block found; this tool refuses to guess."
                % package.pyproject
            )
        start, end = span
        body = text[start:end]
        rebuilt = []
        for line, name, extras, _constraint in parse_requirements(body):
            indent = line[: len(line) - len(line.lstrip())]
            rebuilt.append('%s"%s%s",\n' % (indent, name, extras))
        text = text[:start] + "".join(rebuilt) + text[end:]

    with tempfile.TemporaryDirectory() as scratch:
        scratch_path = pathlib.Path(scratch)
        (scratch_path / "pyproject.toml").write_text(text, encoding="utf-8")
        # The build backend has to be able to produce metadata for the package
        # itself, and hatchling reads the wheel target's package directory.
        # Symlink the source tree in rather than copying multi-thousand files.
        for entry in ("src", "README.md"):
            source = package.path / entry
            if source.exists():
                (scratch_path / entry).symlink_to(source)
        output = scratch_path / "resolved.txt"
        argv = uv + [
            "pip",
            "compile",
            str(scratch_path / "pyproject.toml"),
            "--python-version",
            UV_PYTHON_VERSION,
            "--python-platform",
            UV_PLATFORM,
            "--no-annotate",
            "--no-header",
            "-o",
            str(output),
        ]
        for extra in package.extras:
            argv += ["--extra", extra]
        completed = subprocess.run(argv, capture_output=True, text=True)
        if completed.returncode != 0:
            raise SystemExit(
                "%s: uv could not resolve %s%s\n%s"
                % (
                    package.name,
                    ", ".join(package.extras),
                    " (unconstrained)" if unconstrained else "",
                    completed.stderr.strip(),
                )
            )
        resolved = {}
        for line in output.read_text(encoding="utf-8").splitlines():
            match = RESOLVED_LINE.match(line.strip())
            if match:
                resolved[canonical(match.group("name"))] = match.group("version")
        return resolved


def rewrite(package: Package, resolved: dict, stamp: str) -> list:
    """Write the resolved versions and the stamp. Return the changed lines."""
    text = package.pyproject.read_text(encoding="utf-8")
    span = engine_block(text)
    start, end = span
    body = text[start:end]

    changes = []
    rebuilt = []
    for line, name, extras, constraint in parse_requirements(body):
        indent = line[: len(line) - len(line.lstrip())]
        version = resolved.get(canonical(name))
        if version is None:
            # The set no longer contains this distribution. That is a real
            # change of the closure (it is what happened to `wcmatch` in #799),
            # so it is reported and the line is dropped rather than kept at a
            # version nothing resolved.
            changes.append("  - %s%s  (no longer in the resolved set)" % (name, constraint))
            continue
        new_line = '%s"%s%s==%s",\n' % (indent, name, extras, version)
        if new_line != line:
            changes.append("  %s %s -> ==%s" % (name, constraint or "(unpinned)", version))
        rebuilt.append(new_line)

    text = text[:start] + "".join(rebuilt) + text[end:]
    text = STAMP_PATTERN.sub(
        "# closure-stamp: COPY_MANIFEST.json sha256 %s" % stamp, text
    )
    package.pyproject.write_text(text, encoding="utf-8")
    return changes


def stamp_only(package: Package) -> bool:
    text = package.pyproject.read_text(encoding="utf-8")
    actual = digest(package.manifest)
    match = STAMP_PATTERN.search(text)
    if match is None:
        raise SystemExit(
            "%s carries no closure-stamp line to rewrite. Add one under the "
            "`engine` extra's comment block:\n"
            "    # closure-stamp: COPY_MANIFEST.json sha256 %s"
            % (package.pyproject, actual)
        )
    if match.group("digest") == actual:
        print("%s: closure-stamp already matches (%s)" % (package.name, actual[:12]))
        return False
    package.pyproject.write_text(
        STAMP_PATTERN.sub(
            "# closure-stamp: COPY_MANIFEST.json sha256 %s" % actual, text
        ),
        encoding="utf-8",
    )
    print(
        "%s: closure-stamp %s -> %s"
        % (package.name, match.group("digest")[:12], actual[:12])
    )
    return True


CHECKLIST = """\
### Acceptance gates for a closure refresh

This is a re-resolution of a frozen engine's dependency closure. It is a
PROPOSAL: everything below resolves, and nothing below has been run.

- [ ] The `-engine` image builds.
      `podman build -f services/elitea-deepwiki/Containerfile \\
          --build-arg EXTRAS="[engine,storage-postgres]" -t elitea-deepwiki:engine .`
      (~2 GB, tens of minutes, needs >35 GB free. No CI job does this, which is
      why it is a checklist item and not a check.)
- [ ] The weekly real-engine run is green ON THIS BRANCH.
      `gh workflow run deepwiki-real-engine.yml --ref <this branch>`
      This is the only gate that runs the frozen copy's own imports against the
      versions this pull request lands on. A resolution says nothing about
      whether `anthropic` still has the method the copy calls (#677 was exactly
      that shape).
- [ ] The engine copy is unchanged, or moved deliberately in the same pull
      request. `python tools/refresh_engine_copy.py --check` in each package.
- [ ] Any advisory this refresh was opened for is actually closed by it —
      check the resolved version against the advisory's fixed-in version, do
      not assume the newest version carries the fix.

Do not merge on green CI alone. CI resolves; it does not build the image and it
does not run the engine.
"""


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("packages", nargs="*", help="package paths; default: all")
    parser.add_argument(
        "--check",
        action="store_true",
        help="report drift, change nothing, exit 3 when the closure would move",
    )
    parser.add_argument(
        "--stamp-only",
        action="store_true",
        help="rewrite only the closure-stamp, for a re-copy that does not move "
        "the resolution",
    )
    parser.add_argument(
        "--summary",
        help="write a markdown summary of the change to this path (for the "
        "scheduled workflow's pull request body)",
    )
    args = parser.parse_args(argv)

    selected = PACKAGES
    if args.packages:
        wanted = {path.rstrip("/") for path in args.packages}
        selected = [p for p in PACKAGES if p.name in wanted]
        unknown = wanted - {p.name for p in PACKAGES}
        if unknown:
            print(
                "::error::unknown package(s): %s. Known: %s"
                % (", ".join(sorted(unknown)), ", ".join(p.name for p in PACKAGES)),
                file=sys.stderr,
            )
            return 1
    if not selected:
        print("::error::no package selected; this run measured nothing", file=sys.stderr)
        return 1

    if args.stamp_only:
        changed = False
        for package in selected:
            changed |= stamp_only(package)
        return 0

    uv = uv_command()
    print("uv %s" % subprocess.run(uv + ["--version"], capture_output=True, text=True).stdout.strip())

    drifted = False
    summary_sections = []

    for package in selected:
        print()
        print("== %s" % package.name)
        current = resolve(package, unconstrained=False, uv=uv)
        newest = resolve(package, unconstrained=True, uv=uv)

        body_span = engine_block(package.pyproject.read_text(encoding="utf-8"))
        if body_span is None:
            print("::error::%s: no `engine = [` block" % package.name, file=sys.stderr)
            return 1
        body = package.pyproject.read_text(encoding="utf-8")[body_span[0] : body_span[1]]
        members = [canonical(name) for _, name, _, _ in parse_requirements(body)]

        moves = []
        for member in members:
            before = current.get(member)
            after = newest.get(member)
            if after is None:
                moves.append((member, before, None))
            elif before != after:
                moves.append((member, before, after))

        if not moves:
            print("  the closure is already at its newest compatible set")
            continue

        drifted = True
        print("  %d member(s) would move:" % len(moves))
        lines = []
        for member, before, after in moves:
            line = "%s %s -> %s" % (member, before or "(absent)", after or "(dropped)")
            lines.append(line)
            print("    " + line)
        summary_sections.append((package, lines))

        if args.check:
            continue

        if not package.rewritable:
            print(
                "  NOT rewritten: %s\n"
                "  The drift above is the report; the edit is a person's."
                % package.rewritable_note
            )
            continue

        stamp = digest(package.manifest)
        changes = rewrite(package, newest, stamp)
        print("  rewrote %d line(s); closure-stamp = %s" % (len(changes), stamp[:12]))

    if args.summary and summary_sections:
        parts = [
            "The frozen engine closures were re-resolved as a SET on %s.\n"
            % datetime.date.today().isoformat()
        ]
        for package, lines in summary_sections:
            parts.append("### `%s`\n" % package.name)
            if not package.rewritable:
                parts.append(
                    "NOT rewritten by the tool — %s\n" % package.rewritable_note
                )
            parts.append("```\n%s\n```\n" % "\n".join(lines))
        parts.append(CHECKLIST)
        pathlib.Path(args.summary).write_text("\n".join(parts), encoding="utf-8")
        print("\nsummary written to %s" % args.summary)

    if args.check:
        if drifted:
            print(
                "\nthe closure is not at its newest compatible set. Re-resolve it "
                "with:\n    python3 scripts/ci/refresh-engine-closure.py"
            )
            return 3
        print("\nboth closures are at their newest compatible set")
        return 0

    if drifted:
        print()
        print(CHECKLIST)
    return 0


if __name__ == "__main__":
    sys.exit(main())
