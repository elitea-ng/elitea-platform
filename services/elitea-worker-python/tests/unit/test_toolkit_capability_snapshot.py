"""Gate the Python worker toolkit capability snapshot that Main embeds.

Main decides whether a toolkit type is offered as creatable. It cannot import
the SDK, so it reads a committed record of which SDK toolkits THIS image can
build. A record nobody re-measures becomes a claim: the extras list moves, an
import starts or stops working, and Main keeps offering the old answer.

This test re-measures it. It runs in the job that installs the worker's own
extras — ``[agent-current,indexing-current,test]``, the image profile — so
``FAILED_IMPORTS`` here is the set the deployed worker reports. A run in a
different environment measures a different image, and this test says so rather
than rewriting the record.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from elitea_worker.toolkit_capabilities import (
    SCHEMA_VERSION,
    toolkit_capability_document,
    unsupported_import_keys,
)

_SERVICE_ROOT = Path(__file__).resolve().parents[2]
_LOCK_PATH = _SERVICE_ROOT / "elitea-sdk.lock.json"
_SNAPSHOT_PATH = (
    _SERVICE_ROOT.parent
    / "elitea-main"
    / "internal"
    / "runtimecomposition"
    / "current_python_worker_toolkit_capability_snapshot.json"
)


def _revision() -> str:
    return json.loads(_LOCK_PATH.read_bytes())["source"]["revision"]


def test_snapshot_file_is_present() -> None:
    assert _SNAPSHOT_PATH.is_file(), (
        f"{_SNAPSHOT_PATH} is missing. Main embeds it, so its absence is a "
        "build break rather than a skipped assertion."
    )


def test_snapshot_matches_this_image() -> None:
    committed = json.loads(_SNAPSHOT_PATH.read_bytes())
    measured = toolkit_capability_document(_revision())
    assert committed == measured, (
        "the committed Python worker toolkit capability snapshot does not "
        "match this environment. Regenerate it from an environment that "
        "installs services/elitea-worker-python[agent-current,indexing-current]."
    )


def test_schema_version_and_revision_are_pinned() -> None:
    committed = json.loads(_SNAPSHOT_PATH.read_bytes())
    assert committed["schema_version"] == SCHEMA_VERSION
    assert committed["implementation"] == "python"
    assert committed["sdk_revision"] == _revision()


def test_measured_keys_are_sorted_and_exclude_inventory() -> None:
    keys = unsupported_import_keys()
    assert list(keys) == sorted(keys)
    assert "inventory" not in keys


def test_indexing_families_are_all_importable() -> None:
    """The 17 build-gated indexing families must never be unsupported.

    ``indexing_capability_profile.required_sdk_tool_import_keys`` already fails
    the worker at boot when one of these cannot import. Assert it here too, so
    a change to the extras that breaks indexing is named by this file rather
    than by a container that refuses to start.
    """

    profile = json.loads(_LOCK_PATH.read_bytes())["indexing_capability_profile"]
    required = set(profile["required_sdk_tool_import_keys"])
    assert not required.intersection(unsupported_import_keys())


@pytest.mark.parametrize("key", ("github", "jira", "confluence", "openapi"))
def test_load_bearing_toolkits_stay_supported(key: str) -> None:
    """Four types the product journeys create must remain buildable.

    They are asserted by name because a set comparison passes whatever the set
    holds. These four have end-to-end journeys behind them.
    """

    assert key not in unsupported_import_keys()
