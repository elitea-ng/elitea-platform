from __future__ import annotations

import ast
from pathlib import Path
from types import SimpleNamespace

import pytest

from elitea_worker.agents.sdk_adapter import _require_complete_configuration_registry
from elitea_worker.agents.sdk_adapter import _import_sdk_configurations
from elitea_worker.execution.errors import DependencyUnavailable


# The modules allowed to reach elitea_sdk, and why each one is.
#
# `agents/sdk_adapter.py` is the single execution boundary: every toolkit and
# configuration call the worker makes goes through it, and it verifies the
# installed package tree against the admitted digest before it makes one.
#
# `toolkit_capabilities.py` joined the list when the served toolkit catalogue
# landed. It reads `elitea_sdk.tools.FAILED_IMPORTS` to publish the deny-set
# elitea-main embeds — it makes NO toolkit call, and its import is deliberately
# inside the function so an interpreter without the SDK fails at the answer
# rather than at import. It was already reaching the SDK and this assertion had
# no entry for it, so this test has been red on the branch since that change;
# an unexplained red gate is how a boundary rule stops being read.
_SDK_IMPORT_BOUNDARY = [
    Path("agents/sdk_adapter.py"),
    Path("toolkit_capabilities.py"),
]


def test_only_sdk_adapter_imports_elitea_sdk() -> None:
    source_root = Path(__file__).parents[2] / "src" / "elitea_worker"
    importers: list[Path] = []
    for path in source_root.rglob("*.py"):
        source = path.read_text(encoding="utf-8")
        tree = ast.parse(source)
        for node in ast.walk(tree):
            names: list[str] = []
            if isinstance(node, ast.Import):
                names.extend(alias.name for alias in node.names)
            elif isinstance(node, ast.ImportFrom) and node.module:
                names.append(node.module)
            if any(name == "elitea_sdk" or name.startswith("elitea_sdk.") for name in names):
                importers.append(path.relative_to(source_root))
                break
        else:
            if "elitea_sdk.configurations" in source:
                importers.append(path.relative_to(source_root))
    assert sorted(importers) == sorted(_SDK_IMPORT_BOUNDARY)


def test_only_command_transport_modules_may_import_redis_client() -> None:
    source_root = Path(__file__).parents[2] / "src" / "elitea_worker"
    allowed = {
        Path("transport/redis_asyncio.py"),
        Path("transport/redis_commands.py"),
    }
    offenders: list[Path] = []
    for path in source_root.rglob("*.py"):
        if path.relative_to(source_root) in allowed:
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"))
        for node in ast.walk(tree):
            if isinstance(node, ast.Import):
                names = [alias.name for alias in node.names]
            elif isinstance(node, ast.ImportFrom) and node.module:
                names = [node.module]
            else:
                continue
            if any(name == "redis" or name.startswith("redis.") for name in names):
                offenders.append(path.relative_to(source_root))
                break
    assert offenders == []


def test_sdk_configuration_import_failures_are_rejected_without_error_text() -> None:
    canary = "password=TEST_ONLY_DO_NOT_EMIT"

    with pytest.raises(DependencyUnavailable) as caught:
        _require_complete_configuration_registry(
            SimpleNamespace(FAILED_IMPORTS={"provider": canary})
        )

    assert canary not in str(caught.value)


def test_sdk_tools_are_loaded_before_configurations(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    imported: list[str] = []
    configurations = SimpleNamespace()

    def import_module(name: str) -> object:
        imported.append(name)
        if name == "elitea_sdk.configurations":
            return configurations
        return SimpleNamespace()

    monkeypatch.setattr(
        "elitea_worker.agents.sdk_adapter.importlib.import_module",
        import_module,
    )

    assert _import_sdk_configurations() is configurations
    assert imported == ["elitea_sdk.tools", "elitea_sdk.configurations"]


@pytest.mark.parametrize("failed_imports", [None, (), []])
def test_sdk_configuration_failure_registry_must_be_an_explicit_empty_dict(
    failed_imports: object,
) -> None:
    with pytest.raises(DependencyUnavailable):
        _require_complete_configuration_registry(
            SimpleNamespace(FAILED_IMPORTS=failed_imports)
        )

    _require_complete_configuration_registry(SimpleNamespace(FAILED_IMPORTS={}))
