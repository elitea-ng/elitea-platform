from __future__ import annotations

import importlib
import json
import os
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pytest


BASELINE_LOCK = Path(__file__).with_name("current_sdk_baseline.json")


@dataclass(frozen=True, slots=True)
class CurrentSDK:
    root: Path
    revision: str
    version: str
    client: type[Any]
    confluence_wrapper: type[Any]
    confluence_loader: type[Any]


def current_model_clients(
    current_sdk: CurrentSDK,
    base_url: str,
    *,
    auth_token: str = "fixture-pat",
    project_id: int = 41,
) -> tuple[Any, Any]:
    """Build models through the same SDK client used by the worker adapter."""

    client = current_sdk.client(
        base_url=base_url,
        project_id=project_id,
        auth_token=auth_token,
        model_timeout=5,
    )
    llm = client.get_llm(
        "fixture-vision",
        {
            "max_tokens": 512,
            "max_retries": 0,
            "streaming": False,
            "stream_usage": False,
            "temperature": 0,
        },
    )
    return llm, client.get_embeddings("fixture-embedding")


def load_current_sdk(platform_root: Path) -> CurrentSDK:
    configured = os.getenv("ELITEA_CURRENT_SDK_ROOT")
    if not configured:
        pytest.skip(
            "current SDK source unavailable: set ELITEA_CURRENT_SDK_ROOT to "
            "the exact revision in current_sdk_baseline.json"
        )
    sdk_root = Path(configured).resolve()
    if not (sdk_root / "elitea_sdk").is_dir():
        pytest.skip(
            f"current SDK source unavailable: {sdk_root}/elitea_sdk does not exist"
        )
    _ = platform_root
    lock = json.loads(BASELINE_LOCK.read_text())
    if lock.get("schema_version") != "elitea.current-sdk-baseline.v1":
        pytest.fail("current SDK baseline lock has an unsupported schema")
    expected_revision = lock["source"]["revision"]
    expected_version = lock["distribution_version"]
    revision = subprocess.run(
        ["git", "-C", str(sdk_root), "rev-parse", "HEAD"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    if revision != expected_revision:
        pytest.fail(
            f"SDK source revision drift: got {revision}, want {expected_revision}"
        )
    pyproject = (sdk_root / "pyproject.toml").read_text()
    if f'version = "{expected_version}"' not in pyproject:
        pytest.fail(
            f"SDK source version drift: expected pyproject version {expected_version}"
        )

    sys.path.insert(0, str(sdk_root))
    for module_name in tuple(sys.modules):
        if module_name == "elitea_sdk" or module_name.startswith("elitea_sdk."):
            del sys.modules[module_name]
    wrapper_module = importlib.import_module(
        "elitea_sdk.tools.confluence.api_wrapper"
    )
    loader_module = importlib.import_module("elitea_sdk.tools.confluence.loader")
    client_module = importlib.import_module("elitea_sdk.runtime.clients.client")
    return CurrentSDK(
        root=sdk_root,
        revision=revision,
        version=expected_version,
        client=client_module.EliteAClient,
        confluence_wrapper=wrapper_module.ConfluenceAPIWrapper,
        confluence_loader=loader_module.EliteAConfluenceLoader,
    )
