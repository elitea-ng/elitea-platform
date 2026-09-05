#!/usr/bin/env python3
"""Synchronize SDK-owned configuration schemas into the pinned catalog."""

from __future__ import annotations

import argparse
import copy
import importlib
import json
import logging
import subprocess
import sys
from collections.abc import Mapping
from pathlib import Path
from typing import Any


REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_SDK_ROOT = REPO_ROOT.parent / "elitea-sdk"
DEFAULT_SNAPSHOT = (
    REPO_ROOT
    / "services"
    / "elitea-main"
    / "internal"
    / "application"
    / "configurations"
    / "current_available_snapshot.json"
)
EXPECTED_SDK_REVISION = "b5113a129329b85d23c2d5c2bf55f18e307414ec"
# SDK 0.9.8 preserves the SDK-owned configuration projection byte-for-byte, so
# its independently versioned source identity remains at the producing commit.
SDK_CONFIGURATION_CATALOG_REVISION = "a78d3654f99d8ff89ca7233f20a66d676e564f79"
EXPECTED_SDK_ENTRY_COUNT = 32
# 17 platform-owned entries plus the three gateway-dispatchable credential
# types (anthropic, open_ai_azure, vllm). Those three carry no SDK model and
# no LiteLLM registration: the LLM data plane is the Bifrost gateway, and
# providerConfigTypes in its account package is the source of their shape.
EXPECTED_NON_SDK_ENTRY_COUNT = 20
SNAPSHOT_SCHEMA_VERSION = "elitea.current-configuration-available-snapshot.v1"
SDK_ENTRY_ANCHOR = "github"
NEW_SDK_ENTRY = "aha"
SDK_VALIDATION_FUNC = "applications_configuration_validator"
SDK_CHECK_CONNECTION_FUNC = "applications_configuration_check_connection"
DOCUMENT_FIELDS = {"schema_version", "sources", "dynamic_sources", "entries"}
ENTRY_FIELDS = {
    "type",
    "section",
    "config_schema",
    "has_test_connection",
    "check_connection_label",
    "validation_func",
    "check_connection_func",
}


class ContractSyncError(RuntimeError):
    """The source registry or aggregate snapshot is incomplete or ambiguous."""


def canonical_json(value: object) -> bytes:
    return (
        json.dumps(
            value,
            sort_keys=True,
            separators=(",", ":"),
            ensure_ascii=False,
            allow_nan=False,
        )
        + "\n"
    ).encode("utf-8")


def _reject_duplicate_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ContractSyncError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def _reject_non_finite(value: str) -> None:
    raise ContractSyncError(f"non-finite JSON number: {value}")


def load_document(raw: bytes) -> dict[str, Any]:
    try:
        value = json.loads(
            raw,
            object_pairs_hook=_reject_duplicate_object,
            parse_constant=_reject_non_finite,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ContractSyncError("snapshot is not valid UTF-8 JSON") from exc
    if not isinstance(value, dict):
        raise ContractSyncError("snapshot root must be an object")
    return value


def _run_git(sdk_root: Path, *arguments: str) -> str:
    try:
        result = subprocess.run(
            ["git", "-C", str(sdk_root), *arguments],
            check=True,
            capture_output=True,
            text=True,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        raise ContractSyncError(f"cannot inspect SDK Git state: {' '.join(arguments)}") from exc
    return result.stdout.strip()


def sdk_revision(sdk_root: Path) -> str:
    revision = _run_git(sdk_root, "rev-parse", "HEAD")
    if revision != EXPECTED_SDK_REVISION:
        raise ContractSyncError(
            f"SDK revision is {revision}, expected pinned {EXPECTED_SDK_REVISION}"
        )
    dirty = _run_git(
        sdk_root,
        "status",
        "--porcelain",
        "--untracked-files=all",
        "--",
        "elitea_sdk/configurations",
    )
    if dirty:
        raise ContractSyncError("SDK configurations source has uncommitted files")
    return revision


def import_sdk_registry(sdk_root: Path) -> dict[str, type[Any]]:
    expected_package = (sdk_root / "elitea_sdk" / "configurations").resolve()
    # Some configuration imports transitively load the toolkit registry. Its
    # optional-tool warning is unrelated to this stricter configuration gate.
    logging.getLogger("elitea_sdk.tools").setLevel(logging.ERROR)
    sys.path.insert(0, str(sdk_root))
    try:
        module = importlib.import_module("elitea_sdk.configurations")
    finally:
        sys.path.pop(0)

    module_file = Path(module.__file__ or "").resolve()
    if module_file.parent != expected_package:
        raise ContractSyncError(f"imported SDK configurations from unexpected path: {module_file}")

    failed_imports = getattr(module, "FAILED_IMPORTS", None)
    if not isinstance(failed_imports, Mapping):
        raise ContractSyncError("SDK FAILED_IMPORTS contract is unavailable")
    if failed_imports:
        names = ", ".join(sorted(str(name) for name in failed_imports))
        raise ContractSyncError(f"SDK configuration imports failed: {names}")

    getter = getattr(module, "get_class_configurations", None)
    if not callable(getter):
        raise ContractSyncError("SDK class registry getter is unavailable")
    registry = getter()
    if not isinstance(registry, Mapping) or not registry:
        raise ContractSyncError("SDK class registry is empty or invalid")

    result: dict[str, type[Any]] = {}
    for type_name, model in registry.items():
        if not isinstance(type_name, str) or not type_name or type_name in result:
            raise ContractSyncError("SDK registry contains an invalid or duplicate type")
        if not callable(getattr(model, "model_json_schema", None)):
            raise ContractSyncError(f"SDK type {type_name} has no model_json_schema")
        result[type_name] = model
    return result


def _outer_schema_template(entry: Mapping[str, Any]) -> dict[str, Any]:
    schema = copy.deepcopy(entry.get("config_schema"))
    if not isinstance(schema, dict):
        raise ContractSyncError("SDK anchor has no object config_schema")
    properties = schema.get("properties")
    if not isinstance(properties, dict) or "data" not in properties:
        raise ContractSyncError("SDK anchor has no outer data schema")
    properties["data"] = {}
    schema["title"] = ""
    return schema


def _validate_document_shape(document: Mapping[str, Any]) -> list[dict[str, Any]]:
    if set(document) != DOCUMENT_FIELDS:
        raise ContractSyncError("snapshot has missing or unknown top-level fields")
    if document.get("schema_version") != SNAPSHOT_SCHEMA_VERSION:
        raise ContractSyncError("snapshot schema version is not supported")
    if not isinstance(document.get("sources"), dict) or "elitea_sdk" not in document["sources"]:
        raise ContractSyncError("snapshot has no elitea_sdk source revision")
    if not isinstance(document.get("dynamic_sources"), dict):
        raise ContractSyncError("snapshot has no dynamic source map")

    entries = document.get("entries")
    if not isinstance(entries, list) or not entries:
        raise ContractSyncError("snapshot entries are empty or invalid")
    seen: set[str] = set()
    for entry in entries:
        if not isinstance(entry, dict) or set(entry) != ENTRY_FIELDS:
            raise ContractSyncError("snapshot entry has missing or unknown fields")
        type_name = entry.get("type")
        if not isinstance(type_name, str) or not type_name or type_name in seen:
            raise ContractSyncError("snapshot contains an invalid or duplicate type")
        seen.add(type_name)
    return entries


def _sdk_entry(
    type_name: str,
    model: type[Any],
    outer_template: Mapping[str, Any],
) -> dict[str, Any]:
    schema = model.model_json_schema()
    if not isinstance(schema, dict):
        raise ContractSyncError(f"SDK type {type_name} produced a non-object schema")
    schema = copy.deepcopy(schema)
    metadata = schema.setdefault("metadata", {})
    if not isinstance(metadata, dict):
        raise ContractSyncError(f"SDK type {type_name} metadata is not an object")
    if metadata.get("type") != type_name:
        raise ContractSyncError(f"SDK type {type_name} metadata.type does not match")
    section = metadata.get("section")
    if not isinstance(section, str) or not section:
        raise ContractSyncError(f"SDK type {type_name} has no metadata.section")
    title = metadata.get("label", schema.get("title"))
    if not isinstance(title, str) or not title:
        raise ContractSyncError(f"SDK type {type_name} has no display title")

    # This is intentionally identical to the current indexer projection.
    supports_check = hasattr(model, "check_connection")
    metadata["check_connection_supported"] = supports_check

    config_schema = copy.deepcopy(outer_template)
    config_schema["properties"]["data"] = schema
    config_schema["title"] = title
    check_label = metadata.get("check_connection_label")
    if check_label is not None and not isinstance(check_label, str):
        raise ContractSyncError(f"SDK type {type_name} has an invalid check label")
    return {
        "type": type_name,
        "section": section,
        "config_schema": config_schema,
        "has_test_connection": supports_check,
        "check_connection_label": check_label,
        "validation_func": SDK_VALIDATION_FUNC,
        "check_connection_func": SDK_CHECK_CONNECTION_FUNC if supports_check else None,
    }


def synchronize_document(
    source: Mapping[str, Any],
    registry: Mapping[str, type[Any]],
    revision: str,
) -> dict[str, Any]:
    document = copy.deepcopy(source)
    entries = _validate_document_shape(document)
    registry_names = list(registry)
    if not registry_names or registry_names[0] != SDK_ENTRY_ANCHOR:
        raise ContractSyncError("SDK registry no longer starts at the pinned anchor")

    entry_names = [entry["type"] for entry in entries]
    try:
        anchor_index = entry_names.index(SDK_ENTRY_ANCHOR)
    except ValueError as exc:
        raise ContractSyncError("aggregate snapshot has no SDK ownership anchor") from exc

    non_sdk_entries = entries[:anchor_index]
    old_sdk_entries = entries[anchor_index:]
    if any(entry["type"] in registry for entry in non_sdk_entries):
        raise ContractSyncError("SDK type collides with a non-SDK aggregate entry")
    old_sdk_names = [entry["type"] for entry in old_sdk_entries]
    allowed_sdk_suffixes = [registry_names]
    if registry_names[-1] == NEW_SDK_ENTRY:
        allowed_sdk_suffixes.append(registry_names[:-1])
    if old_sdk_names not in allowed_sdk_suffixes:
        raise ContractSyncError("existing SDK suffix is partial, reordered, or ambiguous")

    outer_template = _outer_schema_template(old_sdk_entries[0])
    for entry in old_sdk_entries:
        if entry.get("validation_func") != SDK_VALIDATION_FUNC:
            raise ContractSyncError("SDK suffix contains a non-SDK validation contract")
        if entry.get("check_connection_func") not in {None, SDK_CHECK_CONNECTION_FUNC}:
            raise ContractSyncError("SDK suffix contains a non-SDK connection contract")
        if _outer_schema_template(entry) != outer_template:
            raise ContractSyncError("SDK suffix does not share one outer create schema")

    document["entries"] = copy.deepcopy(non_sdk_entries) + [
        _sdk_entry(type_name, model, outer_template)
        for type_name, model in registry.items()
    ]
    document["sources"]["elitea_sdk"] = revision
    return document


def _parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sdk-root", type=Path, default=DEFAULT_SDK_ROOT)
    parser.add_argument("--snapshot", type=Path, default=DEFAULT_SNAPSHOT)
    parser.add_argument(
        "--check",
        action="store_true",
        help="fail when the checked-in snapshot differs instead of writing it",
    )
    return parser.parse_args()


def main() -> int:
    args = _parse_args()
    try:
        raw = args.snapshot.read_bytes()
        source = load_document(raw)
        sdk_revision(args.sdk_root.resolve())
        registry = import_sdk_registry(args.sdk_root.resolve())
        if len(registry) != EXPECTED_SDK_ENTRY_COUNT:
            raise ContractSyncError(
                f"SDK registry has {len(registry)} entries, expected {EXPECTED_SDK_ENTRY_COUNT}"
            )
        synchronized = synchronize_document(
            source,
            registry,
            SDK_CONFIGURATION_CATALOG_REVISION,
        )
        if len(synchronized["entries"]) != EXPECTED_NON_SDK_ENTRY_COUNT + EXPECTED_SDK_ENTRY_COUNT:
            raise ContractSyncError("aggregate snapshot does not contain the expected 52 entries")
        rendered = canonical_json(synchronized)
        if args.check:
            if raw != rendered:
                raise ContractSyncError(
                    "snapshot is stale; run sync_current_available_sdk.py without --check"
                )
            print(f"configuration catalog is current ({len(registry)} SDK, 52 total)")
            return 0
        args.snapshot.write_bytes(rendered)
        print(f"updated {args.snapshot} ({len(registry)} SDK, 52 total)")
        return 0
    except (ContractSyncError, OSError, ValueError, TypeError) as exc:
        print(f"configuration catalog sync failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
