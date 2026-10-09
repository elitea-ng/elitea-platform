"""Every image build that compiles Rust into a shared target cache compiles the checked-out source.

A BuildKit cache mount is shared by every build on a host: every worktree,
every stage and every image that names the same target. Cargo judges a path
crate fresh by comparing source mtimes with its last compile, and COPY keeps
the build context's mtimes. A context last edited before another checkout's
build therefore looks up to date, and the image ships the other checkout's
binary under a freshly built tag.

The Containerfiles hold the fix: inside the build RUN, under `sharing=locked`,
every file under /src is touched before `cargo build`. This test finds every
RUN that runs `cargo build` with a cache mount other than cargo's own registry
and git caches (however the target directory is named: CARGO_TARGET_DIR inline
or in ENV, --target-dir, or the default ./target) and fails if either half is
missing.
"""

from __future__ import annotations

import os
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
# Cargo's download caches hold immutable, checksummed sources; sharing them is safe.
CARGO_DOWNLOAD_CACHES = {"/usr/local/cargo/registry", "/usr/local/cargo/git"}
_CARGO_BUILD = re.compile(r"\bcargo\b[^&;|]*?\bbuild\b")
_CACHE_MOUNT = re.compile(r"--mount=(\S*\btype=cache\S*)")
_DESTINATION_KEYS = ("target=", "dst=", "destination=")
_SKIP_DIRS = {".git", "node_modules", "target", ".venv", "venv", "__pycache__"}


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


def _destination(mount: str) -> str | None:
    for option in mount.split(","):
        for key in _DESTINATION_KEYS:
            if option.startswith(key):
                return option.removeprefix(key)
    return None


def violations(text: str) -> tuple[int, list[str]]:
    """Count cache-mounted cargo builds in `text` and describe each unsafe one."""
    builds = 0
    problems: list[str] = []
    for run in _run_instructions(text):
        build = _CARGO_BUILD.search(run)
        if build is None:
            continue
        for mount in _CACHE_MOUNT.findall(run):
            destination = _destination(mount)
            if destination is None or destination in CARGO_DOWNLOAD_CACHES:
                continue
            builds += 1
            if "sharing=locked" not in mount.split(","):
                problems.append(f"{destination}: cache mount is not sharing=locked")
            touch_at = run.find(TOUCH)
            if touch_at == -1 or touch_at > build.start():
                problems.append(f"{destination}: local sources are not touched before cargo build")
    return builds, problems


def _containerfiles() -> list[Path]:
    found: list[Path] = []
    for directory, subdirectories, files in os.walk(REPO_ROOT):
        subdirectories[:] = [d for d in subdirectories if d not in _SKIP_DIRS]
        found.extend(
            Path(directory, name)
            for name in files
            if name.startswith(("Containerfile", "Dockerfile"))
        )
    return found


def test_required_containerfiles_rebuild_local_sources() -> None:
    for relative, expected in REQUIRED.items():
        path = REPO_ROOT / relative
        assert path.is_file(), f"{relative} is missing; update REQUIRED"
        builds, problems = violations(path.read_text())
        assert builds == expected, f"{relative}: found {builds} cache-mounted cargo builds, expected {expected}"
        assert problems == [], f"{relative}: {problems}"


def test_no_other_containerfile_builds_rust_into_a_shared_cache_unsafely() -> None:
    containerfiles = _containerfiles()
    assert {str(p.relative_to(REPO_ROOT)) for p in containerfiles} >= REQUIRED.keys()
    for path in containerfiles:
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

    # The target directory named any other way is still a shared target cache.
    env_target = """
ENV CARGO_TARGET_DIR=/cargo-target
RUN --mount=type=cache,dst=/cargo-target cargo build --release
"""
    flag_target = "RUN --mount=type=cache,destination=/t cargo build --target-dir /t\n"
    default_target = "RUN --mount=type=cache,target=/src/target cargo build --release\n"
    for text in (env_target, flag_target, default_target):
        builds, problems = violations(text)
        assert builds == 1 and len(problems) == 2, text

    safe = f"""
RUN --mount=type=cache,target=/usr/local/cargo/registry \\
    --mount=type=cache,target=/cargo-target,sharing=locked \\
    {TOUCH} && \\
    CARGO_TARGET_DIR=/cargo-target cargo build --release
"""
    assert violations(safe) == (1, [])

    # No cargo build, or only cargo's download caches mounted: nothing to share.
    assert violations("RUN --mount=type=cache,target=/root/.cache/go-build go build ./...\n") == (0, [])
    assert violations(
        "RUN --mount=type=cache,target=/usr/local/cargo/registry cargo install cargo-auditable --locked\n"
    ) == (0, [])
    assert violations("RUN cargo build --release\n") == (0, [])
