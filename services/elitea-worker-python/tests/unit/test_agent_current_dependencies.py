from __future__ import annotations

import asyncio
import json
import tomllib
from pathlib import Path
from types import SimpleNamespace

import pytest

import elitea_worker.serve as serve_module
from elitea_worker.agent_current_runtime_capabilities import (
    require_agent_current_runtime_capabilities,
)
from elitea_worker.execution.errors import DependencyUnavailable
from elitea_worker.indexing_runtime_capabilities import _profile_digest


_SERVICE_ROOT = Path(__file__).resolve().parents[2]
_LOCK_PATH = _SERVICE_ROOT / "elitea-sdk.lock.json"


def _profile() -> dict:
    return json.loads(_LOCK_PATH.read_bytes())["agent_current_capability_profile"]


def _passing_check(**overrides) -> str:
    profile = _profile()
    versions = profile["verified_distributions"]

    def importer(name: str):
        if name == "elitea_sdk":
            return SimpleNamespace(__file__="/opt/elitea_sdk/__init__.py")
        return SimpleNamespace()

    parameters = {
        "lock_path": _LOCK_PATH,
        "distribution_version": versions.__getitem__,
        "import_module": importer,
        "package_tree_digest": lambda path: json.loads(
            _LOCK_PATH.read_bytes()
        )["installed_package_tree"]["sha256"],
    }
    parameters.update(overrides)
    return require_agent_current_runtime_capabilities(**parameters)


def test_profile_digest_and_exact_requirements_match_project_dependencies() -> None:
    profile = _profile()
    project = tomllib.loads((_SERVICE_ROOT / "pyproject.toml").read_text())
    dependencies = set(project["project"]["dependencies"])
    extras = project["project"]["optional-dependencies"]["agent-current"]

    assert profile["profile_sha256"] == _profile_digest(profile)
    assert profile["python_requirements"] == [
        "elitea-sdk==0.9.8",
        "langchain-core==1.3.3",
        "langgraph==1.0.7",
        "langchain-mcp-adapters==0.1.14",
        "mcp==1.28.1",
        "langgraph-checkpoint-postgres==3.0.0",
    ]
    assert "elitea-sdk==0.9.8" in dependencies
    assert "langchain-core==1.3.3" in dependencies
    assert "langgraph==1.0.7" in dependencies
    assert "langchain-mcp-adapters>=0.1.14,<0.2.0" in extras
    assert "mcp==1.28.1" in extras
    assert "langgraph-checkpoint-postgres==3.0.0" in extras


def test_complete_profile_is_admitted() -> None:
    assert _passing_check() == _profile()["profile_sha256"]


def test_required_flat_scope_auth_imports_are_explicit() -> None:
    profile = _profile()

    assert profile["required_imports"] == [
        "elitea_sdk",
        "elitea_sdk.runtime.clients.client",
        "elitea_sdk.runtime.toolkits.tools",
        "elitea_sdk.runtime.utils.mcp_adapter",
        "langchain_mcp_adapters",
        "langchain_mcp_adapters.client",
        "langchain_mcp_adapters.tools",
        "langgraph.checkpoint.postgres",
    ]


@pytest.mark.parametrize(
    ("override", "expected_cause"),
    [
        (
            {
                "distribution_version": lambda name: (
                    "missing"
                    if name == "langchain-mcp-adapters"
                    else _profile()["verified_distributions"][name]
                )
            },
            "distribution:langchain-mcp-adapters:version-mismatch",
        ),
        (
            {
                "import_module": lambda name: (
                    (_ for _ in ()).throw(ModuleNotFoundError())
                    if name == "langchain_mcp_adapters.client"
                    else (
                        SimpleNamespace(__file__="/opt/elitea_sdk/__init__.py")
                        if name == "elitea_sdk"
                        else SimpleNamespace()
                    )
                )
            },
            "import:langchain_mcp_adapters.client:ModuleNotFoundError",
        ),
        (
            {
                "package_tree_digest": lambda path: "bad-tree",
            },
            "sdk-package-tree:digest-mismatch",
        ),
    ],
)
def test_missing_capability_fails_with_safe_public_error(
    override: dict,
    expected_cause: str,
) -> None:
    with pytest.raises(DependencyUnavailable) as captured:
        _passing_check(**override)

    assert captured.value.safe_message == (
        "The agent current runtime capability profile is incomplete."
    )
    assert captured.value.retryable is True
    assert captured.value.__cause__ is not None
    assert expected_cause in str(captured.value.__cause__)


def test_serve_verifies_agent_profile_before_deployment(monkeypatch) -> None:
    calls: list[str] = []
    config = object()

    def load(_path: Path):
        calls.append("load")
        return config

    def verify_indexing():
        calls.append("verify-indexing")

    def verify_agent():
        calls.append("verify-agent")

    async def serve(actual, *, stop):
        assert actual is config
        assert isinstance(stop, asyncio.Event)
        calls.append("serve")

    monkeypatch.setattr(serve_module, "load_deploy_config", load)
    monkeypatch.setattr(
        serve_module,
        "require_indexing_runtime_capabilities",
        verify_indexing,
    )
    monkeypatch.setattr(
        serve_module,
        "require_agent_current_runtime_capabilities",
        verify_agent,
    )
    monkeypatch.setattr(serve_module, "serve_deployment", serve)

    asyncio.run(serve_module.serve_from_config(Path("/not-opened/runtime.json")))

    assert calls == ["load", "verify-indexing", "verify-agent", "serve"]
