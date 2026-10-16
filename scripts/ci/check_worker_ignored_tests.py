#!/usr/bin/env python3
"""Fail when a Rust worker `#[ignore]` test is run by nothing.

An ignored test never runs under a plain `cargo test`. It runs only when a
`cargo test ... -- --ignored` invocation names it, and nothing reports one that
no invocation names: it simply never runs, and reads like a passing test.
Six sandbox ledger tests sat in that state (they needed fixtures no workflow
provisioned). This check makes that state a CI failure.

Every ignored test the worker compiles must be either

  * selected by a `cargo test ... -- --ignored` invocation in the workflow
    (libtest semantics: positional filters are substrings, or exact names
    under `--exact`; `--skip` substrings exclude) or by a
    `scripts/runtime/test_sandbox_ledger.py --test-filter <substring>` run, or
  * matched by an allowlist entry `<name prefix>  # <where it runs instead>`.

Inputs:
  --list       output of `cargo test ... -- --list --ignored` (lines `name: test`)
  --workflow   the workflow whose invocations count (repeatable)
  --allowlist  the allowlist file
  --sources    the crate root; its `#[ignore` attributes are counted and must
               equal the listed count, so an empty or truncated list cannot
               pass as "nothing to check".
"""
from __future__ import annotations

import argparse
import re
import shlex
import sys
from dataclasses import dataclass, field
from pathlib import Path

# cargo flags that consume the next token, so it is not a test-name filter.
CARGO_VALUE_FLAGS = {
    "-p", "--package", "--features", "-F", "--manifest-path", "-j", "--jobs",
    "--test", "--bench", "--bin", "--example", "--target", "--target-dir",
    "--profile", "--color", "--message-format", "--config", "-Z", "--exclude",
}
# libtest flags that consume the next token.
LIBTEST_VALUE_FLAGS = {"--skip", "--test-threads", "--format", "--color", "-Z", "--logfile"}

IGNORE_ATTRIBUTE = re.compile(r"^\s*#\[ignore\b")


@dataclass
class Invocation:
    filters: list[str] = field(default_factory=list)
    skips: list[str] = field(default_factory=list)
    exact: bool = False
    source: str = ""

    def selects(self, name: str) -> bool:
        if any(skip in name for skip in self.skips):
            return False
        if not self.filters:
            return True
        if self.exact:
            return name in self.filters
        return any(f in name for f in self.filters)


def logical_lines(text: str) -> list[str]:
    """Join shell `\\` continuations so one command is one line."""
    lines: list[str] = []
    pending = ""
    for raw in text.splitlines():
        stripped = raw.rstrip()
        if stripped.endswith("\\"):
            pending += stripped[:-1] + " "
            continue
        lines.append(pending + stripped)
        pending = ""
    if pending:
        lines.append(pending)
    return lines


# scripts/runtime/test_sandbox_ledger.py provisions Docker fixtures, then runs
# `cargo test ... --lib <--test-filter> -- --ignored` itself.
LEDGER_WRAPPER = re.compile(r"test_sandbox_ledger\.py\b.*--test-filter[ =](\S+)")


def parse_invocations(text: str, source: str) -> list[Invocation]:
    found: list[Invocation] = []
    for line in logical_lines(text):
        wrapper = LEDGER_WRAPPER.search(line)
        if wrapper:
            found.append(Invocation(filters=[wrapper.group(1)], source=f"{source}: {line.strip()}"))
            continue
        if "cargo test" not in line or "--ignored" not in line:
            continue
        command = line[line.index("cargo test"):]
        # A YAML `run: cargo test ...` line; the shell comment, if any, is not part of it.
        command = command.split(" #", 1)[0]
        try:
            tokens = shlex.split(command)
        except ValueError as error:
            raise SystemExit(f"{source}: cannot parse {command!r}: {error}") from error
        tokens = tokens[2:]
        invocation = Invocation(source=f"{source}: {command.strip()}")
        cargo_names = 0
        libtest = False
        index = 0
        while index < len(tokens):
            token = tokens[index]
            if token in {"&&", "||", ";", "|"}:
                break
            if not libtest and token == "--":
                libtest = True
            elif libtest and token == "--skip":
                index += 1
                invocation.skips.append(tokens[index])
            elif libtest and token == "--exact":
                invocation.exact = True
            elif token in (LIBTEST_VALUE_FLAGS if libtest else CARGO_VALUE_FLAGS):
                index += 1
            elif token.startswith("-"):
                pass
            else:
                invocation.filters.append(token)
                cargo_names += 0 if libtest else 1
            index += 1
        if cargo_names > 1:
            # cargo refuses this ("unexpected argument"), so it selects nothing.
            raise SystemExit(f"{source}: cargo takes one test name before `--`; put the "
                             f"other filters after it: {command.strip()}")
        # `-- --list --ignored` enumerates the tests (this check's own input); it runs none.
        if "--list" in tokens:
            continue
        if "--ignored" in tokens or "--include-ignored" in tokens:
            found.append(invocation)
    return found


def parse_list(text: str) -> list[str]:
    return sorted({line[: -len(": test")] for line in text.splitlines() if line.endswith(": test")})


def parse_allowlist(text: str, source: str) -> tuple[list[tuple[str, str]], list[str]]:
    entries: list[tuple[str, str]] = []
    errors: list[str] = []
    for number, raw in enumerate(text.splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        prefix, _, reason = line.partition("#")
        prefix, reason = prefix.strip(), reason.strip()
        if not prefix or " " in prefix:
            errors.append(f"{source}:{number}: expected '<test name prefix>  # <where it runs>'")
        elif len(reason) < 20:
            errors.append(f"{source}:{number}: {prefix}: the reason must say where the test runs instead")
        else:
            entries.append((prefix, reason))
    return entries, errors


def count_ignore_attributes(root: Path) -> int:
    count = 0
    for directory in ("src", "tests", "benches"):
        for path in sorted((root / directory).rglob("*.rs")):
            count += sum(1 for line in path.read_text().splitlines() if IGNORE_ATTRIBUTE.match(line))
    return count


def check(names: list[str], invocations: list[Invocation], allowlist: list[tuple[str, str]],
          source_count: int | None) -> list[str]:
    errors: list[str] = []
    if source_count is not None and source_count != len(names):
        errors.append(
            f"the sources carry {source_count} #[ignore] attribute(s) but the test list names "
            f"{len(names)} ignored test(s): the list is empty, truncated or built without "
            "--all-features, so it cannot vouch for anything"
        )
    used: set[str] = set()
    for name in names:
        if any(invocation.selects(name) for invocation in invocations):
            continue
        matched = [prefix for prefix, _ in allowlist if name.startswith(prefix)]
        if matched:
            used.update(matched)
            continue
        errors.append(
            f"{name} is #[ignore] and no CI `cargo test ... -- --ignored` invocation selects it; "
            "run it in CI, delete it, or allowlist it with where it runs instead"
        )
    for prefix, _ in allowlist:
        if prefix not in used and not any(name.startswith(prefix) for name in names):
            errors.append(f"allowlist entry {prefix} matches no ignored test; remove it")
    return errors


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--list", required=True, type=Path)
    parser.add_argument("--workflow", required=True, type=Path, action="append")
    parser.add_argument("--allowlist", required=True, type=Path)
    parser.add_argument("--sources", type=Path)
    args = parser.parse_args(argv)

    names = parse_list(args.list.read_text())
    invocations: list[Invocation] = []
    for workflow in args.workflow:
        invocations += parse_invocations(workflow.read_text(), str(workflow))
    allowlist, errors = parse_allowlist(args.allowlist.read_text(), str(args.allowlist))
    source_count = count_ignore_attributes(args.sources) if args.sources else None
    errors += check(names, invocations, allowlist, source_count)
    if not invocations:
        errors.append("no `cargo test ... -- --ignored` invocation found in the workflow(s)")
    for error in errors:
        print(f"::error::{error}")
    selected = [n for n in names if any(i.selects(n) for i in invocations)]
    allowed = [n for n in names if n not in selected and any(n.startswith(p) for p, _ in allowlist)]
    print(f"{len(names)} ignored test(s): {len(selected)} run by {len(invocations)} CI "
          f"invocation(s), {len(allowed)} allowlisted, "
          f"{len(names) - len(selected) - len(allowed)} run by nothing; {len(errors)} error(s)")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
