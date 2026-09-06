"""Guards on the engine copy. No engine dependencies needed.

None of these import an engine module — they hash bytes and compile source —
so they run in any environment, including the SPI-shell image's, which does
not carry torch.
"""

from __future__ import annotations

import ast
import hashlib
import json
import subprocess
import sys
from pathlib import Path

import pytest

SERVICE_ROOT = Path(__file__).resolve().parents[2]
ENGINE_DIR = SERVICE_ROOT / "src" / "elitea_deepwiki" / "engine"
MANIFEST_PATH = ENGINE_DIR / "COPY_MANIFEST.json"
REFRESH_TOOL = SERVICE_ROOT / "tools" / "refresh_engine_copy.py"

MANIFEST = json.loads(MANIFEST_PATH.read_text(encoding="utf-8"))


def engine_files() -> list[Path]:
    return sorted(
        path
        for path in ENGINE_DIR.rglob("*.py")
        if "__pycache__" not in path.parts
    )


def test_manifest_pins_a_revision_and_a_plausible_tree():
    assert MANIFEST["source_revision"], "the legacy revision must be pinned"
    assert MANIFEST["source_path"] == "plugin_implementation"
    # ~90.5k lines across ~101 files. A manifest that suddenly covered five
    # files would still "pass" every digest check it listed.
    #
    # COUNT BOTH SETS. A file that gains a declared in-place substitution
    # LEAVES `files` for `transformed_files` — it is still guarded, by a
    # digest in the other set — so a bound on `file_count` alone falls by one
    # per transform and would have to be lowered each time, which is the
    # opposite of what it is for.
    in_place = sum(1 for entry in MANIFEST["transformed_files"].values() if entry.get("in_place"))
    assert MANIFEST["file_count"] + in_place >= 101
    # The GUARDED SET is counted, not `file_count` alone: an in-place
    # transform moves a file out of `files` and into `transformed_files`,
    # where it is still digest-checked. Counting only `files` would let a
    # declared transform lower this floor one file at a time, which is the
    # erosion the assertion exists to stop.
    in_place = [
        entry
        for entry in MANIFEST["transformed_files"].values()
        if entry.get("in_place")
    ]
    assert MANIFEST["file_count"] + len(in_place) >= 101
    assert MANIFEST["total_bytes"] > 3_000_000


def test_every_copied_file_matches_its_digest():
    """The copy is verbatim, byte for byte.

    ADR-0022 decision 1 is that the engine *moves* rather than being
    rewritten. That claim is only checkable if a modification is detectable,
    and at ~90k lines it is not checkable by reading.
    """
    mismatched = []
    for relative, entry in MANIFEST["files"].items():
        path = ENGINE_DIR / relative
        if not path.is_file():
            mismatched.append(f"missing: {relative}")
            continue
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        if digest != entry["sha256"]:
            mismatched.append(f"modified: {relative}")

    assert not mismatched, (
        "the engine copy has diverged from the manifest:\n  "
        + "\n  ".join(mismatched)
        + "\n\nRevert, or re-run tools/refresh_engine_copy.py and say why in "
        "the commit."
    )


def test_no_engine_file_is_outside_the_manifest():
    """An added file would otherwise be unguarded.

    The reverse of the digest check: digests only cover what they list, so a
    file smuggled into the copy would never be hashed. This is the same shape
    as the manifest bug found while building it — matching on a bare filename
    dropped eight subpackage ``__init__.py`` files from the manifest while the
    file count still looked plausible.
    """
    not_copied = set(MANIFEST["not_copied"])
    # Files transformed IN PLACE are accounted for under transformed_files
    # instead of files: they carry declared substitutions, so their digest is
    # deliberately not the legacy one. They are still guarded — see
    # test_in_place_transforms_match_their_recorded_digest.
    transformed = {
        entry["target"].split("engine/", 1)[-1]
        for entry in MANIFEST.get("transformed_files", {}).values()
        if entry.get("in_place")
    }
    present = {
        path.relative_to(ENGINE_DIR).as_posix()
        for path in ENGINE_DIR.rglob("*")
        if path.is_file() and "__pycache__" not in path.parts
    }
    unaccounted = present - set(MANIFEST["files"]) - not_copied - transformed
    assert not unaccounted, f"files in the copy but not in the manifest: {sorted(unaccounted)}"


def test_in_place_transforms_match_their_recorded_digest():
    """A transformed file is guarded too, against its POST-transform digest.

    Otherwise the transform mechanism would be a hole in the copy guard: a
    file could be edited freely once it was listed as transformed.
    """
    entries = {
        name: entry
        for name, entry in MANIFEST.get("transformed_files", {}).items()
        if entry.get("in_place")
    }
    assert entries, "no in-place transforms recorded; the mechanism regressed"

    for name, entry in entries.items():
        path = SERVICE_ROOT / entry["target"]
        assert path.is_file(), name
        assert hashlib.sha256(path.read_bytes()).hexdigest() == entry["sha256"], (
            f"{name} does not match its recorded post-transform digest"
        )
        assert entry["substitutions"], f"{name} records no substitutions"


def test_the_adr_0022_security_retirements_hold_in_the_copy():
    """ADR-0022 decision 6, checked against the code rather than the README.

    "the legacy X-SECRET shared-string header is retired; no surface of the
    ported service sends or honours it" and "TLS verification is mandatory on
    every outbound call. verify=False does not appear in the ported code."

    Both were FALSE of the copied artifact client until it was transformed —
    the README claimed them while the code still sent the header on every
    upload. This asserts the code, over the whole tree, so the claim cannot
    drift from the thing again.
    """
    offenders_header, offenders_tls = [], []
    for path in engine_files() + [
        SERVICE_ROOT / "src" / "elitea_deepwiki" / "tool_operations.py"
    ]:
        for number, line in enumerate(
            path.read_text(encoding="utf-8").splitlines(), start=1
        ):
            stripped = line.strip()
            if stripped.startswith("#") or stripped.startswith("--"):
                continue
            # A header DICT entry, not a mention in prose or a constant name.
            if '"X-SECRET":' in line:
                offenders_header.append(f"{path.name}:{number}")
            if "verify=False" in line:
                offenders_tls.append(f"{path.name}:{number}")

    assert not offenders_header, (
        "X-SECRET is still sent: " + ", ".join(offenders_header)
    )
    assert not offenders_tls, (
        "TLS verification is still disabled: " + ", ".join(offenders_tls)
    )


def test_the_refresh_tool_agrees():
    """The tool CI runs, run here, so a broken tool is caught by the suite."""
    result = subprocess.run(
        [sys.executable, str(REFRESH_TOOL), "--check"],
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, result.stdout + result.stderr


@pytest.mark.parametrize(
    "path", engine_files(), ids=lambda p: p.relative_to(ENGINE_DIR).as_posix()
)
def test_every_engine_module_parses(path: Path):
    """Syntax-check the whole copy without importing it.

    Importing needs torch, faiss, tree-sitter grammars and the rest of the
    engine's 92-package closure, which the shell image deliberately does not
    carry. Compiling needs none of that, and it is enough to prove the copy
    landed intact and targets this interpreter's Python version.
    """
    source = path.read_text(encoding="utf-8")
    ast.parse(source, filename=str(path))


def test_only_our_init_is_not_copied():
    """The one file we own inside the copied tree, and the manifest itself."""
    assert set(MANIFEST["not_copied"]) == {"__init__.py", "COPY_MANIFEST.json"}

    init = (ENGINE_DIR / "__init__.py").read_text(encoding="utf-8")
    assert "plugin_implementation" in init, (
        "the package initialiser must still install the compatibility alias"
    )
    assert MANIFEST["source_revision"] in init, (
        "the initialiser's documented revision has drifted from the manifest"
    )


def test_subpackage_inits_are_guarded():
    """The bug the manifest tool had: they must be IN the manifest.

    Matching ``not_copied`` on a bare filename excluded every subpackage
    ``__init__.py`` — nine files — from the digest set while the file count
    still looked reasonable.
    """
    guarded = {
        name for name in MANIFEST["files"] if name.endswith("__init__.py")
    }
    assert len(guarded) >= 8, f"only {len(guarded)} subpackage inits are guarded"
    assert "state/__init__.py" in guarded
    assert "agents/__init__.py" in guarded


def test_the_engine_does_not_import_the_pylon_shim():
    """Nothing copied may reach back into module.py / methods / routes / events.

    The shim is not copied and has no successor inside the engine; if a module
    imported it, the copy would be incomplete rather than verbatim-and-whole.
    """
    offenders = []
    for path in engine_files():
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.ImportFrom) and node.module:
                root = node.module.split(".")[0]
                if root in ("methods", "routes", "events", "pylon", "arbiter"):
                    offenders.append(f"{path.name}:{node.lineno} -> {node.module}")
            elif isinstance(node, ast.Import):
                for alias in node.names:
                    root = alias.name.split(".")[0]
                    if root in ("methods", "routes", "events", "pylon", "arbiter"):
                        offenders.append(f"{path.name}:{node.lineno} -> {alias.name}")

    assert not offenders, (
        "engine modules import the Pylon shim, which was not copied:\n  "
        + "\n  ".join(offenders)
    )


def test_the_compatibility_alias_resolves_engine_submodules():
    """``from plugin_implementation.x import y`` must work inside the engine.

    Twenty engine call sites import their siblings absolutely. They are all
    inside functions, so by the time one runs, this package has been imported
    and the alias installed — but only *in that process*.
    """
    import sys

    import elitea_deepwiki.engine as engine

    assert sys.modules.get("plugin_implementation") is engine

    import plugin_implementation.constants as constants  # noqa: PLC0415

    assert constants.__name__ == "plugin_implementation.constants"


def test_subprocess_workers_resolve_by_their_real_module_name():
    """The subprocess launch path, fixed after running it.

    ``tool_operations`` picks a worker module by trying to import each
    candidate IN THE PARENT, then hands the winner to a child that inherits
    only ``sys.path``. ``plugin_implementation.X`` imports fine in the parent —
    the engine package installs that alias — and is unresolvable in the child,
    which never imports the engine package. The declared substitution names
    the real module instead, so the child imports something ``sys.path`` can
    find, and that import installs the alias for the engine's own absolute
    imports.

    Diagnosed by running generate_wiki with run_in_subprocess=True and reading
    the worker log: ``ModuleNotFoundError: No module named
    'plugin_implementation'``.
    """
    source = (SERVICE_ROOT / "src" / "elitea_deepwiki" / "tool_operations.py").read_text(
        encoding="utf-8"
    )
    for worker in (
        "wiki_subprocess_worker",
        "ask_subprocess_worker",
        "deep_research_subprocess_worker",
    ):
        assert f'"elitea_deepwiki.engine.{worker}"' in source, worker
        assert f'"plugin_implementation.{worker}"' not in source, worker


def test_the_kubernetes_job_path_is_still_not_repointed():
    """The remaining half of ADR-0022 decision 7, pinned.

    Subprocess mode works (above). The Kubernetes-Job path does not: it still
    builds a ``PYTHONPATH`` from the legacy filesystem layout
    (``/data/plugins/deepwiki_plugin``, ``/app/deepwiki_plugin``) and runs
    ``plugin_implementation/wiki_job_worker.py`` as a FILE, neither of which
    exists in this image. It also still expects the licence-credential init
    container that ADR-0022 drops, and passes secrets through Job environment
    variables where the ADR requires projected files.

    This asserts the legacy shape is still what the copy contains — a reminder,
    not an endorsement. When the Job path is repointed, this test fails and
    should be replaced by one checking the new entry point.
    """
    source = (ENGINE_DIR / "k8s_job_manager.py").read_text(encoding="utf-8")
    assert "/data/plugins/deepwiki_plugin" in source
    assert "plugin_implementation/wiki_job_worker.py" in source
