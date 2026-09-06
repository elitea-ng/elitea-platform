#!/usr/bin/env python3
"""Synchronize Main's built-in toolkit settings projection from the worker SDK.

The current indexer publishes ``elitea_sdk.runtime.toolkits.tools.get_toolkits``
schemas to Main. Main consumes the top-level annotations used to expand
configuration references and derive a stable toolkit name, plus the per-tool
argument schemas it serves to toolkit tool forms. This script runs that exact
SDK registry from the source revision admitted by the worker lock and emits
only those consumed parts.

Deployment-defined MCP servers are intentionally excluded from this immutable
built-in snapshot. They remain actor/project-visible dynamic schemas in Main.

The run emits TWO files from one registry read. The argument-schema snapshot
carries the per-tool argument payload and the settings annotations. The
catalogue snapshot carries each type's settings schema and its metadata: the
label, categories and icon the create page groups the type under. They are
separate files because the first is already 596 KB against a 1 MiB ceiling.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib
import json
import logging
import re
import subprocess
import sys
import tomllib
from collections.abc import Iterable, Mapping
from datetime import date
from pathlib import Path
from typing import Any


REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_SDK_ROOT = REPO_ROOT.parent / "elitea-sdk"
DEFAULT_LOCK = REPO_ROOT / "services" / "elitea-worker-python" / "elitea-sdk.lock.json"
DEFAULT_SNAPSHOT = (
    REPO_ROOT
    / "services"
    / "elitea-main"
    / "internal"
    / "runtimecomposition"
    / "current_toolkit_schema_snapshot.json"
)
DEFAULT_CATALOGUE = (
    REPO_ROOT
    / "services"
    / "elitea-main"
    / "internal"
    / "runtimecomposition"
    / "current_toolkit_catalogue_snapshot.json"
)
SCHEMA_VERSION = "elitea.current-toolkit-schema-snapshot.v1"
CATALOGUE_SCHEMA_VERSION = "elitea.current-toolkit-catalogue-snapshot.v1"
ANNOTATION_FIELDS = (
    "configuration_types",
    "configuration_model",
    "secret",
    "toolkit_name",
)
MAX_TOOLKIT_NAME_LENGTH = 4096
# The SDK publishes each toolkit's per-tool argument schemas as a
# ``json_schema_extra`` payload on the tool-selection field, which Pydantic
# merges verbatim into that property's JSON Schema. Main needs those argument
# schemas to render toolkit tool forms, so they are projected as a sibling of
# ``properties`` instead of being folded into ANNOTATION_FIELDS: the annotation
# projection is a fixed, flat allowlist of scalar hints, while an argument
# schema is an arbitrarily nested JSON Schema document with its own ``$defs``.
TOOL_SELECTION_FIELD = "selected_tools"
ARGUMENT_SCHEMAS_FIELD = "args_schemas"
# One SDK tool builds an argument default from the clock. In
# elitea_sdk/tools/carrier/ui_reports_tool.py the ``current_date`` field reads
# ``datetime.now()`` when Python imports the module, so the projected default
# is the date of the projection run. Such a value cannot enter an immutable
# snapshot: the committed file becomes stale at the next midnight, and the
# --check gate then fails for every later change to this repository. Remove
# the ``default`` key when it holds a date that this run observed, and keep
# the remainder of the schema verbatim. A removed default leaves the field
# empty in the toolkit tool form. The worker then applies the model default,
# which reads the clock of the run and gives the correct current date.
DEFAULT_FIELD = "default"


class ContractSyncError(RuntimeError):
    """The admitted SDK or its projected registry is incomplete or ambiguous."""


def _canonical(value: object) -> bytes:
    return (
        json.dumps(
            value,
            allow_nan=False,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode()
        + b"\n"
    )


def _git(sdk_root: Path, *arguments: str, binary: bool = False) -> bytes | str:
    process = subprocess.run(
        ["git", "-C", str(sdk_root), *arguments],
        check=False,
        capture_output=True,
        text=not binary,
    )
    if process.returncode != 0:
        raise ContractSyncError("SDK Git source is unavailable")
    return process.stdout if binary else process.stdout.strip()


def _package_tree_digest(package_root: Path) -> tuple[int, str]:
    digest = hashlib.sha256()
    paths = sorted(
        path for path in package_root.rglob("*.py") if path.is_file()
    )
    for path in paths:
        relative = path.relative_to(package_root).as_posix().encode()
        content = path.read_bytes()
        digest.update(len(relative).to_bytes(4, "big"))
        digest.update(relative)
        digest.update(len(content).to_bytes(8, "big"))
        digest.update(content)
    return len(paths), digest.hexdigest()


def _require_locked_patch_paths(
    sdk_root: Path,
    patch_revisions: object,
) -> None:
    if not isinstance(patch_revisions, list) or any(
        not isinstance(revision, str) or len(revision) != 40
        for revision in patch_revisions
    ):
        raise ContractSyncError("worker SDK patch lock is invalid")

    expected: set[str] = set()
    for revision in patch_revisions:
        paths = _git(
            sdk_root,
            "diff-tree",
            "--no-commit-id",
            "--name-only",
            "-r",
            revision,
            "--",
            "elitea_sdk",
            "pyproject.toml",
        )
        expected.update(path for path in paths.splitlines() if path)

    actual = _git(
        sdk_root,
        "diff",
        "--name-only",
        "HEAD",
        "--",
        "elitea_sdk",
        "pyproject.toml",
    )
    if {path for path in actual.splitlines() if path} != expected:
        raise ContractSyncError("SDK source changes do not match the worker patch lock")


def _require_sdk_identity(sdk_root: Path, lock_path: Path) -> dict[str, Any]:
    try:
        lock = json.loads(lock_path.read_bytes())
        revision = lock["source"]["revision"]
        version = lock["distribution_version"]
        archive_digest = lock["source"]["git_archive_sha256"]
        patch_revisions = lock["source"].get("patch_revisions", [])
        tree = lock["installed_package_tree"]
    except (OSError, KeyError, TypeError, json.JSONDecodeError) as exc:
        raise ContractSyncError("worker SDK lock is invalid") from exc

    if _git(sdk_root, "rev-parse", "HEAD") != revision:
        raise ContractSyncError("SDK checkout does not match the worker lock")
    _require_locked_patch_paths(sdk_root, patch_revisions)

    try:
        project = tomllib.loads((sdk_root / "pyproject.toml").read_text())
    except (OSError, tomllib.TOMLDecodeError) as exc:
        raise ContractSyncError("SDK project metadata is invalid") from exc
    if project.get("project", {}).get("version") != version:
        raise ContractSyncError("SDK distribution version does not match the worker lock")

    archive = _git(sdk_root, "archive", "--format=tar", "HEAD", binary=True)
    if not isinstance(archive, bytes) or hashlib.sha256(archive).hexdigest() != archive_digest:
        raise ContractSyncError("SDK source archive does not match the worker lock")
    count, digest = _package_tree_digest(sdk_root / "elitea_sdk")
    if count != tree.get("file_count") or digest != tree.get("sha256"):
        raise ContractSyncError("SDK Python package tree does not match the worker lock")
    return lock


def _annotation_projection(
    properties: Mapping[str, Any],
) -> tuple[dict[str, dict[str, Any]], dict[str, Any]]:
    projected: dict[str, dict[str, Any]] = {}
    name_field: str | None = None
    max_length = 0
    for field, raw_schema in properties.items():
        if not isinstance(field, str) or not field or not isinstance(raw_schema, Mapping):
            raise ContractSyncError("toolkit schema contains an invalid property")
        annotations = {
            key: raw_schema[key]
            for key in ANNOTATION_FIELDS
            if key in raw_schema
        }
        if annotations:
            projected[field] = annotations
        if raw_schema.get("toolkit_name") is True and name_field is None:
            name_field = field
        if raw_schema.get("max_toolkit_length") and max_length == 0:
            try:
                max_length = int(raw_schema["max_toolkit_length"])
            except (TypeError, ValueError) as exc:
                raise ContractSyncError("toolkit name limit is invalid") from exc
            if not 0 < max_length <= MAX_TOOLKIT_NAME_LENGTH:
                raise ContractSyncError("toolkit name limit is outside the supported range")
    return projected, {"field": name_field, "max_length": max_length}


def _without_clock_defaults(value: Any, clock_dates: frozenset[str]) -> Any:
    """Remove each schema default that holds a date from the projection run.

    The walk keeps every other key and every other value. It removes only a
    ``default`` that is a string in ``clock_dates``, so ``$ref`` pointers keep
    their targets and no other part of the document moves.
    """

    if isinstance(value, Mapping):
        return {
            key: _without_clock_defaults(item, clock_dates)
            for key, item in value.items()
            if not (
                key == DEFAULT_FIELD
                and isinstance(item, str)
                and item in clock_dates
            )
        }
    if isinstance(value, list):
        return [_without_clock_defaults(item, clock_dates) for item in value]
    return value


def _argument_schema_projection(
    properties: Mapping[str, Any],
    clock_dates: frozenset[str] = frozenset(),
) -> dict[str, Any]:
    """Project each tool's argument schema from the tool-selection property.

    The payload is carried verbatim: an argument schema is a self-contained
    JSON Schema whose internal ``$ref`` pointers resolve against its own
    ``$defs``, so narrowing it the way ``_annotation_projection`` narrows
    annotations would produce documents that no longer describe their own
    inputs. Toolkits without a tool-selection field legitimately publish no
    argument schemas and project an empty mapping; a tool-selection field whose
    payload is present but malformed is a contract break and fails closed.
    """

    selection = properties.get(TOOL_SELECTION_FIELD)
    if selection is None:
        return {}
    if not isinstance(selection, Mapping):
        raise ContractSyncError("toolkit tool selection property is invalid")
    raw_schemas = selection.get(ARGUMENT_SCHEMAS_FIELD)
    if raw_schemas is None:
        return {}
    if not isinstance(raw_schemas, Mapping):
        raise ContractSyncError("toolkit argument schemas are not a tool mapping")

    projected: dict[str, Any] = {}
    for tool_name, schema in raw_schemas.items():
        if not isinstance(tool_name, str) or not tool_name:
            raise ContractSyncError("toolkit argument schema has an invalid tool name")
        if not isinstance(schema, Mapping):
            raise ContractSyncError(
                f"toolkit argument schema for tool {tool_name!r} is not an object"
            )
        stable = _without_clock_defaults(schema, clock_dates)
        # Reject anything the canonical encoder cannot represent here rather
        # than at write time, so the failure names the offending tool.
        try:
            _canonical(stable)
        except (TypeError, ValueError) as exc:
            raise ContractSyncError(
                f"toolkit argument schema for tool {tool_name!r} is not canonical JSON"
            ) from exc
        projected[tool_name] = stable
    # The canonical encoder already sorts keys, but the in-memory document is
    # compared directly by the tests and by --check callers, so keep the
    # mapping ordered here too.
    return {name: projected[name] for name in sorted(projected)}


def project_toolkit_schemas(
    models: Iterable[Any],
    revision: str,
    clock_dates: frozenset[str] = frozenset(),
) -> dict[str, Any]:
    entries: list[dict[str, Any]] = []
    seen: set[str] = set()
    for model in models:
        schema_method = getattr(model, "model_json_schema", None)
        if not callable(schema_method):
            schema_method = getattr(model, "schema", None)
        if not callable(schema_method):
            raise ContractSyncError("toolkit registry contains an invalid model")
        schema = schema_method()
        if not isinstance(schema, Mapping):
            raise ContractSyncError("toolkit model produced a non-object schema")
        type_name = schema.get("title")
        properties = schema.get("properties")
        if (
            not isinstance(type_name, str)
            or not type_name
            or type_name in seen
            or not isinstance(properties, Mapping)
        ):
            raise ContractSyncError("toolkit schema has an invalid or duplicate type")
        seen.add(type_name)
        projected, naming = _annotation_projection(properties)
        entries.append(
            {
                "type": type_name,
                "properties": projected,
                "args_schemas": _argument_schema_projection(properties, clock_dates),
                "naming": naming,
            }
        )
    if not entries:
        raise ContractSyncError("toolkit registry is empty")
    entries.sort(key=lambda entry: entry["type"])
    return {
        "schema_version": SCHEMA_VERSION,
        "sdk_revision": revision,
        "entries": entries,
    }


_SAFE_IMPORT_CALL = re.compile(
    r"^_safe_import_tool\(\s*'(?P<key>[a-z0-9_]+)'\s*,"
    r"\s*'(?P<module>[A-Za-z0-9_.]+)'\s*,"
    r"\s*(?:'[A-Za-z0-9_]+'|None)\s*,"
    r"\s*'(?P<toolkit>[A-Za-z0-9_]+)'\s*\)",
    re.MULTILINE,
)


def import_keys_by_type(sdk_root: Path, tools_module: Any) -> dict[str, str]:
    """Map each toolkit TYPE to the registry import key that carries it.

    Two reads are joined, because neither answers this alone. The registry
    source names ``key -> ToolkitClass``; the imported registry names
    ``ToolkitClass -> class``, and only the class can produce the config model
    whose ``title`` is the platform type. A class whose model cannot be built
    is left out rather than guessed at.
    """

    classes = getattr(tools_module, "AVAILABLE_TOOLKITS", None)
    if not isinstance(classes, Mapping) or not classes:
        raise ContractSyncError("SDK toolkit class registry is unavailable")
    keys_by_class = import_keys_by_toolkit_class(sdk_root)
    keys_by_type: dict[str, str] = {}
    for class_name, toolkit_class in classes.items():
        key = keys_by_class.get(class_name)
        if key is None:
            continue
        schema_method = getattr(toolkit_class, "toolkit_config_schema", None)
        if not callable(schema_method):
            raise ContractSyncError(
                f"SDK toolkit class {class_name!r} declares no config schema"
            )
        title = schema_method().model_json_schema().get("title")
        if not isinstance(title, str) or not title:
            raise ContractSyncError(
                f"SDK toolkit class {class_name!r} produced an unnamed type"
            )
        if keys_by_type.setdefault(title, key) != key:
            raise ContractSyncError(f"toolkit type {title!r} has two import keys")
    if not keys_by_type:
        raise ContractSyncError("SDK toolkit import registry produced no types")
    return keys_by_type


def import_keys_by_toolkit_class(sdk_root: Path) -> dict[str, str]:
    """Read the SDK registry's import key for each toolkit class.

    ``elitea_sdk.tools`` registers every optional toolkit through
    ``_safe_import_tool(key, module, get_tools, ToolkitClass)`` and records a
    failure under ``FAILED_IMPORTS[key]``. The key is therefore the only name a
    deployed worker can report a missing toolkit under, and it is NOT the
    platform type: ``k8s`` is the key of the type ``kubernetes``. Read the keys
    from the registry source, so the projection cannot drift from the SDK.

    A toolkit class that no call names is a runtime toolkit (artifact,
    vectorstore, memory, mcp, sandbox, ...). The SDK imports those
    unconditionally, so they have no key and cannot fail this way.
    """

    source = (sdk_root / "elitea_sdk" / "tools" / "__init__.py").read_text()
    keys: dict[str, str] = {}
    for match in _SAFE_IMPORT_CALL.finditer(source):
        toolkit_class = match.group("toolkit")
        key = match.group("key")
        if keys.setdefault(toolkit_class, key) != key:
            raise ContractSyncError(
                f"SDK toolkit class {toolkit_class!r} has two import keys"
            )
    if not keys:
        raise ContractSyncError("SDK toolkit import registry is empty")
    return keys


def _settings_projection(
    schema: Mapping[str, Any],
    clock_dates: frozenset[str],
) -> dict[str, Any]:
    """Project one toolkit's SETTINGS schema: the form the create page renders.

    Three parts of the model schema are removed, and each removal has a reason.

    ``metadata`` is projected separately, because Main serves it as a sibling
    of the settings and pylon injects two fields into it that the SDK does not
    declare.

    ``properties.selected_tools.args_schemas`` is the per-tool argument
    payload. It already has a home — the argument-schema snapshot beside this
    one, which is 596 KB on its own — and ``ListTypeSchemas`` puts it back on
    the served schema from there. Carrying it twice would double that cost for
    no new information.

    ``$defs`` holds the SDK's own configuration models. Main does not serve
    those: it builds a narrower ``$defs`` from the pinned configuration
    catalogue, keyed by configuration TYPE rather than by Pydantic model name,
    and replaces each configuration property with a ``$ref`` into it (see
    CurrentToolkitSettingsDefinitionCatalog). Every ``$ref`` in these schemas
    comes from a property that carries ``configuration_types``, so dropping the
    block leaves no dangling pointer.
    """

    projected: dict[str, Any] = {}
    for key, value in schema.items():
        if key in ("metadata", "$defs"):
            continue
        projected[key] = value
    properties = projected.get("properties")
    if properties is not None and not isinstance(properties, Mapping):
        raise ContractSyncError("toolkit settings properties are invalid")
    if isinstance(properties, Mapping):
        copied = dict(properties)
        selection = copied.get(TOOL_SELECTION_FIELD)
        if isinstance(selection, Mapping):
            copied[TOOL_SELECTION_FIELD] = {
                field: item
                for field, item in selection.items()
                if field != ARGUMENT_SCHEMAS_FIELD
            }
        projected["properties"] = copied
    stable = _without_clock_defaults(projected, clock_dates)
    try:
        _canonical(stable)
    except (TypeError, ValueError) as exc:
        raise ContractSyncError("toolkit settings schema is not canonical JSON") from exc
    return stable


def _metadata_projection(model: Any, schema: Mapping[str, Any]) -> dict[str, Any]:
    """Project one toolkit's metadata, with the two fields pylon injects.

    ``check_connection_supported`` and ``has_function_validators`` are computed
    by the current ``indexer_worker`` before it emits the registry
    (legacy/plugins/indexer_worker/methods/indexer_toolkits.py), not declared by
    the SDK. Compute them the same way here, so the served metadata is the
    metadata the reference deployment serves.

    ``mcp_config`` declares no metadata at all. It still gets these two fields,
    exactly as the reference does through ``setdefault``.
    """

    raw = schema.get("metadata")
    if raw is not None and not isinstance(raw, Mapping):
        raise ContractSyncError("toolkit metadata is not an object")
    metadata: dict[str, Any] = dict(raw or {})
    decorators = getattr(model, "__pydantic_decorators__", None)
    validators = 0
    for group in ("field_validators", "model_validators"):
        declared = getattr(decorators, group, None)
        if declared:
            validators += len(declared)
    metadata.setdefault("has_function_validators", bool(validators))
    metadata["check_connection_supported"] = hasattr(model, "check_connection")
    try:
        _canonical(metadata)
    except (TypeError, ValueError) as exc:
        raise ContractSyncError("toolkit metadata is not canonical JSON") from exc
    return metadata


def project_toolkit_catalogue(
    models: Iterable[Any],
    revision: str,
    import_keys: Mapping[str, str],
    clock_dates: frozenset[str] = frozenset(),
) -> dict[str, Any]:
    """Project the settings schema and metadata of every built-in toolkit type.

    This is the second projection of the same registry read, and it is a
    SEPARATE file on purpose. The argument-schema snapshot is 596 KB against a
    1 MiB ceiling that its own header asks nobody to raise casually; folding the
    settings and metadata into it would spend that headroom on a payload with a
    different lifetime and a different consumer.
    """

    entries: list[dict[str, Any]] = []
    seen: set[str] = set()
    for model in models:
        schema_method = getattr(model, "model_json_schema", None)
        if not callable(schema_method):
            schema_method = getattr(model, "schema", None)
        if not callable(schema_method):
            raise ContractSyncError("toolkit registry contains an invalid model")
        schema = schema_method()
        if not isinstance(schema, Mapping):
            raise ContractSyncError("toolkit model produced a non-object schema")
        type_name = schema.get("title")
        if not isinstance(type_name, str) or not type_name or type_name in seen:
            raise ContractSyncError("toolkit schema has an invalid or duplicate type")
        seen.add(type_name)
        entries.append(
            {
                "type": type_name,
                "import_key": import_keys.get(type_name),
                "metadata": _metadata_projection(model, schema),
                "settings": _settings_projection(schema, clock_dates),
            }
        )
    if not entries:
        raise ContractSyncError("toolkit registry is empty")
    entries.sort(key=lambda entry: entry["type"])
    return {
        "schema_version": CATALOGUE_SCHEMA_VERSION,
        "sdk_revision": revision,
        "entries": entries,
    }


def generate_document(
    sdk_root: Path,
    lock_path: Path,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Return the argument-schema snapshot and the catalogue snapshot.

    Both come from ONE registry read. The SDK import is the expensive part of
    this script and the registry must be identical in the two files, so the
    projections cannot be split into two commands that import it twice.
    """

    lock = _require_sdk_identity(sdk_root, lock_path)

    sys.path.insert(0, str(sdk_root))
    logging.getLogger("elitea_sdk.tools").setLevel(logging.ERROR)
    try:
        # Read the date before the import and again after the registry call.
        # The SDK evaluates its clock default between these two points. A run
        # that crosses midnight observes two dates, and both count as volatile.
        clock_dates = {date.today().isoformat()}
        module = importlib.import_module("elitea_sdk.runtime.toolkits.tools")
        module_path = Path(module.__file__ or "").resolve()
        try:
            module_path.relative_to(sdk_root.resolve())
        except ValueError as exc:
            raise ContractSyncError("SDK import resolved outside the selected checkout") from exc
        dynamic_mcp_loader = module.get_mcp_config_toolkit_schemas
        module.get_mcp_config_toolkit_schemas = lambda: []
        try:
            models = module.get_toolkits()
        finally:
            module.get_mcp_config_toolkit_schemas = dynamic_mcp_loader
        tools_module = importlib.import_module("elitea_sdk.tools")
        failed_imports = getattr(tools_module, "FAILED_IMPORTS", {})
        unexpected_failures = set(failed_imports) - {"inventory"}
        if unexpected_failures:
            raise ContractSyncError(
                "SDK toolkit imports failed: " + ", ".join(sorted(unexpected_failures))
            )
        import_keys = import_keys_by_type(sdk_root, tools_module)
        clock_dates.add(date.today().isoformat())
        stable_clock_dates = frozenset(clock_dates)
        return (
            project_toolkit_schemas(
                models,
                lock["source"]["revision"],
                stable_clock_dates,
            ),
            project_toolkit_catalogue(
                models,
                lock["source"]["revision"],
                import_keys,
                stable_clock_dates,
            ),
        )
    finally:
        sys.path.pop(0)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sdk-root", type=Path, default=DEFAULT_SDK_ROOT)
    parser.add_argument("--lock", type=Path, default=DEFAULT_LOCK)
    parser.add_argument("--snapshot", type=Path, default=DEFAULT_SNAPSHOT)
    parser.add_argument("--catalogue", type=Path, default=DEFAULT_CATALOGUE)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    try:
        document, catalogue = generate_document(
            args.sdk_root.resolve(), args.lock.resolve()
        )
        # Both files are written, and both are checked, by this one command.
        # A second command for the catalogue would import the SDK a second
        # time, and a caller that ran only one of them would leave the two
        # projections describing different registries.
        outputs = ((args.snapshot, document), (args.catalogue, catalogue))
        if args.check:
            for path, projected in outputs:
                if not path.is_file() or path.read_bytes() != _canonical(projected):
                    raise ContractSyncError(f"{path.name} is stale")
            print(
                "current toolkit schema snapshot is current "
                f"({len(document['entries'])} entries) and "
                "current toolkit catalogue snapshot is current "
                f"({len(catalogue['entries'])} entries)"
            )
            return 0
        for path, projected in outputs:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(_canonical(projected))
            print(f"updated {path} ({len(projected['entries'])} entries)")
        return 0
    except (ContractSyncError, OSError, TypeError, ValueError) as exc:
        print(f"current toolkit schema sync failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
