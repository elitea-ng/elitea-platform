"""Every image build that compiles Rust into a shared target cache compiles the checked-out source.

A BuildKit cache mount is shared by every build on a host: every worktree,
every stage and every image that names the same target. Cargo judges a path
crate fresh by comparing source mtimes with its last compile, and COPY keeps
the build context's mtimes. A context last edited before another checkout's
build therefore looks up to date, and the image ships the other checkout's
binary under a freshly built tag.

The Containerfiles hold the fix: inside the build RUN, under `sharing=locked`,
every file under /src is touched before `cargo build`. This test finds every
RUN that builds with CARGO_TARGET_DIR on a cache mount and fails if either half
is missing, so a new Rust image cannot reintroduce the stale-binary build.
"""

from __future__ import annotations

import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]

# Every image known to compile Rust into a cache-mounted target dir. A rename
# FAILS here rather than making the scan below vacuous.
REQUIRED = {
    "services/elitea-worker-rust/Containerfile": 2,  # worker + supervisor stages
    "services/elitea-deepwiki-engine/Containerfile": 1,
    "services/elitea-code-runner/Containerfile": 1,
}

TOUCH = "find /src -type f -exec touch {} +"
_TARGET_DIR = re.compile(r"CARGO_TARGET_DIR=(\S+)\s+cargo\b[^&]*\bbuild\b")
_CACHE_MOUNT = re.compile(r"--mount=(\S*\btype=cache\S*)")


def _run_instructions(text: str) -> list[str]:
    """RUN instructions with their backslash continuations joined."""
    runs: list[str] = []
    current: list[str] | None = None
    for line in text.splitlines():
        stripped = line.strip()
        if current is None:
            if not stripped.upper().startswith("RUN "):
                continue
            current = []
        if stripped.startswith("#"):
            continue
        current.append(stripped.removesuffix("\\").strip())
        if not stripped.endswith("\\"):
            runs.append(" ".join(current))
            current = None
    return runs


def violations(text: str) -> tuple[int, list[str]]:
    """Count cache-mounted cargo builds in `text` and describe each unsafe one."""
    builds = 0
    problems: list[str] = []
    for run in _run_instructions(text):
        target = _TARGET_DIR.search(run)
        if target is None:
            continue
        target_dir = target.group(1)
        mount = next(
            (
                m
                for m in _CACHE_MOUNT.findall(run)
                if f"target={target_dir}" in m.split(",")
            ),
            None,
        )
        if mount is None:
            continue
        builds += 1
        if "sharing=locked" not in mount.split(","):
            problems.append(f"{target_dir}: cache mount is not sharing=locked")
        touch_at = run.find(TOUCH)
        if touch_at == -1 or touch_at > target.start():
            problems.append(f"{target_dir}: local sources are not touched before cargo build")
    return builds, problems


def test_required_containerfiles_rebuild_local_sources() -> None:
    for relative, expected in REQUIRED.items():
        path = REPO_ROOT / relative
        assert path.is_file(), f"{relative} is missing; update REQUIRED"
        builds, problems = violations(path.read_text())
        assert builds == expected, f"{relative}: found {builds} cache-mounted cargo builds, expected {expected}"
        assert problems == [], f"{relative}: {problems}"


def test_no_other_containerfile_builds_rust_into_a_shared_cache_unsafely() -> None:
    skip = {".git", "node_modules", "target", ".venv", "venv"}
    for path in REPO_ROOT.rglob("*"):
        if skip.intersection(path.relative_to(REPO_ROOT).parts) or not path.is_file():
            continue
        if not (path.name.startswith("Containerfile") or path.name.startswith("Dockerfile")):
            continue
        _, problems = violations(path.read_text(errors="replace"))
        assert problems == [], f"{path.relative_to(REPO_ROOT)}: {problems}"


def test_checker_rejects_the_unsafe_shapes() -> None:
    unsafe = """
RUN --mount=type=cache,target=/usr/local/cargo/registry \\
    --mount=type=cache,target=/cargo-target \\
    CARGO_TARGET_DIR=/cargo-target cargo auditable build --locked --release
"""
    assert violations(unsafe) == (
        1,
        [
            "/cargo-target: cache mount is not sharing=locked",
            "/cargo-target: local sources are not touched before cargo build",
        ],
    )

    touched_after = f"""
RUN --mount=type=cache,target=/cargo-target,sharing=locked \\
    CARGO_TARGET_DIR=/cargo-target cargo build --release && \\
    {TOUCH}
"""
    assert violations(touched_after)[1] == [
        "/cargo-target: local sources are not touched before cargo build"
    ]

    safe = f"""
RUN --mount=type=cache,target=/cargo-target,sharing=locked \\
    {TOUCH} && \\
    CARGO_TARGET_DIR=/cargo-target cargo build --release
"""
    assert violations(safe) == (1, [])

    # A build without a cache-mounted target dir has nothing to share.
    assert violations("RUN cargo build --release\n") == (0, [])
