#!/usr/bin/env python3
"""One Rust version everywhere: cargo, CI and images (PR #1297).

The implementation behind scripts/ci/check-rust-toolchain.sh; see that file
for the assertions and HOW TO RAISE THE VERSION. Reads only tracked files
(`git ls-files`), needs no network and no toolchain. Every read that comes
back empty FAILS: a comparison against nothing must not pass.
"""
from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

import yaml

# The image index digest of rust:<pin>-trixie (all platforms). Moves with the pin.
RUST_IMAGE_DIGEST = "sha256:b1b3c9c0d921d7fa0a6d1f9ec7e4eab87f8c8ec97644c3d791450f131dec813f"
# The minimum supported Rust version of our own crates: the documented floor,
# deliberately below the pin (see the PR #1297 body). Must be <= the pin.
MSRV = "1.95"

MIN_TOOLCHAIN_FILES = 9
MIN_CRATES = 30
MIN_RELEASE_LITERALS = 6

# Files that MUST reference the rust image (b). Scanning is exhaustive, so a
# floor would only catch a pattern that stopped matching; this list catches
# that per file instead, and a NEW file that references rust is reported too
# (it must be added here, deliberately, or stop referencing rust).
RUST_IMAGE_FILES = {
    "services/elitea-code-runner/Containerfile": 2,
    "services/elitea-deepwiki-engine/Containerfile": 2,
    "services/elitea-doc-extractor/Containerfile": 2,
    "services/elitea-inventory-engine/Containerfile": 2,
    "services/elitea-vector/Containerfile": 2,
    "services/elitea-worker-rust/Containerfile": 2,
}

# Cargo.toml files no Rust toolchain of ours builds as a crate with an MSRV.
EXEMPT_CRATES = {
    "libs/rust/office-core/fuzz/Cargo.toml": "cargo-fuzz harness built only by nightly cargo-fuzz, never by CI or an image",
    "services/elitea-code-runner/adapters/rust/Cargo.toml": "template compiled inside the sandbox for user code (include_str! in src/rust_profile.rs); not a crate of ours",
    "scripts/runtime/fixtures/code-data-processing/Cargo.toml": "locked acceptance fixture of user code; its manifest is a test input",
}

# (e) Each validator's COMPARISON line, which must carry the pinned release.
# A literal that moves (to a const, a helper, another line) leaves this
# pattern unmatched, so the move fails instead of passing on test data.
VERSION = r"['\"]?(\d+\.\d+\.\d+)['\"]?"
CHECKER_LINES = {
    "services/elitea-worker-rust/src/sandbox/cargo_dependency_bundle.rs":
        re.compile(r"\|\|\s*self\.profile\.rust_revision\s*!=\s*" + VERSION),
    "services/elitea-main/internal/infra/storage/sandbox_native_bundle.go":
        re.compile(r"p\.Profile\.Rust\s*!=\s*" + VERSION),
    "scripts/runtime/rust_compiled_release_manifest.py":
        re.compile(r"^RUST_RELEASE\s*=\s*" + VERSION + r"\s*$"),
}
# The audit must USE that constant on both of its checks.
AUDIT_USES = ('profile["rust_revision"] == RUST_RELEASE', 'f"release: {RUST_RELEASE}\\n"')

# (e) A release literal anywhere: the value next to rust_revision /
# Profile.Rust / RUST_RELEASE in any quoting (or none), on either side of the
# comparison, and the `release: X.Y.Z` / `rustc X.Y.Z` lines of rustc's output.
V = r"['\"]?(?P<v>\d+\.\d+\.\d+)['\"]?"
RELEASE_LITERALS = [
    re.compile(r"\brust_revision['\"]?\]?\s*(?:[:=]|==|!=)\s*" + V),
    re.compile(r"\bProfile\.Rust\s*(?:==|!=)\s*" + V),
    re.compile(V + r"\s*(?:==|!=)\s*[\w.]*Profile\.Rust\b"),
    re.compile(V + r"\s*(?:==|!=)\s*[\w.\[\]'\"]*rust_revision"),
    re.compile(r"\bRUST_RELEASE\s*(?:=|==|!=)\s*" + V),
    re.compile(r"release: (?P<v>\d+\.\d+\.\d+)"),
    re.compile(r"\brustc (?P<v>\d+\.\d+\.\d+)"),
]
RELEASE_SKIP = re.compile(r"(\.md|\.lock)$|(^|/)vendor/|^scripts/ci/(check[-_]rust[-_]toolchain|assert-rust-toolchain)")

# (b) The official rust image after ANY registry or path prefix, with or
# without a tag/digest: rust, rust:1, rust@sha256:..., docker.io/library/rust,
# mirror.gcr.io/library/rust:..., ghcr.io/x/rust:... — but not elitea-rustfs,
# rustfs/rustfs, trust:1 or a stage named rust-runtime.
RUST_IMAGE = re.compile(r"^(?:[^\s\"'@:]+/)?rust(?:[:@]\S*)?$")

failures: list[str] = []


def fail(message: str) -> None:
    failures.append(message)
    print(f"ERROR: {message}", file=sys.stderr)


def tracked(*patterns: str) -> list[str]:
    out = subprocess.run(["git", "ls-files", "--", *patterns], capture_output=True, text=True, check=False)
    return sorted({line for line in out.stdout.splitlines() if line})


def version_tuple(value: str) -> tuple[int, ...]:
    parts = [int(p) for p in value.split(".")]
    return tuple(parts + [0] * (3 - len(parts)))


# ── (a) toolchain files ───────────────────────────────────────────────────────
def channel_of(path: str) -> str:
    match = re.search(r'^\s*channel\s*=\s*"([^"]*)"\s*$', Path(path).read_text(), re.MULTILINE)
    return match.group(1) if match else ""


def check_toolchains() -> str:
    if not Path("rust-toolchain.toml").is_file():
        print("ERROR: rust-toolchain.toml is missing at the repository root; there is no pin", file=sys.stderr)
        sys.exit(1)
    pin = channel_of("rust-toolchain.toml")
    if not re.fullmatch(r"\d+\.\d+\.\d+", pin):
        print(f"ERROR: read no exact 'channel = \"X.Y.Z\"' from rust-toolchain.toml (got '{pin}').", file=sys.stderr)
        sys.exit(1)
    print(f"pin: rust {pin} (rust-toolchain.toml)")
    files = tracked("rust-toolchain.toml", "*/rust-toolchain.toml", "rust-toolchain", "*/rust-toolchain")
    if len(files) < MIN_TOOLCHAIN_FILES:
        fail(f"found {len(files)} rust-toolchain file(s), expected at least {MIN_TOOLCHAIN_FILES}; the pattern stopped matching")
    for path in files:
        if not path.endswith(".toml"):
            fail(f"{path}: a legacy rust-toolchain file without .toml; use rust-toolchain.toml")
            continue
        channel = channel_of(path)
        if not channel:
            fail(f"{path}: read no 'channel = \"...\"'")
        elif channel != pin:
            fail(f"{path}: channel {channel}, the pin is {pin}")
        else:
            print(f"  {path}: {channel}")
    return pin


# ── (b) images ────────────────────────────────────────────────────────────────
def logical_lines(text: str) -> list[tuple[int, str]]:
    """Dockerfile lines with `\\` continuations joined; comments dropped."""
    out: list[tuple[int, str]] = []
    pending, start = "", 0
    for number, raw in enumerate(text.splitlines(), 1):
        line = raw.rstrip()
        if not pending and re.match(r"^\s*(#|//)", line):
            continue
        if not pending:
            start = number
        if line.endswith("\\"):
            pending += line[:-1] + " "
            continue
        out.append((start, pending + line))
        pending = ""
    if pending:
        out.append((start, pending))
    return out


def substitute(value: str, args: dict[str, str]) -> str:
    """Expand ${NAME}, ${NAME:-default} and $NAME from ARG defaults."""
    def one(match: re.Match) -> str:
        name = match.group(1) or match.group(3)
        default = match.group(2)
        if name in args and args[name]:
            return args[name]
        return default if default is not None else match.group(0)
    return re.sub(r"\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)", one, value)


def dockerfile_operands(text: str) -> list[tuple[int, str]]:
    """(line, image operand) for FROM, --from=, --mount=...,from= and bake contexts."""
    lines = logical_lines(text)
    stages: set[str] = set()
    for _, line in lines:
        match = re.match(r"^\s*FROM\s+(?:--\S+\s+)*\S+\s+AS\s+(\S+)", line, re.IGNORECASE)
        if match:
            stages.add(match.group(1).lower())
    args: dict[str, str] = {}
    found: list[tuple[int, str]] = []
    for number, line in lines:
        arg = re.match(r"^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)(?:=(\S*))?", line, re.IGNORECASE)
        if arg:
            value = (arg.group(2) or "").strip("\"'")
            args[arg.group(1)] = substitute(value, args)
            continue
        words = line.split()
        if words and words[0].upper() == "FROM":
            rest = [w for w in words[1:] if not w.startswith("--")]
            if rest:
                found.append((number, substitute(rest[0], args)))
        for word in words:
            for match in re.finditer(r"(?:^|,|--)from=([^,\s]+)", word):
                found.append((number, substitute(match.group(1).strip("\"'"), args)))
        for match in re.finditer(r"docker-image://([^\"\s]+)", line):
            found.append((number, substitute(match.group(1), args)))
    return [(n, op.strip("\"'")) for n, op in found if op.strip("\"'").lower() not in stages]


def yaml_operands(text: str) -> list[tuple[int, str]]:
    """(line, value) for every `image:` and `container:` key in a YAML file.

    Every document of a multi-document file is walked. A file that does not
    parse (a Helm template) is read line by line instead, so it is never
    skipped in silence.
    """
    found: list[tuple[int, str]] = []
    try:
        roots = list(yaml.compose_all(text))
    except yaml.YAMLError:
        for number, line in enumerate(text.splitlines(), 1):
            match = re.match(r"^\s*(?:-\s+)?(?:image|container)\s*:\s*['\"]?([^'\"\s#]+)", line)
            if match:
                found.append((number, match.group(1)))
        return found

    def walk(node: yaml.Node) -> None:
        if isinstance(node, yaml.MappingNode):
            for key, value in node.value:
                if isinstance(key, yaml.ScalarNode) and key.value in {"image", "container"} \
                        and isinstance(value, yaml.ScalarNode):
                    found.append((value.start_mark.line + 1, value.value))
                walk(value)
        elif isinstance(node, yaml.SequenceNode):
            for item in node.value:
                walk(item)

    for root in roots:
        if root is not None:
            walk(root)
    return found


def check_images(pin: str) -> None:
    expected = f"rust:{pin}-trixie@{RUST_IMAGE_DIGEST}"
    per_file: dict[str, int] = {}
    for path in tracked("*Containerfile*", "*Dockerfile*", "*.hcl", "*.yml", "*.yaml"):
        text = Path(path).read_text(errors="replace")
        operands = yaml_operands(text) if path.endswith((".yml", ".yaml")) else dockerfile_operands(text)
        for number, image in operands:
            if not RUST_IMAGE.match(image):
                continue
            per_file[path] = per_file.get(path, 0) + 1
            if image != expected:
                fail(f"{path}:{number}: {image}, expected exactly {expected} (no registry prefix; tag and digest both)")
            else:
                print(f"  {path}:{number}: rust:{pin}-trixie@…{RUST_IMAGE_DIGEST[-8:]}")
    for path, count in sorted(per_file.items()):
        if path not in RUST_IMAGE_FILES:
            fail(f"{path}: references the rust image ({count}x) but is not in RUST_IMAGE_FILES; add it there deliberately")
    for path, minimum in sorted(RUST_IMAGE_FILES.items()):
        count = per_file.get(path, 0)
        if count < minimum:
            fail(f"{path}: found {count} rust image reference(s), expected at least {minimum}; the pattern stopped matching or the file moved")


# ── (c) MSRV ──────────────────────────────────────────────────────────────────
def check_msrv(pin: str) -> None:
    if not re.fullmatch(r"\d+\.\d+(\.\d+)?", MSRV):
        fail(f"MSRV '{MSRV}' is not a version")
        return
    if version_tuple(MSRV) > version_tuple(pin):
        fail(f"MSRV {MSRV} is above the pin {pin}; the pinned compiler could not build our crates")
    print(f"MSRV: {MSRV} (floor of our crates; vendored crates keep their own, <= {pin})")

    def value_of(path: str) -> str:
        match = re.search(r'^\s*rust-version\s*=\s*"([^"]*)"\s*$', Path(path).read_text(), re.MULTILINE)
        return match.group(1) if match else ""

    workspace = value_of("libs/rust/Cargo.toml") if Path("libs/rust/Cargo.toml").is_file() else ""
    crates = 0
    for path in tracked("*Cargo.toml"):
        if not (path.endswith("/Cargo.toml") or path == "Cargo.toml" or path.endswith(".Cargo.toml")):
            continue
        text = Path(path).read_text()
        if not re.search(r"^\[package\]", text, re.MULTILINE):
            continue
        crates += 1
        if path in EXEMPT_CRATES:
            print(f"  {path}: exempt")
            continue
        if "/vendor/" in path:
            value = value_of(path)
            if not value:
                fail(f"{path}: vendored crate with no readable rust-version")
            elif version_tuple(value) > version_tuple(pin):
                fail(f"{path}: vendored crate needs rust {value}, above the pin {pin}")
            else:
                print(f"  {path}: {value} (vendored, <= {pin})")
            continue
        if re.search(r"^\s*rust-version\.workspace\s*=\s*true", text, re.MULTILINE):
            if not path.startswith("libs/rust/"):
                fail(f"{path}: inherits rust-version from a workspace this check does not read")
                continue
            value, how = workspace, "inherited from libs/rust"
        else:
            value, how = value_of(path), "stated"
        if not value:
            fail(f"{path}: no readable rust-version (state \"{MSRV}\" or inherit the workspace value)")
        elif value != MSRV:
            fail(f"{path}: rust-version {value} ({how}), the MSRV is {MSRV}")
        else:
            print(f"  {path}: {value} ({how})")
    if crates < MIN_CRATES:
        fail(f"found {crates} crate manifest(s), expected at least {MIN_CRATES}; the pattern stopped matching")
    for path in EXEMPT_CRATES:
        if not Path(path).is_file():
            fail(f"MSRV exemption {path} names no file; remove it")


# ── (e) release literals ──────────────────────────────────────────────────────
def check_release_literals(pin: str) -> int:
    count = 0
    for path in tracked("*"):
        if RELEASE_SKIP.search(path):
            continue
        try:
            text = Path(path).read_text()
        except (UnicodeDecodeError, IsADirectoryError, FileNotFoundError):
            continue
        for number, line in enumerate(text.splitlines(), 1):
            seen: set[int] = set()
            for pattern in RELEASE_LITERALS:
                for match in pattern.finditer(line):
                    if match.start("v") in seen:
                        continue
                    seen.add(match.start("v"))
                    count += 1
                    value = match.group("v")
                    if value != pin:
                        fail(f"{path}:{number}: toolchain release {value}, the pin is {pin} (a native bundle and its validators would disagree with the compiler)")
                    else:
                        print(f"  {path}:{number}: release {value}")
    if count < MIN_RELEASE_LITERALS:
        fail(f"found {count} toolchain release literal(s), expected at least {MIN_RELEASE_LITERALS}; one moved or the pattern stopped matching")
    for path, pattern in CHECKER_LINES.items():
        text = Path(path).read_text() if Path(path).is_file() else ""
        hits = [(n, m) for n, line in enumerate(text.splitlines(), 1) for m in [pattern.search(line)] if m]
        if not hits:
            fail(f"{path}: carries no toolchain release literal on its comparison line; the validator moved, so this check cannot see what it compares")
        for number, match in hits:
            if match.group(1) != pin:
                fail(f"{path}:{number}: the comparison checks release {match.group(1)}, the pin is {pin}")
    audit = Path("scripts/runtime/rust_compiled_release_manifest.py")
    if audit.is_file():
        source = audit.read_text()
        for use in AUDIT_USES:
            if use not in source:
                fail(f"{audit}: no longer checks `{use}`; the RUST_RELEASE constant is not what it compares")
    return count


def main() -> int:
    root = os.environ.get("RUST_TOOLCHAIN_ROOT") or str(Path(__file__).resolve().parents[2])
    os.chdir(root)
    if not tracked():
        print(f"ERROR: {root} is not a git work tree with tracked files; there is nothing to check", file=sys.stderr)
        return 1
    pin = check_toolchains()
    check_images(pin)
    check_msrv(pin)
    literals = check_release_literals(pin)
    if failures:
        print(f"\n{len(failures)} Rust toolchain disagreement(s). See 'HOW TO RAISE THE VERSION' in "
              "scripts/ci/check-rust-toolchain.sh.", file=sys.stderr)
        return 1
    print(f"One Rust everywhere: rust {pin}, image digest …{RUST_IMAGE_DIGEST[-12:]}, MSRV {MSRV}, "
          f"{literals} release literal(s).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
