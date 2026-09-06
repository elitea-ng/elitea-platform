from __future__ import annotations

import asyncio
import json
import tomllib
from pathlib import Path
from types import SimpleNamespace

import pytest

import elitea_worker.serve as serve_module
from elitea_worker.execution.errors import DependencyUnavailable
from elitea_worker.indexing_runtime_capabilities import (
    _profile_digest,
    require_indexing_runtime_capabilities,
)


_SERVICE_ROOT = Path(__file__).resolve().parents[2]
_LOCK_PATH = _SERVICE_ROOT / "elitea-sdk.lock.json"


def _profile() -> dict:
    return json.loads(_LOCK_PATH.read_bytes())["indexing_capability_profile"]


def _passing_check(**overrides) -> str:
    profile = _profile()
    versions = profile["verified_distributions"]

    def importer(name: str):
        if name == "elitea_sdk":
            return SimpleNamespace(__file__="/opt/elitea_sdk/__init__.py")
        if name == "elitea_sdk.tools":
            return SimpleNamespace(FAILED_IMPORTS={})
        return SimpleNamespace()

    parameters = {
        "lock_path": _LOCK_PATH,
        "distribution_version": versions.__getitem__,
        "import_module": importer,
        "find_executable": lambda name: f"/usr/bin/{name}",
        "find_shared_library": lambda name: f"lib{name}.so",
        "package_tree_digest": lambda path: json.loads(
            _LOCK_PATH.read_bytes()
        )["installed_package_tree"]["sha256"],
        "ocr_probe": lambda: None,
        "markdown_probe": lambda: None,
    }
    parameters.update(overrides)
    return require_indexing_runtime_capabilities(**parameters)


def test_profile_digest_and_exact_requirements_match_project_extra() -> None:
    profile = _profile()
    project = tomllib.loads((_SERVICE_ROOT / "pyproject.toml").read_text())

    assert profile["profile_sha256"] == _profile_digest(profile)
    assert project["project"]["optional-dependencies"]["indexing-current"] == (
        profile["python_requirements"]
    )
    assert all("==" in requirement for requirement in profile["python_requirements"])
    assert "elitea-sdk[all]" not in json.dumps(project)


def test_complete_profile_is_admitted() -> None:
    assert _passing_check() == _profile()["profile_sha256"]


def test_required_indexing_families_and_sdk_import_aliases_are_explicit() -> None:
    profile = _profile()

    assert profile["required_indexing_families"] == [
        "ado_boards",
        "ado_plans",
        "ado_repos",
        "ado_wiki",
        "bitbucket",
        "confluence",
        "figma",
        "github",
        "gitlab",
        "jira",
        "qtest",
        "sharepoint",
        "testrail",
        "xray_cloud",
        "zephyr_enterprise",
        "zephyr_essential",
        "zephyr_scale",
    ]
    assert set(profile["required_indexing_family_imports"]) == set(
        profile["required_indexing_families"]
    )
    assert profile["required_indexing_family_imports"]["ado_boards"] == (
        "elitea_sdk.tools.ado.work_item"
    )
    assert profile["required_indexing_family_imports"]["xray_cloud"] == (
        "elitea_sdk.tools.xray"
    )
    assert profile["required_sdk_tool_import_keys"] == [
        "ado",
        *profile["required_indexing_families"],
    ]


def test_unrelated_sdk_optional_tool_failures_do_not_block_indexing() -> None:
    def importer(name: str):
        if name == "elitea_sdk":
            return SimpleNamespace(__file__="/opt/elitea_sdk/__init__.py")
        if name == "elitea_sdk.tools":
            return SimpleNamespace(
                FAILED_IMPORTS={
                    "aws": "sensitive optional failure",
                    "playwright": "sensitive optional failure",
                }
            )
        return SimpleNamespace()

    assert _passing_check(import_module=importer) == _profile()["profile_sha256"]


def test_required_sdk_indexing_tool_failure_fails_closed_without_error_text() -> None:
    canary = "https://secret.invalid/token"

    def importer(name: str):
        if name == "elitea_sdk":
            return SimpleNamespace(__file__="/opt/elitea_sdk/__init__.py")
        if name == "elitea_sdk.tools":
            return SimpleNamespace(
                FAILED_IMPORTS={"github": canary, "aws": canary}
            )
        return SimpleNamespace()

    with pytest.raises(DependencyUnavailable) as captured:
        _passing_check(import_module=importer)

    assert captured.value.__cause__ is not None
    cause = str(captured.value.__cause__)
    assert "sdk-indexing-toolkit:github:failed-import" in cause
    assert "aws" not in cause
    assert canary not in cause


@pytest.mark.parametrize(
    ("override", "expected_cause"),
    [
        (
            {
                "distribution_version": lambda name: (
                    "missing" if name == "atlassian-python-api" else
                    _profile()["verified_distributions"][name]
                )
            },
            "distribution:atlassian-python-api:version-mismatch",
        ),
        (
            {
                "import_module": lambda name: (
                    (_ for _ in ()).throw(ModuleNotFoundError())
                    if name == "elitea_sdk.tools.confluence"
                    else (
                        SimpleNamespace(__file__="/opt/elitea_sdk/__init__.py")
                        if name == "elitea_sdk"
                        else SimpleNamespace()
                    )
                )
            },
            "import:elitea_sdk.tools.confluence:ModuleNotFoundError",
        ),
        (
            {
                "find_executable": lambda name: (
                    None if name == "pdftoppm" else f"/usr/bin/{name}"
                )
            },
            "executable:pdftoppm:missing",
        ),
        (
            {"find_shared_library": lambda name: None},
            "shared-library:cairo:missing",
        ),
        (
            {
                "ocr_probe": lambda: (_ for _ in ()).throw(
                    RuntimeError("probe failed")
                )
            },
            "ocr-runtime:RuntimeError",
        ),
        (
            {
                "markdown_probe": lambda: (_ for _ in ()).throw(
                    RuntimeError("probe failed")
                )
            },
            "markdown-runtime:RuntimeError",
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
        "The indexing runtime capability profile is incomplete."
    )
    assert captured.value.retryable is True
    assert captured.value.__cause__ is not None
    assert expected_cause in str(captured.value.__cause__)


def test_profile_admits_exact_langchain_ocr_wrapper_and_artifact() -> None:
    profile = _profile()

    assert "pytesseract==0.3.13" in profile["python_requirements"]
    assert "unstructured_pytesseract==0.3.13" not in profile[
        "python_requirements"
    ]
    assert profile["verified_distributions"]["pytesseract"] == "0.3.13"
    assert "unstructured-pytesseract" not in profile["verified_distributions"]
    assert "pytesseract" in profile["required_imports"]
    assert "unstructured_pytesseract" not in profile["required_imports"]
    assert "fonts-dejavu-core" in profile["system_packages"]
    assert profile["verified_wheels"]["pytesseract"] == {
        "filename": "pytesseract-0.3.13-py3-none-any.whl",
        "license": "Apache License 2.0",
        "sha256": (
            "7a99c6c2ac598360693d83a416e36e0b"
            "33a67638bb9d77fdcac094a3589d4b34"
        ),
    }


def test_profile_admits_shared_markdown_parser_and_verified_artifacts() -> None:
    profile = _profile()

    assert "unstructured==0.18.18" in profile["python_requirements"]
    assert "unstructured-client==0.39.1" in profile["python_requirements"]
    assert profile["verified_distributions"]["unstructured"] == "0.18.18"
    assert profile["verified_distributions"]["unstructured-client"] == "0.39.1"
    assert "unstructured" in profile["required_imports"]
    assert profile["verified_wheels"]["unstructured"]["sha256"] == (
        "d5189bdd5e2a1c5ed3cc289cfb4fb483"
        "c6f2dd544b42744bdc5b81d3388ea527"
    )
    assert profile["verified_wheels"]["unstructured-client"]["sha256"] == (
        "b0a179bcbbeae1f155712fd646012d2b"
        "3d426778c06eb9a3f5971563ad6fa8fa"
    )


def test_serve_verifies_profile_before_deployment(monkeypatch) -> None:
    calls: list[str] = []
    config = object()

    def load(_path: Path):
        calls.append("load")
        return config

    def verify():
        calls.append("verify")

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
        verify,
    )
    monkeypatch.setattr(
        serve_module,
        "require_agent_current_runtime_capabilities",
        verify_agent,
    )
    monkeypatch.setattr(serve_module, "serve_deployment", serve)

    asyncio.run(serve_module.serve_from_config(Path("/not-opened/runtime.json")))

    assert calls == ["load", "verify", "verify-agent", "serve"]
