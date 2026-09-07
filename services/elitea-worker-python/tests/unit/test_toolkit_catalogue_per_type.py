"""One case per SERVED toolkit type, against the admitted SDK.

WHY THIS FILE EXISTS. ``elitea-main`` serves fifty-two SDK toolkit types and
hides the ones this worker cannot run, using a committed capability snapshot
this worker writes
(``services/elitea-main/internal/runtimecomposition/current_python_worker_toolkit_capability_snapshot.json``).
Before this file, the only test of that join was
``test_toolkit_capability_snapshot.py``'s four hand-picked names. The snapshot
records IMPORT KEYS and the catalogue records TYPES, and the two differ for at
least one toolkit — the SDK registers Kubernetes as ``k8s`` and publishes it as
the type ``kubernetes``. A join written on the wrong side of that pair silently
never matches, and the visible result is a tile offered by a deployment that
cannot run it.

WHAT IS ASSERTED, per served type:

1. it resolves to exactly one of three states — importable, import-failed, or a
   runtime toolkit the SDK registers no import for — and that state is the one
   the committed snapshot claims;
2. an importable type carries the ``get_toolkit`` factory
   ``elitea_sdk.tools.instantiate_toolkit`` calls, i.e. the ``build_toolkit``
   equivalent really is reachable for it rather than merely named;
3. the worker's own available-tools handler ANSWERS for it, with a settings body
   derived from that type's published schema, without raising and without
   echoing the credential it was given.

WHAT IS DELIBERATELY NOT ASSERTED. The enumerator answers the empty shape
``{"tools": [], "args_schemas": {}}`` for every type in all three states when it
is handed synthetic credentials — measured, not assumed. An importable toolkit
with a fake token and a toolkit whose module never imported are therefore
INDISTINGUISHABLE from the answer alone, which is why assertion 1 goes through
the registry and not through the answer. Asserting "the tool list is non-empty"
here would either fail everywhere or, with a lenient comparison, pass for a
worker with no SDK at all.

RUNNING IT. The registry assertions need the exact pinned SDK; see
ci-python.yml's ``runtime-worker`` job for the checkout and single-pass install.
Without it the module-level import fails, which is the correct outcome: a
skipped registry check reads identically to a passing one.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from elitea_worker.agents.sdk_adapter import EliteaSdkToolkitAdapter
from elitea_worker.handlers.toolkit_available_tools import (
    ToolkitAvailableToolsHandler,
    ToolkitAvailableToolsRequest,
)
from elitea_worker.toolkit_capabilities import EXPECTED_ABSENT_IMPORT_KEYS, unsupported_import_keys

_SERVICE_ROOT = Path(__file__).resolve().parents[2]
_PLATFORM_ROOT = _SERVICE_ROOT.parents[1]
_RUNTIME_COMPOSITION = (
    _PLATFORM_ROOT / "services/elitea-main/internal/runtimecomposition"
)
_CATALOGUE_PATH = _RUNTIME_COMPOSITION / "current_toolkit_catalogue_snapshot.json"
_CAPABILITY_PATH = (
    _RUNTIME_COMPOSITION / "current_python_worker_toolkit_capability_snapshot.json"
)

# A value no toolkit can mistake for a real credential and that no answer may
# echo back. The parity suite uses the same idea for the same reason.
_CANARY = "TEST_ONLY_CREDENTIAL_CANARY_NOT_A_SECRET"


def _load(path: Path) -> Any:
    return json.loads(path.read_text(encoding="utf-8"))


def _catalogue_entries() -> list[dict[str, Any]]:
    entries = _load(_CATALOGUE_PATH)["entries"]
    assert entries, "the served toolkit catalogue snapshot is empty"
    return entries


_ENTRIES = _catalogue_entries()
_TYPES = [entry["type"] for entry in _ENTRIES]
_IMPORT_KEY_BY_TYPE = {entry["type"]: entry["import_key"] for entry in _ENTRIES}
# A type is OFFERED when the SDK gives it a label and does not hide it — the
# same rule elitea-main's catalogue applies before it serves a tile.
_OFFERED_TYPES = {
    entry["type"]: bool(entry["metadata"].get("label"))
    and entry["metadata"].get("hidden") is not True
    for entry in _ENTRIES
}
# Importable types the SDK registers with no way to construct them. Both are
# served hidden, so no create form reaches them; recorded rather than waived so
# that a THIRD one has to be added here deliberately.
_KNOWN_UNCONSTRUCTIBLE = {"elastic", "keycloak"}


class _MissingValue:
    """Sentinel for a schema shape this derivation does not understand."""


_MISSING = _MissingValue()


def _schema_value(schema: dict[str, Any]) -> Any:
    """One settings value for one property schema.

    Mirrors the Go side's ``schemaValueFixture``
    (services/elitea-main/internal/api/v2/toolkits/per_type_lifecycle_test.go)
    so both suites drive the same body shape. Returning ``_MISSING`` is not a
    silent skip: the caller turns it into a named failure, which is what makes
    "every type has a fixture" a gate rather than a claim.
    """

    if "$ref" in schema:
        return {"id": 1, "name": "autotest-credential"}
    enum = schema.get("enum")
    if isinstance(enum, list) and enum:
        return enum[0]
    branches = schema.get("anyOf")
    if isinstance(branches, list):
        for branch in branches:
            if not isinstance(branch, dict) or branch.get("type") == "null":
                continue
            value = _schema_value(branch)
            if value is not _MISSING:
                return value
        return _MISSING
    kind = schema.get("type")
    if kind == "string":
        return _CANARY if schema.get("secret") or schema.get("format") == "password" else "autotest-value"
    if kind == "integer":
        floor = max(int(schema.get("minimum", 1)), int(schema.get("exclusiveMinimum", 0)) + 1)
        return max(1, floor)
    if kind == "number":
        return 1.0
    if kind == "boolean":
        return True
    if kind == "array":
        return []
    if kind == "object":
        return {}
    return _MISSING


def _settings_fixture(entry: dict[str, Any]) -> tuple[dict[str, Any], list[str]]:
    properties = (entry["settings"].get("properties") or {})
    settings: dict[str, Any] = {}
    missing: list[str] = []
    for name in sorted(properties):
        schema = properties[name]
        if not isinstance(schema, dict):
            missing.append(f"{entry['type']}.{name}: property is not an object")
            continue
        if name == "selected_tools":
            settings[name] = []
            continue
        value = _schema_value(schema)
        if value is _MISSING:
            missing.append(f"{entry['type']}.{name}: no value for {json.dumps(schema)[:120]}")
            continue
        settings[name] = value
    if not properties:
        missing.append(f"{entry['type']}: the published schema declares no properties")
    return settings, missing


_FIXTURES = {entry["type"]: _settings_fixture(entry) for entry in _ENTRIES}


def _request(toolkit_type: str, settings: dict[str, Any]) -> ToolkitAvailableToolsRequest:
    """The bundle identity every request carries. Fixed here: this file varies
    the TYPE and the SETTINGS, and a varying bundle id would put noise in the
    one field a failure has to be read by."""

    return ToolkitAvailableToolsRequest(
        toolkit_type=toolkit_type,
        input_bundle_id="per-type-bundle",
        input_bundle_digest=b"b" * 32,
        settings_entry_id="per-type-settings",
        settings_entry_version="1",
        settings_content_digest=b"s" * 32,
        settings=settings,
    )


@pytest.fixture(scope="module")
def sdk() -> EliteaSdkToolkitAdapter:
    return EliteaSdkToolkitAdapter()


@pytest.fixture(scope="module")
def registry() -> Any:
    import elitea_sdk.tools as tools

    return tools


def test_every_served_type_has_a_settings_fixture() -> None:
    """The gate the per-type cases depend on.

    Kept separate so a schema shape the derivation cannot read reports once,
    naming every offending property, instead of as fifty-two case failures.
    """

    gaps = [problem for _, problems in _FIXTURES.values() for problem in problems]
    assert not gaps, (
        f"{len(gaps)} served toolkit type(s) have no derivable settings fixture:\n  "
        + "\n  ".join(gaps)
    )
    # A catalogue that collapsed back to a handful of types would otherwise
    # satisfy every per-type case below by having a handful of cases.
    assert len(_TYPES) >= 52, f"the served catalogue holds only {len(_TYPES)} types: {_TYPES}"


def test_the_capability_snapshot_matches_this_worker(registry: Any) -> None:
    """The committed deny-set is the one this SDK really produces.

    ``unsupported_import_keys()`` reads the LIVE registry; the snapshot is the
    committed copy elitea-main embeds. When they disagree the platform hides the
    wrong tiles, and nothing on either side reports it.
    """

    published = _load(_CAPABILITY_PATH)
    assert published["implementation"] == "python"
    assert sorted(published["unsupported_import_keys"]) == sorted(unsupported_import_keys()), (
        "the committed python worker capability snapshot disagrees with the installed SDK."
        " Regenerate it from elitea_worker.toolkit_capabilities.toolkit_capability_json."
    )
    # The expected-absent keys are excluded from the deny-set on purpose; a
    # regeneration that started publishing them would hide a type that works.
    for key in EXPECTED_ABSENT_IMPORT_KEYS:
        assert key not in published["unsupported_import_keys"]
    assert registry.FAILED_IMPORTS, "the SDK reports no failed imports at all"


@pytest.mark.parametrize("toolkit_type", _TYPES)
def test_every_served_type_resolves_to_one_registry_state(
    toolkit_type: str, registry: Any
) -> None:
    """Importable, import-failed, or runtime — exactly one, and it is the
    state the committed snapshot claims."""

    import_key = _IMPORT_KEY_BY_TYPE[toolkit_type] or toolkit_type
    importable = import_key in registry.AVAILABLE_TOOLS
    failed = import_key in registry.FAILED_IMPORTS
    runtime_only = _IMPORT_KEY_BY_TYPE[toolkit_type] is None and not importable

    assert sum((importable, failed, runtime_only)) == 1, (
        f"{toolkit_type} (import key {import_key!r}) is in"
        f" {'AVAILABLE_TOOLS ' if importable else ''}{'FAILED_IMPORTS ' if failed else ''}"
        f"{'neither' if runtime_only else ''} — a type must resolve to exactly one state"
    )

    denied = _load(_CAPABILITY_PATH)["unsupported_import_keys"]
    assert failed == (import_key in denied), (
        f"{toolkit_type}: the SDK says failed={failed} and the committed capability"
        f" snapshot says denied={import_key in denied}. This is the k8s/kubernetes"
        " trap — the join runs on the IMPORT KEY, never on the type."
    )

    if not importable:
        return

    entry = registry.AVAILABLE_TOOLS[import_key]
    assert entry.get("toolkit_class") is not None, (
        f"{toolkit_type} is registered with no toolkit class"
    )
    # The build_toolkit equivalent. instantiate_toolkit() calls ``get_toolkit``;
    # the enumeration path calls ``get_tools``. A type with NEITHER cannot be
    # constructed by any caller, whatever the registry says about it — measured
    # at SDK b5113a1, ``elastic`` and ``keycloak`` are in exactly that state and
    # both are served hidden, so no tile offers them. The assertion is therefore
    # conditional on the catalogue OFFERING the type, which makes it fail the
    # day one of those two is unhidden without gaining a factory.
    constructible = callable(entry.get("get_toolkit")) or callable(entry.get("get_tools"))
    if _OFFERED_TYPES.get(toolkit_type, False):
        assert constructible, (
            f"{toolkit_type} is offered as a creatable tile and its SDK registry entry"
            " carries neither get_toolkit nor get_tools, so nothing can build it"
        )
    elif not constructible:
        assert toolkit_type in _KNOWN_UNCONSTRUCTIBLE, (
            f"{toolkit_type} has no construction entry point and is not in the"
            f" recorded set {sorted(_KNOWN_UNCONSTRUCTIBLE)}. Add it with a reason,"
            " or give the toolkit a factory."
        )


@pytest.mark.parametrize("toolkit_type", _TYPES)
def test_every_served_type_answers_the_available_tools_handler(
    toolkit_type: str, sdk: EliteaSdkToolkitAdapter
) -> None:
    """The worker's handler carries every served type without raising.

    The answer's CONTENT is not asserted (see the module docstring): with
    synthetic credentials the SDK answers the empty shape for every type in all
    three registry states. What is asserted is that the call completes, that it
    produces the documented shape, and that no credential the fixture supplied
    comes back in the artifact.
    """

    settings, problems = _FIXTURES[toolkit_type]
    assert not problems, problems

    handler = ToolkitAvailableToolsHandler(sdk)
    result = handler.execute(_request(toolkit_type, settings))
    payload = json.loads(result.artifact.content)

    assert isinstance(payload.get("tools"), list), f"{toolkit_type}: no tools list"
    assert isinstance(payload.get("args_schemas"), dict), f"{toolkit_type}: no args_schemas object"
    assert _CANARY.encode("utf-8") not in result.artifact.content, (
        f"{toolkit_type}: the available-tools answer echoed the credential it was given"
    )


def test_the_openapi_type_still_reports_its_own_parse_failure(
    sdk: EliteaSdkToolkitAdapter,
) -> None:
    """The one type whose enumerator distinguishes a bad body from an empty one.

    ``openapi`` builds its tool list by PARSING the spec the settings carry, so
    a fixture-shaped spec produces an ``error`` key while every other type
    answers the bare empty shape. It is asserted because it is the only evidence
    in this file that a non-empty answer path exists at all — without it, a
    handler that returned a hardcoded empty shape would pass every case above.
    """

    settings, _ = _FIXTURES["openapi"]
    handler = ToolkitAvailableToolsHandler(sdk)
    payload = json.loads(handler.execute(_request("openapi", settings)).artifact.content)
    assert "error" in payload, (
        "openapi answered no error for an unparseable spec; the SDK's own error"
        " passthrough is the only content this suite can tell apart from silence"
    )
