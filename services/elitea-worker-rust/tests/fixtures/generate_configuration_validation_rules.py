#!/usr/bin/env python3
"""Regenerate the Rust worker's configuration.validate.v1 rule table and the
differential corpus that proves it matches the pinned SDK's pydantic models.

The Python worker answers configuration.validate.v1 by calling
``model_validate`` on the registered SDK configuration model, so "valid" means
exactly what pydantic accepts. The Rust worker cannot import pydantic. It
instead carries a small data table derived from each model's JSON schema plus
the two ``model_validator(mode="before")`` hooks, and this script both writes
that table and records, per case, what pydantic itself says.

Run it against an export of the pinned SDK (b5113a1) with the same pydantic the
admitted catalog digests were computed under:

  git -C <elitea-sdk> archive b5113a1 elitea_sdk/configurations | tar -x -C <root>
  touch <root>/elitea_sdk/__init__.py
  uv run --python 3.13 --with pydantic==2.12.5 --with langchain-core \
      --with langgraph --no-project python \
      services/elitea-worker-rust/tests/fixtures/generate_configuration_validation_rules.py \
      --sdk-root <root>

It refuses to write anything unless every recomputed schema digest and the
catalog digest equal the values Main embeds
(services/elitea-main/internal/runtimecomposition/
current_sdk_configuration_catalog_snapshot.json), so a drifted SDK or pydantic
cannot silently produce a table for a different catalog.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
WORKER = HERE.parent.parent
PLATFORM = WORKER.parent.parent
SNAPSHOT = (
    PLATFORM
    / "services/elitea-main/internal/runtimecomposition"
    / "current_sdk_configuration_catalog_snapshot.json"
)
RULES_OUT = WORKER / "src/validation/configuration_rules.json"
CASES_OUT = HERE / "configuration_validation_cases.json"

# model_validator(mode="before") hooks the table cannot express as field rules.
HOOKS = {"github": "github_auth", "openapi": "openapi_auth"}

# Mirrors services/elitea-worker-python/src/elitea_worker/handlers/validation.py.
SAFE_CODES = {
    "literal_error": "VALUE_NOT_ALLOWED",
    "enum": "VALUE_NOT_ALLOWED",
    "missing": "REQUIRED_FIELD",
    "extra_forbidden": "UNKNOWN_FIELD",
    "value_error": "INVALID_CONFIGURATION",
    "greater_than": "VALUE_OUT_OF_RANGE",
    "greater_than_equal": "VALUE_OUT_OF_RANGE",
    "less_than": "VALUE_OUT_OF_RANGE",
    "less_than_equal": "VALUE_OUT_OF_RANGE",
}
MAX_ISSUES = 64

PROBES = [
    None, True, False, 0, 1, -1, 2, 1.5, 2.0, 1e30, 10**30, -0.0,
    "", " ", "x", "5", " 5 ", "5.0", "1e3", "1_000", "+7", "٣", "true",
    "false", "Yes", "no", "on", "OFF", "0", "1", "t", "y", "Auto", "Cloud",
    "Server", "custom", "Custom", "Basic", "Bearer", "default",
    [], ["a"], ["a", 1], [1], [None], {}, {"a": 1}, "é", "\U0001f600",
]
# Numeric-text edge cases only matter for integer and boolean fields.
NUMERIC_PROBES = [
    "1_0.00", "5.0_0", "-0", "+0", "00012", "1__0", "5\n", "\t5", "5.", ".5",
    "9223372036854775808", "-9223372036854775809", "1 ", "1 ",
    "TRUE", "True", "T", "OFF", "On", " true ", "true\n", "2", "-1", "0.0",
    9223372036854775807, -9223372036854775808, 9.2e18, 9.3e18, 1e18, 1e19,
    -9.2e18, -9.3e18, 0.5, -0.0, 1.0000000000000001,
]
# Probes that exercise a rule, not a field name: the full set runs once per
# distinct rule shape, a short set on every later field of that shape.
SHORT_PROBES = [None, 5, "", [], True, "x"]


def canonical(value: object) -> bytes:
    text = json.dumps(
        value, allow_nan=False, ensure_ascii=False,
        separators=(",", ":"), sort_keys=True,
    )
    return text.replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode()


def pointer(loc: tuple) -> str:
    if not loc:
        return ""
    return "/" + "/".join(str(p).replace("~", "~0").replace("/", "~1") for p in loc)


def expected(model, text: str) -> dict:
    from pydantic import ValidationError

    settings = json.loads(text)
    try:
        model.model_validate(settings)
        return {"valid": True, "issues": []}
    except ValidationError as exc:
        seen, mapped = set(), []
        for index, item in enumerate(
            exc.errors(include_url=False, include_context=False, include_input=False)
        ):
            code = SAFE_CODES.get(str(item.get("type")), "INVALID_VALUE")
            at = pointer(tuple(item.get("loc", ())))
            if (code, at) in seen:
                continue
            seen.add((code, at))
            mapped.append((at, code, index))
            if len(mapped) == MAX_ISSUES:
                break
        mapped.sort()
        return {"valid": False, "issues": [[code, at] for at, code, _ in mapped]}


def field_rule(name: str, prop: dict, required: bool) -> dict:
    nullable = False
    base = prop
    if "anyOf" in prop:
        options = [o for o in prop["anyOf"] if o != {"type": "null"}]
        nullable = len(options) != len(prop["anyOf"])
        if len(options) != 1:
            sys.exit(f"unsupported anyOf for {name}: {prop}")
        base = options[0]
    rule = {"name": name, "nullable": nullable, "required": required}
    if "enum" in base:
        assert base.get("type") == "string", (name, base)
        rule.update(kind="enum", values=list(base["enum"]))
    elif base.get("type") == "string":
        rule["kind"] = "string"
    elif base.get("type") == "integer":
        rule["kind"] = "integer"
    elif base.get("type") == "boolean":
        rule["kind"] = "boolean"
    elif base.get("type") == "array" and base.get("items") == {"type": "string"}:
        rule["kind"] = "string_list"
    else:
        sys.exit(f"unsupported schema for {name}: {prop}")
    return rule


def sample(rule: dict):
    kind = rule["kind"]
    return {
        "string": "value",
        "enum": rule.get("values", [""])[0],
        "integer": 5432,
        "boolean": True,
        "string_list": ["a"],
    }[kind]


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--sdk-root", required=True)
    args = parser.parse_args()
    sys.path.insert(0, args.sdk_root)
    from elitea_sdk import configurations as sdk

    registry = sdk.get_class_configurations()
    if sdk.FAILED_IMPORTS:
        sys.exit(f"failed SDK imports: {sorted(sdk.FAILED_IMPORTS)}")
    snapshot = json.loads(SNAPSHOT.read_text())
    pinned = {e["configuration_type"]: e for e in snapshot["entries"]}
    if set(registry) != set(pinned):
        sys.exit("SDK registry and Main catalog list different types")

    entries, types, cases = [], [], []
    probed: set = set()
    for kind, model in sorted(registry.items()):
        meta = model.model_config["json_schema_extra"]["metadata"]
        schema = model.model_json_schema()
        schema_digest = hashlib.sha256(canonical(schema)).hexdigest()
        if f"sha256:{schema_digest}" != pinned[kind]["schema_digest"]:
            sys.exit(f"schema digest drift for {kind}")
        entries.append({
            "connection_check_supported": callable(getattr(model, "check_connection", None)),
            "schema": schema,
            "section": meta["section"],
            "type": kind,
            "validation_supported": True,
        })
        required = set(schema.get("required") or [])
        fields = [
            field_rule(name, prop, name in required)
            for name, prop in schema["properties"].items()
        ]
        before = [
            d.cls_var_name
            for d in model.__pydantic_decorators__.model_validators.values()
            if d.info.mode == "before"
        ]
        extra_hooks = [
            d for d in model.__pydantic_decorators__.model_validators.values()
            if d.info.mode != "before"
        ] + list(model.__pydantic_decorators__.field_validators.values())
        if extra_hooks or bool(before) != (kind in HOOKS):
            sys.exit(f"unmodelled validators on {kind}")
        types.append({
            "type": kind,
            "section": meta["section"],
            "schema_id": pinned[kind]["schema_id"],
            "schema_digest": schema_digest,
            "hook": HOOKS.get(kind),
            "fields": fields,
        })

        base = {f["name"]: sample(f) for f in fields if f["required"]}
        docs = [base, {**base, "unknown_extra": 1}]
        for f in fields:
            docs.append({**base, f["name"]: sample(f)})
            if f["required"]:
                docs.append({k: v for k, v in base.items() if k != f["name"]})
            shape = (f["kind"], f["nullable"], f["required"], tuple(f.get("values", ())))
            probes = SHORT_PROBES
            if shape not in probed:
                probed.add(shape)
                probes = PROBES + (NUMERIC_PROBES if f["kind"] in ("integer", "boolean") else [])
            for probe in probes:
                docs.append({**base, f["name"]: probe})
        if kind == "github":
            for combo in [
                {"username": "u"}, {"password": "p"}, {"username": "u", "password": "p"},
                {"app_id": "1"}, {"app_private_key": "k"},
                {"app_id": "1", "app_private_key": "k"},
                {"username": " ", "password": "p"}, {"username": "u", "password": " "},
                {"username": 5, "password": "p"}, {"username": "u", "password": []},
                {"access_token": " "}, {"access_token": "t", "username": "u"},
                {"username": "u", "password": "p", "base_url": 5},
                {"username": "u", "password": "p", "app_id": "1"},
            ]:
                docs.append({"base_url": "https://api.github.com", **combo})
        if kind == "openapi":
            for combo in [
                {"oauth_discovery_endpoint": "https://d"},
                {"oauth_discovery_endpoint": "https://d", "client_id": "c"},
                {"oauth_discovery_endpoint": "https://d", "client_id": "c", "client_secret": "s"},
                {"client_id": "c"}, {"client_secret": "s"}, {"token_url": "t"},
                {"client_id": "c", "client_secret": "s"},
                {"client_id": "c", "client_secret": "s", "token_url": "t"},
                {"auth_type": "custom", "api_key": "k"},
                {"auth_type": " Custom ", "api_key": "k"},
                {"auth_type": "Custom", "api_key": "k", "custom_header_name": "X"},
                {"auth_type": "Custom", "api_key": "k", "custom_header_name": ""},
                {"auth_type": "Custom"}, {"auth_type": "bogus", "api_key": "k"},
                {"auth_type": 5, "api_key": "k"}, {"method": "default"},
                {"method": "bogus"}, {"oauth_discovery_endpoint": 1},
                {"oauth_discovery_endpoint": 0, "client_id": "c"},
                {"oauth_discovery_endpoint": "d", "client_id": 1, "client_secret": [0]},
            ]:
                docs.append(combo)
        seen_docs = set()
        for doc in docs:
            text = json.dumps(doc, sort_keys=True)
            if text in seen_docs:
                continue
            seen_docs.add(text)
            cases.append({"type": kind, "settings": text, **expected(model, text)})

    catalog_digest = hashlib.sha256(canonical({"entries": entries})).hexdigest()
    if f"sha256:{catalog_digest}" != snapshot["catalog_digest"]:
        sys.exit("catalog digest drift")

    RULES_OUT.parent.mkdir(parents=True, exist_ok=True)
    RULES_OUT.write_text(json.dumps({
        "schema_version": "elitea.worker-configuration-validation-rules.v1",
        "catalog_revision": snapshot["catalog_revision"],
        "catalog_digest": catalog_digest,
        "types": types,
    }, indent=1, sort_keys=True) + "\n")
    CASES_OUT.write_text(json.dumps(
        {"catalog_revision": snapshot["catalog_revision"], "cases": cases},
        separators=(",", ":"), sort_keys=True,
    ) + "\n")
    print(f"wrote {len(types)} types, {len(cases)} cases")


if __name__ == "__main__":
    main()
