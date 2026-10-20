#!/usr/bin/env python3
"""Write the dependency skeleton of one Rust service: its manifests, no sources.

    cargo-dependency-skeleton.py <service-dir> <out-dir>

The Rust images build in two steps so that a source-only change does not
recompile ~400 registry crates (see services/elitea-worker-rust/Containerfile):

  1. a dependency layer, built from this skeleton alone — every local crate's
     Cargo.toml verbatim, the service's Cargo.lock and rust-toolchain.toml,
     and an empty placeholder for every build target — so the layer's inputs
     change only when a manifest or the lockfile does;
  2. the real build, on top of it, which recompiles only the local crates.

WHY NOT cargo-chef. `cargo chef prepare` collects WORKSPACE MEMBERS only
(cargo-chef 0.1.78, src/skeleton/read.rs `manifests`). Every service here is a
standalone package whose local crates are path dependencies OUTSIDE its
directory — ../../libs/rust and the [patch.crates-io] crates under
libs/rust/vendor — so its recipe omits them and `cargo chef cook` fails with
"failed to read /src/libs/rust/<crate>/Cargo.toml". This is the same idea,
driven by `cargo metadata`, which does see path dependencies.

WHICH CRATES. Every package `cargo metadata --all-features` resolves with no
registry or git source (the service, every path dependency, optional ones
included), every `[patch]` path the manifest names (cargo reads a patch's
manifest even when no enabled feature uses it — the worker's adk-sandbox,
reached only through `sandbox-supervisor`), plus every member
of each enclosing [workspace] those packages inherit fields from (cargo loads
the whole workspace to resolve `workspace = true`), plus that workspace's
root manifest.

LOCAL CRATES A REGISTRY CRATE DEPENDS ON ARE COPIED WHOLE. A `[patch]` swaps
a registry crate for a local one, so a REGISTRY crate can depend on a LOCAL
crate (the worker: registry adk-rust uses the vendored adk-runner and
adk-agent). A placeholder there makes the registry crate fail to compile in
the dependency layer ("unresolved import adk_runner::Launcher", measured).
So every local crate reachable from a non-local one in the resolve graph,
and every local crate those reach, is copied with its real sources. They
are vendored third-party crates that change rarely; the real build still
recompiles them after the touch.

THE PLACEHOLDERS. One file per target cargo reports, at the path it reports:
`fn main() {}` for binaries, build scripts, tests, benches and examples, and a
crate doc comment for libraries. Nothing in this repository denies a lint an
empty crate would trip (the workspaces set `unsafe_code = "forbid"`), so the
manifests are copied byte for byte, [lints] included — the real build then
sees exactly the manifests the dependency layer was built with.

The output is a pure function of the manifests and the lockfile: sorted, no
timestamps, so an unchanged recipe is an unchanged COPY and the dependency
layer is reused.
"""

from __future__ import annotations

import json
import pathlib
import shutil
import subprocess
import sys
import tomllib

LIBRARY_KINDS = {"lib", "rlib", "dylib", "cdylib", "staticlib", "proc-macro"}
PLACEHOLDER_LIB = "//! Dependency-layer placeholder; replaced by the real source before the build.\n"
PLACEHOLDER_MAIN = "fn main() {}\n"


def metadata(manifest: pathlib.Path, *extra: str) -> dict:
    out = subprocess.run(
        ["cargo", "metadata", "--format-version", "1", "--locked", "--manifest-path", str(manifest), *extra],
        check=True,
        stdout=subprocess.PIPE,
    ).stdout
    return json.loads(out)


def enclosing_workspace(package_dir: pathlib.Path, root: pathlib.Path) -> pathlib.Path | None:
    """The nearest ancestor manifest with a [workspace] table, inside `root`."""
    for directory in [package_dir, *package_dir.parents]:
        if root not in [directory, *directory.parents]:
            return None
        manifest = directory / "Cargo.toml"
        if manifest.is_file() and any(
            line.strip() == "[workspace]" for line in manifest.read_text().splitlines()
        ):
            return manifest
    return None


def whole_crates(full: dict, by_id: dict) -> set[str]:
    """Manifest paths of the local crates that must keep their real sources."""
    deps = {node["id"]: [d["pkg"] for d in node.get("deps", [])] for node in (full.get("resolve") or {}).get("nodes", [])}
    local = {pid for pid, pkg in by_id.items() if pkg["source"] is None}
    frontier = [d for pid, ds in deps.items() if pid not in local for d in ds if d in local]
    seen: set[str] = set()
    while frontier:
        pid = frontier.pop()
        if pid in seen:
            continue
        seen.add(pid)
        frontier.extend(d for d in deps.get(pid, []) if d in local)
    return {by_id[pid]["manifest_path"] for pid in seen}


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__.split("\n\n", 1)[0], file=sys.stderr)
        return 2
    service = pathlib.Path(argv[1]).resolve()
    out = pathlib.Path(argv[2]).resolve()
    # Everything is written relative to the common root the Containerfiles
    # keep the repository layout under (/src): the service is two levels down.
    root = service.parents[1]

    packages: dict[str, dict] = {}
    full = metadata(service / "Cargo.toml", "--all-features")
    by_id = {package["id"]: package for package in full["packages"]}
    for package in full["packages"]:
        if package["source"] is None:
            packages[package["manifest_path"]] = package
    whole = whole_crates(full, by_id)
    patches = tomllib.loads((service / "Cargo.toml").read_text()).get("patch", {})
    for table in patches.values():
        for spec in table.values():
            if isinstance(spec, dict) and "path" in spec:
                manifest = (service / spec["path"] / "Cargo.toml").resolve()
                if str(manifest) not in packages:
                    for package in metadata(manifest, "--no-deps", "--offline")["packages"]:
                        packages.setdefault(package["manifest_path"], package)

    workspace_roots = set()
    for manifest_path in list(packages):
        workspace = enclosing_workspace(pathlib.Path(manifest_path).parent, root)
        if workspace is not None and workspace.parent != service:
            workspace_roots.add(workspace)
    for workspace in sorted(workspace_roots):
        for package in metadata(workspace, "--no-deps", "--offline")["packages"]:
            packages.setdefault(package["manifest_path"], package)

    files: dict[pathlib.Path, str | pathlib.Path] = {}
    for workspace in workspace_roots:
        files[workspace.relative_to(root)] = workspace
    for manifest_path, package in packages.items():
        manifest = pathlib.Path(manifest_path)
        files[manifest.relative_to(root)] = manifest
        if manifest_path in whole:
            for source in sorted(manifest.parent.rglob("*")):
                if source.is_file() and "target" not in source.relative_to(manifest.parent).parts:
                    files[source.relative_to(root)] = source
            continue
        for target in package["targets"]:
            src = pathlib.Path(target["src_path"]).relative_to(root)
            is_library = bool(LIBRARY_KINDS.intersection(target["kind"]))
            files.setdefault(src, PLACEHOLDER_LIB if is_library else PLACEHOLDER_MAIN)
    for name in ("Cargo.lock", "rust-toolchain.toml"):
        if (service / name).is_file():
            files[(service / name).relative_to(root)] = service / name

    if out.exists():
        shutil.rmtree(out)
    for relative in sorted(files):
        destination = out / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        source = files[relative]
        if isinstance(source, pathlib.Path):
            destination.write_bytes(source.read_bytes())
        else:
            destination.write_text(source)
    local = sum(1 for p in packages if pathlib.Path(p).is_relative_to(root))
    print(f"dependency skeleton: {local} local crate(s), {len(whole)} copied whole, {len(files)} file(s) under {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
