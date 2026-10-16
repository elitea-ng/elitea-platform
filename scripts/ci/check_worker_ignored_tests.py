#!/usr/bin/env python3
"""Fail when a Rust worker `#[ignore]` test is run by nothing.

An ignored test never runs under a plain `cargo test`. It runs only when a
`cargo test ... -- --ignored` invocation names it, and nothing reports one that
no invocation names: it simply never runs, and reads like a passing test.
Thirty-nine worker tests sat in that state (they needed fixtures no workflow
provisioned). This check makes that state a CI failure.

Every ignored test the worker compiles must be either

  * selected by a `cargo test ... -- --ignored` invocation that CI will really
    execute and whose failure fails the job, or by a
    `scripts/runtime/test_sandbox_ledger.py --test-filter <substring>` run, or
  * matched by an allowlist entry `<name prefix>  # <where it runs instead>`.

"Really execute" is read from the workflow YAML, not from its text:

  * only `jobs.<id>.steps[].run` scripts count; YAML comments, step names and
    shell comment lines inside a script select nothing;
  * a step (or job) whose `if:` is anything but unconditional is not credited
    (`if: false` never runs; `if: failure()` runs only when something else
    already failed); `success()`, `always()` and `!cancelled()` are;
  * a step (or job) with `continue-on-error: true` is not credited, and
    neither is a command whose line tolerates its failure (`cargo test ... ||
    true`): a red test there turns nothing red;
  * cargo's target selection is honoured: `--lib` credits only library unit
    tests, `--test <name>` only that integration test, `--bins`/`--bin` only
    binaries; no selector, `--tests` or `--all-targets` credits every target.
    The test list therefore records which target each name came from.

Inputs:
  --list       `cargo test ... -- --list --ignored 2>&1` output: the cargo
               `Running <target>` headers (stderr) and the `name: test` lines
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

import yaml

# cargo flags that consume the next token, so it is not a test-name filter.
CARGO_VALUE_FLAGS = {
    "-p", "--package", "--features", "-F", "--manifest-path", "-j", "--jobs",
    "--bench", "--example", "--target", "--target-dir",
    "--profile", "--color", "--message-format", "--config", "-Z", "--exclude",
}
# libtest flags that consume the next token.
LIBTEST_VALUE_FLAGS = {"--skip", "--test-threads", "--format", "--color", "-Z", "--logfile"}

IGNORE_ATTRIBUTE = re.compile(r"^\s*#\[ignore\b")
# cargo prints one of these (to stderr) before each test binary's list.
RUNNING = re.compile(r"^\s*Running (?:unittests )?(\S+)")
# Step and job conditions under which a step runs whenever the job does.
UNCONDITIONAL = {"success()", "always()", "!cancelled()"}
# scripts/runtime/test_sandbox_ledger.py provisions Docker fixtures, then runs
# `cargo test ... --lib <--test-filter> -- --ignored` itself.
LEDGER_WRAPPER = re.compile(r"test_sandbox_ledger\.py\b.*--test-filter[ =](\S+)")


@dataclass(frozen=True)
class Test:
    name: str
    target: str  # "lib", "bin:<name>" or "test:<name>"


def target_of(path: str) -> str:
    """Map a cargo `Running` path to a target id."""
    path = path.strip()
    if path in {"src/lib.rs"}:
        return "lib"
    if path.startswith("src/") and path.endswith(".rs"):
        return "bin:" + Path(path).stem
    if path.startswith("tests/") and path.endswith(".rs"):
        return "test:" + Path(path).stem
    return "other:" + path


@dataclass
class Invocation:
    filters: list[str] = field(default_factory=list)
    skips: list[str] = field(default_factory=list)
    exact: bool = False
    # None = every target; otherwise the set of target ids it runs.
    targets: set[str] | None = None
    source: str = ""

    def runs_target(self, target: str) -> bool:
        if self.targets is None:
            return True
        if target in self.targets:
            return True
        return ("bin:*" in self.targets and target.startswith("bin:")) or (
            "test:*" in self.targets and target.startswith("test:"))

    def selects(self, test: Test) -> bool:
        if not self.runs_target(test.target):
            return False
        name = test.name
        if any(skip in name for skip in self.skips):
            return False
        if not self.filters:
            return True
        if self.exact:
            return name in self.filters
        return any(f in name for f in self.filters)


@dataclass
class Uncredited:
    command: str
    reason: str


def logical_lines(text: str) -> list[str]:
    """Join shell `\\` continuations; drop shell comment lines."""
    lines: list[str] = []
    pending = ""
    for raw in text.splitlines():
        stripped = raw.rstrip()
        if not pending and stripped.lstrip().startswith("#"):
            continue
        if stripped.endswith("\\"):
            pending += stripped[:-1] + " "
            continue
        lines.append(pending + stripped)
        pending = ""
    if pending:
        lines.append(pending)
    return lines


def condition(value: object) -> str:
    """Normalise an `if:` value; '' means no condition."""
    if value is None:
        return ""
    text = str(value).strip()
    if text.startswith("${{") and text.endswith("}}"):
        text = text[3:-2].strip()
    return text.replace(" ", "")


def credited_step(step: dict, job: dict) -> str | None:
    """None when the step's commands count; otherwise why they don't."""
    for owner, scope in ((job, "job"), (step, "step")):
        if owner.get("continue-on-error") not in (None, False, "false"):
            return f"{scope} has continue-on-error, so a failure turns nothing red"
        guard = condition(owner.get("if"))
        if guard and guard not in UNCONDITIONAL:
            return f"{scope} runs only `if: {owner.get('if')}`"
    return None


# The crate whose ignored tests this check counts. A cargo run in another
# package runs none of them.
WORKER_PACKAGE = "elitea-worker-rust"
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
SEPARATORS = {"&&", "||", ";", "|", "&"}


def commands(line: str, source: str) -> list[tuple[list[str], bool]]:
    """Split one shell line into (words, tolerated) commands, comments dropped.

    `tolerated` is true when the command's failure is swallowed by a following
    `|| ...`. Pipes keep failing (the runner's bash uses -o pipefail).
    """
    lexer = shlex.shlex(line, posix=True, punctuation_chars="|&;")
    lexer.whitespace_split = True
    lexer.commenters = "#"
    try:
        tokens = list(lexer)
    except ValueError as error:
        raise SystemExit(f"{source}: cannot parse {line.strip()!r}: {error}") from error
    found: list[tuple[list[str], bool]] = []
    words: list[str] = []
    for token in tokens + [";"]:
        if token in SEPARATORS or set(token) <= set("|&;"):
            if words:
                found.append((words, False))
                words = []
            if token == "||" and found:
                found[-1] = (found[-1][0], True)
            continue
        words.append(token)
    return found


def program(words: list[str]) -> list[str]:
    """The command's own words: leading VAR=value assignments removed."""
    index = 0
    while index < len(words) and ASSIGNMENT.match(words[index]):
        index += 1
    return words[index:]


def parse_command(line: str, source: str) -> tuple[list[Invocation], list[Uncredited]]:
    found: list[Invocation] = []
    refused: list[Uncredited] = []
    for words, tolerated in commands(line, source):
        argv = program(words)
        text = " ".join(words)
        if not argv:
            continue
        # The ledger wrapper, run directly or by a Python interpreter.
        script = argv[1] if len(argv) > 1 and Path(argv[0]).name.startswith("python") else argv[0]
        if Path(script).name == "test_sandbox_ledger.py":
            wrapper = LEDGER_WRAPPER.search(" ".join(argv))
            if not wrapper:
                continue
            if tolerated:
                refused.append(Uncredited(text, "its failure is tolerated (`||`)"))
            else:
                found.append(Invocation(filters=[wrapper.group(1)], targets={"lib"},
                                        source=f"{source}: {text}"))
            continue
        if "cargo" in argv and "--ignored" in argv and argv[0] != "cargo":
            refused.append(Uncredited(text, f"`{argv[0]}` is the command, not cargo"))
            continue
        if argv[:2] != ["cargo", "test"]:
            continue
        invocation, why_not = parse_cargo_test(argv[2:], f"{source}: {text}")
        if invocation is None and why_not is None:
            continue
        if why_not:
            refused.append(Uncredited(text, why_not))
        elif tolerated:
            refused.append(Uncredited(text, "its failure is tolerated (`||`)"))
        else:
            found.append(invocation)
    return found, refused


def parse_cargo_test(tokens: list[str], source: str) -> tuple[Invocation | None, str | None]:
    """Parse `cargo test` arguments. (None, None): not an ignored-test run."""
    if any("${{" in token for token in tokens):
        return None, ("a GitHub expression (e.g. `${{ matrix.* }}`) chooses its filters at run "
                      "time; name the tests literally so this check can see them")
    invocation = Invocation(source=source)
    cargo_names = 0
    libtest = False
    targets: set[str] = set()
    every_target = False
    listing = False
    ignored = False
    packages: list[str] = []
    no_run = False
    index = 0
    while index < len(tokens):
        token = tokens[index]
        value = None
        if "=" in token and token.startswith("--"):
            token, value = token.split("=", 1)

        def operand() -> str:
            nonlocal index
            if value is not None:
                return value
            index += 1
            if index >= len(tokens):
                raise SystemExit(f"{source}: `{token}` takes a value")
            return tokens[index]

        if not libtest and token == "--":
            libtest = True
        elif libtest and token == "--skip":
            invocation.skips.append(operand())
        elif libtest and token == "--exact":
            invocation.exact = True
        elif libtest and token == "--list":
            listing = True
        elif libtest and token in {"--ignored", "--include-ignored"}:
            ignored = True
        elif not libtest and token == "--no-run":
            no_run = True
        elif not libtest and token in {"-p", "--package"}:
            packages.append(operand())
        elif not libtest and token == "--lib":
            targets.add("lib")
        elif not libtest and token == "--bins":
            targets.add("bin:*")
        elif not libtest and token == "--tests":
            targets.update({"lib", "bin:*", "test:*"})
        elif not libtest and token == "--all-targets":
            every_target = True
        elif not libtest and token in {"--test", "--bin"}:
            targets.add(("test:" if token == "--test" else "bin:") + operand())
        elif token in (LIBTEST_VALUE_FLAGS if libtest else CARGO_VALUE_FLAGS):
            operand()
        elif token.startswith("-"):
            pass
        else:
            invocation.filters.append(token)
            cargo_names += 0 if libtest else 1
        index += 1
    if cargo_names > 1:
        # cargo refuses this ("unexpected argument"), so it selects nothing.
        raise SystemExit(f"{source}: cargo takes one test name before `--`; put the "
                         "other filters after it")
    # `-- --list --ignored` enumerates the tests (this check's own input); it runs none.
    if listing or not ignored:
        return None, None
    if no_run:
        return None, "`--no-run` builds the tests and runs none"
    others = [name for name in packages if name != WORKER_PACKAGE]
    if others:
        return None, f"`-p {others[0]}` runs another crate's tests, not {WORKER_PACKAGE}'s"
    invocation.targets = None if every_target or not targets else targets
    return invocation, None


def parse_workflow(text: str, source: str) -> tuple[list[Invocation], list[Uncredited]]:
    document = yaml.safe_load(text) or {}
    jobs = document.get("jobs") if isinstance(document, dict) else None
    if not isinstance(jobs, dict):
        raise SystemExit(f"{source}: no `jobs:` mapping; not a workflow")
    found: list[Invocation] = []
    refused: list[Uncredited] = []
    for job_id, job in jobs.items():
        for number, step in enumerate((job or {}).get("steps") or [], 1):
            script = step.get("run") if isinstance(step, dict) else None
            if not isinstance(script, str):
                continue
            where = f"{source}: jobs.{job_id}.steps[{number}] ({step.get('name', 'unnamed')})"
            why_not = credited_step(step, job)
            for line in logical_lines(script):
                invocations, uncredited = parse_command(line, where)
                if why_not:
                    refused += [Uncredited(i.source, why_not) for i in invocations]
                    refused += [Uncredited(f"{where}: {u.command}", why_not) for u in uncredited]
                    continue
                found += invocations
                refused += [Uncredited(f"{where}: {u.command}", u.reason) for u in uncredited]
    return found, refused


def parse_list(text: str) -> list[Test]:
    """Names with the target they were listed under; unlabelled names are the lib."""
    tests: set[Test] = set()
    target = "lib"
    for line in text.splitlines():
        running = RUNNING.match(line)
        if running:
            target = target_of(running.group(1))
        elif line.endswith(": test"):
            tests.add(Test(line[: -len(": test")], target))
    return sorted(tests, key=lambda t: (t.target, t.name))


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


def label(test: Test) -> str:
    return test.name if test.target == "lib" else f"{test.name} [{test.target}]"


def check(tests: list[Test], invocations: list[Invocation], allowlist: list[tuple[str, str]],
          source_count: int | None) -> list[str]:
    errors: list[str] = []
    if source_count is not None and source_count != len(tests):
        errors.append(
            f"the sources carry {source_count} #[ignore] attribute(s) but the test list names "
            f"{len(tests)} ignored test(s): the list is empty, truncated or built without "
            "--all-features, so it cannot vouch for anything"
        )
    used: set[str] = set()
    for test in tests:
        if any(invocation.selects(test) for invocation in invocations):
            continue
        matched = [prefix for prefix, _ in allowlist if test.name.startswith(prefix)]
        if matched:
            used.update(matched)
            continue
        errors.append(
            f"{label(test)} is #[ignore] and no CI `cargo test ... -- --ignored` invocation "
            "selects it; run it in CI, delete it, or allowlist it with where it runs instead"
        )
    for prefix, _ in allowlist:
        if prefix not in used and not any(test.name.startswith(prefix) for test in tests):
            errors.append(f"allowlist entry {prefix} matches no ignored test; remove it")
    return errors


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    parser.add_argument("--list", required=True, type=Path)
    parser.add_argument("--workflow", required=True, type=Path, action="append")
    parser.add_argument("--allowlist", required=True, type=Path)
    parser.add_argument("--sources", type=Path)
    args = parser.parse_args(argv)

    tests = parse_list(args.list.read_text())
    invocations: list[Invocation] = []
    refused: list[Uncredited] = []
    for workflow in args.workflow:
        found, skipped = parse_workflow(workflow.read_text(), str(workflow))
        invocations += found
        refused += skipped
    allowlist, errors = parse_allowlist(args.allowlist.read_text(), str(args.allowlist))
    source_count = count_ignore_attributes(args.sources) if args.sources else None
    errors += check(tests, invocations, allowlist, source_count)
    if not invocations:
        errors.append("no `cargo test ... -- --ignored` invocation found in the workflow(s)")
    for item in refused:
        print(f"::notice::not credited ({item.reason}): {item.command}")
    for error in errors:
        print(f"::error::{error}")
    selected = [t for t in tests if any(i.selects(t) for i in invocations)]
    allowed = [t for t in tests if t not in selected
               and any(t.name.startswith(p) for p, _ in allowlist)]
    print(f"{len(tests)} ignored test(s): {len(selected)} run by {len(invocations)} CI "
          f"invocation(s), {len(allowed)} allowlisted, "
          f"{len(tests) - len(selected) - len(allowed)} run by nothing; "
          f"{len(refused)} invocation(s) not credited; {len(errors)} error(s)")
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
