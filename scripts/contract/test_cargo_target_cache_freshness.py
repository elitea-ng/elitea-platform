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

The images CI builds for the E2E stack moved their dependency build out of
the shared mount and into a layer (libs/rust/scripts/cargo-dependency-skeleton.py):
a stage copies a manifests-only skeleton, builds every registry crate from it,
then copies the real sources over it and builds again. The same mtime hazard
applies one step down — a context older than the placeholder build would
look fresh and the image would ship the placeholder — so the SECOND build in
such a stage must touch /src first. `REQUIRED_SKELETON` holds those images.
"""

from __future__ import annotations

import os
import re
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]

# Every image known to compile Rust into a cache-mounted target dir. A rename
# FAILS here rather than making the scan below vacuous.
REQUIRED = {
    "services/elitea-worker-rust/Containerfile": 1,  # the supervisor stage
    "services/elitea-code-runner/Containerfile": 1,
}

# Every image whose builder compiles over a dependency skeleton, with the
# number of real (post-skeleton) builds. A rename or a regression to the old
# shape FAILS here.
REQUIRED_SKELETON = {
    "services/elitea-worker-rust/Containerfile": 1,
    "services/elitea-deepwiki-engine/Containerfile": 1,
    "services/elitea-inventory-engine/Containerfile": 1,
    "services/elitea-vector/Containerfile": 1,
    "services/elitea-doc-extractor/Containerfile": 1,
}
SKELETON_COPY = re.compile(r"^COPY\s+--from=\S+\s+/skeleton/?\s")

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


def _instructions(text: str) -> list[str]:
    """Every instruction, continuations joined, comments dropped."""
    out: list[str] = []
    current: list[str] | None = None
    for line in text.splitlines():
        stripped = line.strip()
        if stripped.startswith("#") or (current is None and not stripped):
            continue
        if current is None:
            current = []
        current.append(stripped.removesuffix("\\").strip())
        if not stripped.endswith("\\"):
            out.append(" ".join(current))
            current = None
    return out


def skeleton_violations(text: str) -> tuple[int, list[str]]:
    """Count post-skeleton cargo builds in `text` and describe each unsafe one.

    Within a stage that copies a /skeleton, the first cargo build is the
    dependency build; every later one compiles the real sources and must
    touch /src before it builds.
    """
    real_builds = 0
    problems: list[str] = []
    in_skeleton_stage = False
    builds_after_skeleton = 0
    for instruction in _instructions(text):
        keyword = instruction.split(None, 1)[0].upper()
        if keyword == "FROM":
            in_skeleton_stage = False
            builds_after_skeleton = 0
            continue
        if keyword == "COPY" and SKELETON_COPY.match(instruction):
            in_skeleton_stage = True
            builds_after_skeleton = 0
            continue
        if keyword != "RUN" or not in_skeleton_stage:
            continue
        build = _CARGO_BUILD.search(instruction)
        if build is None:
            continue
        builds_after_skeleton += 1
        if builds_after_skeleton == 1:
            continue
        real_builds += 1
        touch_at = instruction.find(TOUCH)
        if touch_at == -1 or touch_at > build.start():
            problems.append("a build over the dependency skeleton does not touch local sources before cargo build")
    return real_builds, problems


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


def test_skeleton_builds_rebuild_local_sources() -> None:
    for relative, expected in REQUIRED_SKELETON.items():
        path = REPO_ROOT / relative
        assert path.is_file(), f"{relative} is missing; update REQUIRED_SKELETON"
        builds, problems = skeleton_violations(path.read_text())
        assert builds == expected, f"{relative}: found {builds} builds over a dependency skeleton, expected {expected}"
        assert problems == [], f"{relative}: {problems}"
    for path in _containerfiles():
        _, problems = skeleton_violations(path.read_text(errors="replace"))
        assert problems == [], f"{path.relative_to(REPO_ROOT)}: {problems}"


def test_skeleton_checker_rejects_an_untouched_build() -> None:
    unsafe = """
FROM toolchain AS builder
COPY --from=planner /skeleton/ /src/
RUN CARGO_TARGET_DIR=/t cargo auditable build --locked --release
COPY services/x ./
RUN CARGO_TARGET_DIR=/t cargo auditable build --locked --release
"""
    assert skeleton_violations(unsafe) == (
        1,
        ["a build over the dependency skeleton does not touch local sources before cargo build"],
    )
    head, last = unsafe.rsplit("RUN ", 1)
    safe = f"{head}RUN {TOUCH} && {last}"
    assert skeleton_violations(safe) == (1, [])
    # The dependency build itself needs no touch, and a new stage resets.
    assert skeleton_violations(unsafe.split("COPY services")[0]) == (0, [])


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
