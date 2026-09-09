from __future__ import annotations

import ast
from contextlib import contextmanager
import hashlib
import importlib
import importlib.metadata
import json
import os
import re
import subprocess
import sys
import tomllib
from pathlib import Path

import pytest

from elitea_worker.agents.configuration_registry import ConfigurationRegistryShadow
from elitea_worker.agents.sdk_adapter import _package_tree_digest
from elitea_worker.constants import (
    CONFIGURATION_CATALOG_SHA256,
    CONFIGURATION_CATALOG_REVISION,
    INDEX_TYPES_SOURCE_REVISION,
    SDK_DISTRIBUTION_VERSION,
    SDK_PACKAGE_TREE_SHA256,
    SDK_SOURCE_ARCHIVE_SHA256,
    SDK_SOURCE_PATCH_REVISIONS,
    SDK_SOURCE_REVISION,
)


_SERVICE_ROOT = Path(__file__).resolve().parents[2]
_PLATFORM_ROOT = _SERVICE_ROOT.parents[1]
_SDK_ROOT = Path(
    os.environ.get(
        "ELITEA_SDK_SOURCE_ROOT",
        _PLATFORM_ROOT.parent / "elitea-sdk",
    )
)
_GO_CATALOG = (
    _PLATFORM_ROOT
    / "services"
    / "elitea-main"
    / "internal"
    / "runtimecomposition"
    / "current_sdk_configuration_catalog_snapshot.json"
)
_GO_INDEX_TYPES = (
    _PLATFORM_ROOT
    / "services"
    / "elitea-main"
    / "internal"
    / "runtimecomposition"
    / "current_index_types_snapshot.json"
)
_GO_INDEX_TYPES_UI_FIXTURE = (
    _PLATFORM_ROOT
    / "services"
    / "elitea-main"
    / "internal"
    / "api"
    / "v2"
    / "indextypes"
    / "testdata"
    / "current_index_types_ui_response.json"
)


@contextmanager
def _import_pinned_sdk_checkout():
    """Import the admitted SDK checkout ahead of any stale editable overlays.

    The shared workspace venv can retain partial ``elitea_sdk`` namespace
    directories from older editable installs. The parity assertions in this
    module compare the admitted checkout and generated Go snapshots, so force
    imports to resolve from ``_SDK_ROOT`` for the duration of those reads.
    """

    original_path = list(sys.path)
    preserved_modules = {
        name: module
        for name, module in sys.modules.items()
        if name == "elitea_sdk" or name.startswith("elitea_sdk.")
    }
    try:
        for name in list(preserved_modules):
            sys.modules.pop(name, None)
        sys.path.insert(0, str(_SDK_ROOT))
        yield
    finally:
        for name in list(sys.modules):
            if name == "elitea_sdk" or name.startswith("elitea_sdk."):
                sys.modules.pop(name, None)
        sys.modules.update(preserved_modules)
        sys.path[:] = original_path


def test_worker_dependency_and_lock_share_one_sdk_identity() -> None:
    lock = json.loads((_SERVICE_ROOT / "elitea-sdk.lock.json").read_bytes())
    project = tomllib.loads((_SERVICE_ROOT / "pyproject.toml").read_text())
    dependency = next(
        item for item in project["project"]["dependencies"] if item.startswith("elitea-sdk")
    )

    assert dependency == f"elitea-sdk=={SDK_DISTRIBUTION_VERSION}"
    containerfile = (_SERVICE_ROOT / "Containerfile").read_text()
    assert f"ARG ELITEA_SDK_REVISION={SDK_SOURCE_REVISION}" in containerfile
    assert f"ARG ELITEA_SDK_ARCHIVE_SHA256={SDK_SOURCE_ARCHIVE_SHA256}" in containerfile
    assert (
        f"ARG ELITEA_SDK_MCP_PREFLIGHT_PATCH={SDK_SOURCE_PATCH_REVISIONS[0]}"
        in containerfile
    )
    assert (
        f"ARG ELITEA_SDK_MCP_HARD_TIMEOUT_PATCH={SDK_SOURCE_PATCH_REVISIONS[1]}"
        in containerfile
    )
    assert (
        f"ARG ELITEA_SDK_TOOL_GROUPS_PATCH={SDK_SOURCE_PATCH_REVISIONS[2]}"
        in containerfile
    )
    assert (
        f"ARG ELITEA_SDK_IMAGEGEN_PATCH={SDK_SOURCE_PATCH_REVISIONS[3]}"
        in containerfile
    )
    assert f"ARG ELITEA_SDK_VERSION={SDK_DISTRIBUTION_VERSION}" in containerfile
    assert "python -m pip wheel \\\n    --no-cache-dir" in containerfile
    assert 'test -f "/wheels/elitea_sdk-${ELITEA_SDK_VERSION}-py3-none-any.whl"' in containerfile
    assert (
        '"elitea-sdk @ file:///wheels/elitea_sdk-${ELITEA_SDK_VERSION}-py3-none-any.whl"'
        in containerfile
    )
    assert "git -C ./elitea-sdk archive --format=tar HEAD" in containerfile
    assert "scripts/verify_locked_artifacts.py" in containerfile
    assert "verified_source_archives" in containerfile
    assert "verified_wheels" in containerfile
    assert "FigmaPy-2018.1.0.tar.gz" in containerfile
    assert "AZURE_DEVOPS_CACHE_DIR=/tmp/.azure-devops" in containerfile
    assert "fonts-dejavu-core" in containerfile
    worker_ci = (_PLATFORM_ROOT / ".github/workflows/ci-python.yml").read_text()
    assert (
        f"SDK_REVISION: {SDK_SOURCE_REVISION}"
    ) in worker_ci
    assert f"SDK_ARCHIVE_SHA256: {SDK_SOURCE_ARCHIVE_SHA256}" in worker_ci
    assert f"SDK_MCP_PREFLIGHT_PATCH: {SDK_SOURCE_PATCH_REVISIONS[0]}" in worker_ci
    assert f"SDK_MCP_HARD_TIMEOUT_PATCH: {SDK_SOURCE_PATCH_REVISIONS[1]}" in worker_ci
    assert f"SDK_TOOL_GROUPS_PATCH: {SDK_SOURCE_PATCH_REVISIONS[2]}" in worker_ci
    assert f"SDK_IMAGEGEN_PATCH: {SDK_SOURCE_PATCH_REVISIONS[3]}" in worker_ci
    assert "/tmp/elitea-sdk-current/tests/runtime/test_mcp_discovery_hang.py" in worker_ci
    assert '"elitea-sdk @ file:///tmp/elitea-sdk-current"' in worker_ci
    assert '"./services/elitea-worker-python[agent-current,indexing-current,test]"' in worker_ci
    # Tier 2 of the elitea-sdk compatibility gates. This line is the ONLY
    # executable reference to the conformance driver in the tree; every other
    # mention is prose. Assert the INVOCATION, not the step name: the string
    # "elitea-sdk tier 3 wiring" survives deleting that step outright, because
    # it also appears in a paths comment.
    assert "services/elitea-llm-gateway/scripts/sdk-conformance/run.sh" in worker_ci
    assert lock["distribution_version"] == SDK_DISTRIBUTION_VERSION
    assert lock["source"]["revision"] == SDK_SOURCE_REVISION
    assert lock["source"]["git_archive_sha256"] == SDK_SOURCE_ARCHIVE_SHA256
    assert tuple(lock["source"]["patch_revisions"]) == SDK_SOURCE_PATCH_REVISIONS
    assert lock["installed_package_tree"]["sha256"] == SDK_PACKAGE_TREE_SHA256
    assert lock["installed_package_tree"]["file_count"] > 0

    profile = lock["indexing_capability_profile"]
    verified_requirements = profile["artifact_verified_requirements"]
    artifact_records = {
        **profile["verified_wheels"],
        **profile["verified_source_archives"],
    }
    normalize = lambda name: re.sub(r"[-_.]+", "-", name).lower()
    requirement_names = {
        normalize(requirement.split("==", 1)[0])
        for requirement in verified_requirements
    }
    record_names = {normalize(name) for name in artifact_records}
    assert all(requirement.count("==") == 1 for requirement in verified_requirements)
    assert len(requirement_names) == len(verified_requirements)
    assert requirement_names == record_names
    assert not (
        set(map(normalize, profile["verified_wheels"]))
        & set(map(normalize, profile["verified_source_archives"]))
    )
    project_requirements = {
        normalize(requirement.split("==", 1)[0])
        for requirement in profile["python_requirements"]
    }
    worker_dependencies = {
        normalize(
            requirement.split("[", 1)[0]
            .split("=", 1)[0]
            .split("<", 1)[0]
            .split(">", 1)[0]
        )
        for requirement in project["project"]["dependencies"]
    }
    assert requirement_names <= project_requirements | worker_dependencies
    artifacts = [
        artifact
        for record in artifact_records.values()
        for artifact in record.get("artifacts", [record])
    ]
    assert len({record["filename"] for record in artifacts}) == len(artifacts)
    for record in artifact_records.values():
        assert record.get("origin") == "PyPI" or record.get("license")
    for record in artifacts:
        assert len(record["sha256"]) == 64
        int(record["sha256"], 16)


def test_local_pinned_sdk_checkout_matches_lock_when_available() -> None:
    if not (_SDK_ROOT / ".git").exists():
        pytest.skip("pinned SDK evidence checkout is unavailable")
    revision = subprocess.run(
        ["git", "-C", str(_SDK_ROOT), "rev-parse", "HEAD"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    if revision != SDK_SOURCE_REVISION:
        pytest.skip("local SDK checkout is not at the admitted revision")

    lock = json.loads((_SERVICE_ROOT / "elitea-sdk.lock.json").read_bytes())
    sdk_project = tomllib.loads((_SDK_ROOT / "pyproject.toml").read_text())
    package_root = _SDK_ROOT / "elitea_sdk"
    archive = subprocess.run(
        ["git", "-C", str(_SDK_ROOT), "archive", "--format=tar", revision],
        check=True,
        capture_output=True,
    ).stdout

    assert sdk_project["project"]["version"] == SDK_DISTRIBUTION_VERSION
    assert len(list(package_root.rglob("*.py"))) == lock["installed_package_tree"][
        "file_count"
    ]
    assert _package_tree_digest(package_root) == SDK_PACKAGE_TREE_SHA256
    assert hashlib.sha256(archive).hexdigest() == SDK_SOURCE_ARCHIVE_SHA256


def test_go_binding_catalog_matches_worker_registry_shadow() -> None:
    with _import_pinned_sdk_checkout():
        module = importlib.import_module("elitea_sdk.configurations")
    assert module.FAILED_IMPORTS == {}
    shadow = ConfigurationRegistryShadow(module.get_class_configurations).snapshot
    document = json.loads(_GO_CATALOG.read_bytes())

    assert document["complete"] is True
    assert document["sdk_revision"] == CONFIGURATION_CATALOG_REVISION
    assert document["catalog_revision"] == CONFIGURATION_CATALOG_REVISION
    assert document["catalog_digest"] == f"sha256:{shadow.catalog_digest.hex()}"
    assert shadow.catalog_digest.hex() == CONFIGURATION_CATALOG_SHA256
    assert document["entry_count"] == len(shadow.entries) == len(document["entries"])
    assert document["entries"] == [
        {
            "configuration_type": entry.type,
            "section": entry.section,
            "schema_id": f"elitea.configuration.{entry.type}",
            "schema_revision": CONFIGURATION_CATALOG_REVISION,
            "schema_digest": f"sha256:{entry.schema_digest.hex()}",
            "validation_supported": entry.validation_supported,
            "connection_check_supported": entry.connection_check_supported,
        }
        for entry in shadow.entries
    ]


def test_go_index_types_snapshot_matches_current_worker_sdk_projection() -> None:
    constants_path = (
        _SDK_ROOT
        / "elitea_sdk"
        / "runtime"
        / "langchain"
        / "document_loaders"
        / "constants.py"
    )
    constants_source = Path(constants_path).read_bytes()
    assignments = {
        statement.targets[0].id: statement.value
        for statement in ast.parse(constants_source).body
        if isinstance(statement, ast.Assign)
        and len(statement.targets) == 1
        and isinstance(statement.targets[0], ast.Name)
    }

    def loader_mime_types(name: str) -> dict[str, str]:
        mapping = assignments[name]
        assert isinstance(mapping, ast.Dict)
        result: dict[str, str] = {}
        for key_node, config_node in zip(
            mapping.keys, mapping.values, strict=True
        ):
            assert key_node is not None
            assert isinstance(config_node, ast.Dict)
            extension = ast.literal_eval(key_node)
            fields = {
                ast.literal_eval(field): value
                for field, value in zip(
                    config_node.keys, config_node.values, strict=True
                )
                if field is not None
            }
            result[extension] = ast.literal_eval(fields["mime_type"])
        return dict(sorted(result.items()))

    code_extensions = ast.literal_eval(assignments["code_extensions"])
    response = {
        "document_types": loader_mime_types("document_loaders_map"),
        "image_types": loader_mime_types("image_loaders_map"),
        "code_types": {
            extension: "text/plain" for extension in sorted(code_extensions)
        },
    }
    canonical = json.dumps(
        response,
        allow_nan=False,
        ensure_ascii=False,
        separators=(",", ":"),
        sort_keys=True,
    ).encode()
    snapshot = json.loads(_GO_INDEX_TYPES.read_bytes())

    assert snapshot["complete"] is True
    assert snapshot["sdk_revision"] == INDEX_TYPES_SOURCE_REVISION
    assert snapshot["source_digest"] == (
        f"sha256:{hashlib.sha256(constants_source).hexdigest()}"
    )
    assert snapshot["category_count"] == 3
    assert snapshot["entry_count"] == sum(map(len, response.values()))
    assert snapshot["snapshot_digest"] == (
        f"sha256:{hashlib.sha256(canonical).hexdigest()}"
    )
    assert snapshot["categories"] == response

    # This intentionally matches indexer_worker's current producer, which does
    # not project image_loaders_map_converted. EliteaUI adds SVG separately.
    assert ".svg" not in response["image_types"]
    assert ".bmp" not in response["image_types"]

    ui_fixture = json.loads(_GO_INDEX_TYPES_UI_FIXTURE.read_bytes())
    assert ui_fixture == response
    assert set(ui_fixture) == {
        "document_types",
        "image_types",
        "code_types",
    }
