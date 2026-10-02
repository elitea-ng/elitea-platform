#!/usr/bin/env python3
"""Validate the reviewed browser/Form current-baseline evidence fixture."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any


EXPECTED_SCHEMA_VERSION = 1
EXPECTED_CATALOG_SHA256 = "f4b66e012b9723b577d1c8a14da1a7283c88783dcbe6e1bc3efe2ca964cf52bc"
EXPECTED_SOURCE_COUNT = 77
EXPECTED_SOURCE_KEYSET_SHA256 = "481e22c121a04f376243d0260fbef97d8b35e1f1f29eeb72696f94f9bbf1e9dc"
EXPECTED_FINGERPRINT_COUNT = 116
EXPECTED_FINGERPRINT_KEYSET_SHA256 = "d22ca6f08039aef21cacd8eeb54ef400959c40bb425c51c24fbcdcbc3ca08b91"
EXPECTED_TOP_LEVEL_KEYS = {
    "behavior_contracts",
    "behavior_fingerprints",
    "deployment_contract",
    "effective_routes",
    "framework_contract",
    "http_outcomes",
    "inference_limits",
    "provenance",
    "route_composition",
    "schema_version",
    "scope",
    "security_dispositions",
    "source_files_sha256",
    "source_inventory",
    "ui_contract",
}
EXPECTED_CONTRACT_IDS = {
    "browser.auth_init.processor",
    "browser.cors_options_replacement",
    "browser.auth.get",
    "browser.info.get",
    "browser.login.get",
    "browser.logout.get",
    "browser.main_rpc_authorize",
    "browser.server_session",
    "browser.target_token",
    "form.authorize.post",
    "form.login.get",
    "form.logout.get",
    "form.resolved_snapshot_bridge",
}
EXPECTED_SECURITY_IDS = {
    "cors.options_replacement",
    "dependency.failure",
    "form.brute_force",
    "form.credentials",
    "form.csrf",
    "identity.provider_namespace",
    "info.disclosure",
    "input.duplicates",
    "proxy.trust",
    "public_rules.regex_engine",
    "rpc.boundary",
    "session.cookie",
    "session.fixation",
    "session.identity",
    "session.processor_failure",
    "session.serialization",
    "target.redirect",
    "credential.basic_decoding",
    "wire.unsupported_method",
}
EXPECTED_SECURITY_MIGRATIONS = {
    identifier: (
        "remove_internal_only"
        if identifier == "rpc.boundary"
        else "preserve_and_strengthen"
        if identifier in {"session.cookie", "session.fixation"}
        else "correct"
    )
    for identifier in EXPECTED_SECURITY_IDS
}
EXPECTED_PROVENANCE = {
    "admin_ui_repo": (
        "33334cb59cca6bc97dc0869ab335123ff12287c9",
        "pylon_main/plugins/admin_ui",
        set(),
    ),
    "auth_core_repo": (
        "fcc4c7a35fe095fb8d67e72451e3a4f9b497f871",
        "pylon_auth/plugins/auth_core",
        set(),
    ),
    "auth_form_repo": (
        "f7da603da7b373737c043873cb83cc96b589f061",
        "pylon_auth/plugins/auth_form",
        set(),
    ),
    "auth_idp_rpc_repo": (
        "68441d9fd94d0e45ad35955a38ea569a7b597f1e",
        "pylon_auth/plugins/auth_idp_rpc",
        set(),
    ),
    "auth_init_repo": (
        "f3e47ea0d3e64dc23d96e5475032e03dee256ab4",
        "pylon_auth/plugins/auth_init",
        set(),
    ),
    "auth_mappers_repo": (
        "5a6934d4f9a6953e926e47a05192d7268a5e5a96",
        "pylon_auth/plugins/auth_mappers",
        set(),
    ),
    "centry_repo": (
        "6b3e59f7f41e41c9d5f1dcf7ca6e870d7391986c",
        ".",
        {
            "docker-compose.yml",
            "envs/default.env",
            "pylon_auth/configs/auth_core.yml",
            "pylon_auth/configs/auth_form.yml",
            "pylon_auth/configs/auth_init.yml",
            "pylon_auth/pylon.yml",
            "pylon_main/configs/auth.yml",
            "pylon_main/configs/elitea_core.yml",
            "pylon_main/configs/shared.yml",
            "pylon_main/pylon.yml",
        },
    ),
    "main_auth_repo": (
        "ff02d66a8858604e6947bb3a52bda8543dbe0e76",
        "pylon_main/plugins/auth",
        set(),
    ),
    "runtime_interface_litellm_repo": (
        "cab9507cdcb12b1d04094c7f8443ed94c8c0834d",
        "pylon_main/plugins/runtime_interface_litellm",
        set(),
    ),
    "artifacts_repo": (
        "9ad2139bdbb1df70f3fa5b4cf78ae953b3fa35b1",
        "pylon_main/plugins/artifacts",
        set(),
    ),
    "elitea_core_repo": (
        "2b713350aa73af770164ac023cc88b4cb83667e1",
        "pylon_main/plugins/elitea_core",
        set(),
    ),
    "pylon_repo": ("cc12c3ec92f5b15d52ad62f557aed2012bca3aec", ".", set()),
    "ui_repo": ("53812f63c722512a225fe5fd27f895cd743555db", ".", set()),
}
EXPECTED_EFFECTIVE_ROUTES = {
    "/auth/auth": ["GET", "HEAD", "OPTIONS"],
    "/auth/auth_form/authorize": ["OPTIONS", "POST"],
    "/auth/auth_form/login": ["GET", "HEAD", "OPTIONS"],
    "/auth/auth_form/logout": ["GET", "HEAD", "OPTIONS"],
    "/auth/info": ["GET", "HEAD", "OPTIONS"],
    "/auth/login": ["GET", "HEAD", "OPTIONS"],
    "/auth/logout": ["GET", "HEAD", "OPTIONS"],
}
EXPECTED_ROUTE_RECORDS = {
    "auth_core/routes/auth.py#Route.auth": {
        "blueprint_prefix": "/",
        "declared_methods": ["GET"],
        "effective_route": "/auth/auth",
        "inner_flask_methods": ["GET", "HEAD", "OPTIONS"],
        "source_route": "/auth",
    },
    "auth_core/routes/auth.py#Route.info": {
        "blueprint_prefix": "/",
        "declared_methods": ["GET"],
        "effective_route": "/auth/info",
        "inner_flask_methods": ["GET", "HEAD", "OPTIONS"],
        "source_route": "/info",
    },
    "auth_core/routes/auth.py#Route.login": {
        "blueprint_prefix": "/",
        "declared_methods": ["GET"],
        "effective_route": "/auth/login",
        "inner_flask_methods": ["GET", "HEAD", "OPTIONS"],
        "source_route": "/login",
    },
    "auth_core/routes/auth.py#Route.logout": {
        "blueprint_prefix": "/",
        "declared_methods": ["GET"],
        "effective_route": "/auth/logout",
        "inner_flask_methods": ["GET", "HEAD", "OPTIONS"],
        "source_route": "/logout",
    },
    "auth_form/routes/form.py#Route.authorize": {
        "blueprint_prefix": "/auth_form",
        "declared_methods": ["POST"],
        "effective_route": "/auth/auth_form/authorize",
        "inner_flask_methods": ["OPTIONS", "POST"],
        "source_route": "/authorize",
    },
    "auth_form/routes/form.py#Route.login": {
        "blueprint_prefix": "/auth_form",
        "declared_methods": ["GET"],
        "effective_route": "/auth/auth_form/login",
        "inner_flask_methods": ["GET", "HEAD", "OPTIONS"],
        "source_route": "/login",
    },
    "auth_form/routes/form.py#Route.logout": {
        "blueprint_prefix": "/auth_form",
        "declared_methods": ["GET"],
        "effective_route": "/auth/auth_form/logout",
        "inner_flask_methods": ["GET", "HEAD", "OPTIONS"],
        "source_route": "/logout",
    },
}
EXPECTED_HTTP_OUTCOME_IDS = {
    "browser.info.empty_target",
    "application.common_headers",
    "browser.info.denied",
    "browser.info.json",
    "browser.info.raw",
    "browser.login",
    "browser.logout",
    "exposure.unsupported_inner_method",
    "exposure.outer_unsupported_method",
    "exposure.registry_miss",
    "exposure.timeout",
    "form.authorize.invalid",
    "form.authorize.processor_failure",
    "form.authorize.success",
    "form.login",
    "form.logout",
    "browser_auth.invalid_credential",
    "browser_auth.empty_target",
    "browser_auth.missing_forwarded_header",
    "browser_auth.no_authentication",
    "browser_auth.noop_success",
    "browser_auth.rpc_success",
    "browser_auth.unknown_target",
    "inner.automatic_options",
    "inner.head",
    "inner.not_found",
    "main.cors_options_replacement",
}
EXPECTED_LOGOUT_CONSUMERS = [
    "Admin UI sidebar assigns same-origin /auth/logout",
    "settings action assigns same-origin /auth/logout",
    "sidebar user action assigns same-origin /auth/logout",
]
EXPECTED_SELECTED_CONFIG_SOURCES = [
    "centry/docker-compose.yml#auth_runtime_allowlist",
    "centry/envs/default.env#auth_nonsecret_allowlist",
    "centry/pylon_auth/configs/auth_core.yml#browser_auth_allowlist",
    "centry/pylon_auth/configs/auth_form.yml#redacted_form_shape",
    "centry/pylon_auth/configs/auth_init.yml#initial_admin_allowlist",
    "centry/pylon_auth/pylon.yml#auth_http_and_session_allowlist",
    "centry/pylon_main/configs/auth.yml#auth_gate_allowlist",
    "centry/pylon_main/configs/elitea_core.yml#public_route_override_allowlist",
    "centry/pylon_main/configs/shared.yml#cors_allowlist",
    "centry/pylon_main/pylon.yml#browser_auth_exposure_allowlist",
    "elitea_core/config.yml#public_route_allowlist",
    "runtime_interface_litellm/config.yml#public_route_allowlist",
]
EXPECTED_OPTIONAL_RUNTIME_CONFIGS = [
    {
        "contents_exported": False,
        "path": "centry/pylon_auth/configs/auth_mappers.yml",
        "present": False,
    }
]
EXPECTED_TRACKED_AUTH_MAPPER_CONTRACT = {
    "header": {
        "outputs": {"X-WEBAUTH-USER": "'provider_attr'.'nameid'"},
        "requirements_present": False,
        "scope": "grafana",
    },
    "json": {
        "endpoint": None,
        "outputs": {"login": "'provider_attr'.'nameid'"},
        "scope": "galloper",
    },
    "jsonpath_dependency": "jsonpath_rw==1.4.0",
}

EXPECTED_CONFIGURED_PUBLIC_RULES = [
    {
        "id": "config.browser_auth",
        "ordinal": 0,
        "rule": {"uri": "/forward\\-auth/.*"},
        "source": "centry/pylon_main/configs/auth.yml#public_rules[0]",
    },
    {
        "id": "config.application_icon",
        "ordinal": 1,
        "rule": {"uri": "/applications/application_icon.*"},
        "source": "centry/pylon_main/configs/auth.yml#public_rules[1]",
    },
    {
        "id": "config.datasource_icon",
        "ordinal": 2,
        "rule": {"uri": "/datasources/datasource_icon.*"},
        "source": "centry/pylon_main/configs/auth.yml#public_rules[2]",
    },
    {
        "id": "config.prompt_icon",
        "ordinal": 3,
        "rule": {"uri": "/prompt_lib/prompt_icon.*"},
        "source": "centry/pylon_main/configs/auth.yml#public_rules[3]",
    },
]

EXPECTED_DYNAMIC_PUBLIC_RULES = [
    {
        "add_source": "runtime_interface_litellm/methods/init.py#Method.init",
        "condition": "plugin initialized with tracked url_prefix=/llm",
        "effective_in_tracked_base_config": True,
        "id": "runtime_interface_litellm.proxy",
        "plugin_local_order": 1,
        "remove_source": "runtime_interface_litellm/methods/init.py#Method.deinit",
        "rule": {"uri": "/llm/.*"},
    },
    {
        "add_source": "admin_ui/module.py#Module.init",
        "condition": "plugin initialized",
        "effective_in_tracked_base_config": True,
        "id": "admin_ui.static_assets",
        "plugin_local_order": 1,
        "remove_source": None,
        "rule": {
            "uri": "/admin/app/.*\\.(js|css|ico|png|jpg|jpeg|gif|svg|woff|woff2|ttf|eot|map)$"
        },
    },
    {
        "add_source": "artifacts/methods/s3.py#Method.s3_api_init",
        "condition": "S3 API init hook runs",
        "effective_in_tracked_base_config": True,
        "id": "artifacts.s3_sigv4",
        "plugin_local_order": 1,
        "remove_source": "artifacts/methods/s3.py#Method.s3_api_deinit",
        "rule": {"uri": "/artifacts/s3/.*"},
    },
    {
        "add_source": "elitea_core/module.py#Module.elitea_ui_init",
        "condition": "elitea_core UI init runs",
        "effective_in_tracked_base_config": True,
        "id": "elitea_core.socket_io",
        "plugin_local_order": 1,
        "remove_source": None,
        "rule": {"uri": "/socket\\.io/.*"},
    },
    {
        "add_source": "elitea_core/module.py#Module.elitea_ui_init",
        "condition": "elitea_core UI init runs",
        "effective_in_tracked_base_config": True,
        "id": "elitea_core.robots",
        "plugin_local_order": 2,
        "remove_source": None,
        "rule": {"uri": "/robots\\.txt"},
    },
    {
        "add_source": "elitea_core/module.py#Module.elitea_ui_init",
        "condition": "elitea_core UI init runs",
        "effective_in_tracked_base_config": True,
        "id": "elitea_core.favicon",
        "plugin_local_order": 3,
        "remove_source": None,
        "rule": {"uri": "/favicon\\.ico"},
    },
    {
        "add_source": "elitea_core/module.py#Module.elitea_ui_init",
        "condition": "elitea_core UI init runs",
        "effective_in_tracked_base_config": True,
        "id": "elitea_core.access_denied",
        "plugin_local_order": 4,
        "remove_source": None,
        "rule": {"uri": "/app/access_denied"},
    },
    {
        "add_source": "elitea_core/module.py#Module.mcp_sse_init",
        "condition": "tracked base config public_messages_route=true",
        "effective_in_tracked_base_config": True,
        "id": "elitea_core.public_messages",
        "plugin_local_order": 5,
        "remove_source": "elitea_core/module.py#Module.mcp_sse_deinit",
        "rule": {"uri": "/elitea_core/[0-9]+/messages\\?session_id=.+"},
    },
    {
        "add_source": "elitea_core/module.py#Module.init",
        "condition": "elitea_core main init reaches webhook registration",
        "effective_in_tracked_base_config": True,
        "id": "elitea_core.webhook",
        "plugin_local_order": 6,
        "remove_source": None,
        "rule": {
            "uri": "/api/v2/elitea_core/webhook/prompt_lib/[0-9]+/[0-9]+/(github|gitlab|custom)"
        },
    },
]
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


def _canonical_sha256(value: Any) -> str:
    data = json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    return hashlib.sha256(data).hexdigest()


def _keyset_sha256(value: dict[str, Any]) -> str:
    return _canonical_sha256(sorted(value))


def _check_exact_keys(
    value: Any,
    expected: set[str],
    name: str,
    failures: list[str],
) -> dict[str, Any]:
    if not isinstance(value, dict):
        failures.append(f"{name} must be an object")
        return {}
    if set(value) != expected:
        failures.append(f"{name} structure changed")
    return value


def _records_by_id(value: Any, name: str, failures: list[str]) -> dict[str, dict[str, Any]]:
    if not isinstance(value, list) or any(not isinstance(item, dict) for item in value):
        failures.append(f"{name} must be an object list")
        return {}
    records: dict[str, dict[str, Any]] = {}
    for item in value:
        identifier = item.get("id")
        if not isinstance(identifier, str) or not identifier or identifier in records:
            failures.append(f"{name} IDs must be unique non-empty strings")
            continue
        records[identifier] = item
    return records


def check_catalog(catalog: dict[str, Any]) -> list[str]:
    failures: list[str] = []
    if set(catalog) != EXPECTED_TOP_LEVEL_KEYS:
        failures.append("browser auth evidence top-level structure changed")
    if catalog.get("schema_version") != EXPECTED_SCHEMA_VERSION:
        failures.append("browser auth evidence schema version changed")

    scope = _check_exact_keys(
        catalog.get("scope"),
        {
            "current_root_auth_alias",
            "full_auth_parity_claim",
            "includes",
            "migration_baseline_name",
        },
        "scope",
        failures,
    )
    if scope.get("full_auth_parity_claim") is not False:
        failures.append("fixture must not claim full Auth parity")
    if scope.get("current_root_auth_alias") is not False:
        failures.append("fixture must not invent a current root /auth alias")
    if scope.get("migration_baseline_name") != "current implementation":
        failures.append("current-baseline naming changed")
    if scope.get("includes") != [
        "Auth Core browser routes and redirect/session orchestration",
        "Form login/logout provider",
        "pylon_main browser authorization RPC divergence",
        "Auth Init login processor business rules",
        "Auth mapper info behavior",
        "Pylon server-side session/cookie mechanics",
        "tracked Centry browser-auth composition",
        "tracked Main-local configured and dynamic public-rule registrations",
        "EliteaUI browser-auth consumers",
        "Admin UI logout consumer",
    ]:
        failures.append("reviewed browser-auth scope inventory changed")
    if catalog.get("inference_limits") != [
        "This is not a complete pylon_auth, pylon_main Auth, administrative API, OIDC, SAML, IdP RPC, or RBAC parity claim.",
        "Tracked configuration can be overridden by the runtime config provider; no running environment or secret source is inspected.",
        "Pylon and browser behavior is derived from pinned source and configuration, not from a live Flask/Traefik execution.",
        "The Centry image tag is bound to the same local Pylon Git tag, but the deployed container image digest was not pulled or live-verified.",
        "The pylon.yml files are represented only by the named auth/session/exposure selectors; unrelated server settings are outside this contract.",
        "The reviewed direct Auth Core public-rule set is empty; tracked Main-local configured and dynamic registrations are inventoried separately, while runtime config-provider payloads or unreviewed plugins can still change the effective Main set.",
        "Migration dispositions preserve valid-client behavior but intentionally correct malformed or ambiguous inputs; they are not a byte-for-byte HTTP parity claim and require cutover validation.",
        "UI source is pinned only for browser-login redirects, logout, popup callback, and request retry behavior.",
    ]:
        failures.append("browser-auth evidence inference limits changed")

    contracts = _records_by_id(catalog.get("behavior_contracts"), "behavior_contracts", failures)
    if set(contracts) != EXPECTED_CONTRACT_IDS:
        failures.append("reviewed browser behavior contract set changed")
    form_authorize = contracts.get("form.authorize.post", {})
    if form_authorize.get("failure", {}).get("target_preserved") is not False:
        failures.append("Form failed-login target-loss behavior changed")
    if form_authorize.get("success", {}).get("authentication_expiration") != (
        "naive datetime.now() plus exactly 86400 seconds"
    ):
        failures.append("Form 24-hour authentication contract changed")
    form_shape = form_authorize.get("configuration_shape", {})
    if form_shape.get("runtime_match_fields") != ["login", "password"] or form_shape.get(
        "top_level_email_consumed_by_route"
    ) is not False:
        failures.append("Form runtime/admin-schema mismatch changed")
    browser_auth = contracts.get("browser.auth.get", {})
    target_semantics = browser_auth.get("target_query_semantics", {})
    if "returns None" not in str(target_semantics.get("absent", "")) or (
        "empty string" not in str(target_semantics.get("explicit_empty", ""))
        or "access is denied" not in str(target_semantics.get("explicit_empty", ""))
    ):
        failures.append("absent versus empty ForwardAuth target semantics changed")
    info = contracts.get("browser.info.get", {})
    if info.get("local_auth_check") is not False or "raw six-field" not in str(
        info.get("outcome", {}).get("target_absent", "")
    ):
        failures.append("unprotected raw info behavior changed")
    if "302 redirect" not in str(info.get("outcome", {}).get("target_unknown", "")):
        failures.append("unknown info mapper redirect outcome changed")
    if "distinct from absent None" not in str(
        info.get("outcome", {}).get("target_empty", "")
    ):
        failures.append("absent versus empty info target semantics changed")
    cors = contracts.get("browser.cors_options_replacement", {})
    main_cors = cors.get("main_effective_behavior", {})
    if main_cors.get("enabled_by") != "tracked pylon_main settings.allow_cors=true" or (
        main_cors.get("lost_response_state") != ["body", "status", "previous headers"]
    ):
        failures.append("effective Main OPTIONS replacement contract changed")
    if "Server: Centry" not in str(cors.get("auth_core_code_capability", "")):
        failures.append("Auth Core CORS Server-header loss is no longer explicit")
    if "Allow-Origin '*'" not in str(cors.get("security_defect", "")) or (
        "Allow-Credentials true" not in str(cors.get("security_defect", ""))
    ):
        failures.append("wildcard credentialed CORS defect is no longer explicit")
    rpc = contracts.get("browser.main_rpc_authorize", {})
    if "allow_auth_traversal defaults true" not in str(rpc.get("credential_failure_divergence", "")):
        failures.append("main RPC credential traversal divergence changed")
    if "matching public rule succeeds" not in str(rpc.get("credential_failure_divergence", "")):
        failures.append("main RPC public-after-invalid branch changed")
    if "synthetic public principal" not in str(rpc.get("transport_failure", "")):
        failures.append("main RPC fail-open transport defect is no longer explicit")
    if rpc.get("evaluation_sequence") != [
        "Authorization credential handler",
        "configured additional credential headers",
        "public-rule classification",
        "referenced browser session",
        "public success when no valid session",
        "login redirect or invalid-credential denial",
    ]:
        failures.append("main RPC evaluation sequence changed")
    cache_contract = rpc.get("cache_contract", {})
    if cache_contract.get("ttl_seconds") != 60 or cache_contract.get(
        "revocation_delay_seconds"
    ) != 60 or "auth_ok=true only" not in str(cache_contract.get("cached_results", "")):
        failures.append("main RPC authorization-cache contract changed")
    if cache_contract.get("key_omissions") != [
        "HTTP method",
        "request URI",
        "target",
        "scope",
        "public-rule or authorization-policy revision",
    ] or "do not port this cache" not in str(cache_contract.get("migration", "")):
        failures.append("unsafe Main authorization-cache key disposition changed")
    if rpc.get("local_public_override") != {
        "classification_time": "before cache lookup and auth_authorize RPC",
        "rpc_short_circuited": False,
        "successful_authentication_wins": True,
        "negative_authorization_result": (
            "a matching local public rule replaces the negative result with a "
            "synthetic public principal"
        ),
        "transport_exception": (
            "becomes a synthetic public principal regardless of whether a local "
            "public rule matched"
        ),
    }:
        failures.append("Main local-public override ordering changed")
    if "direct typed in-process call" not in str(rpc.get("migration_transport", "")) or (
        "internal Redis RPC" not in str(rpc.get("migration_transport", ""))
    ):
        failures.append("auth_authorize in-process merge disposition changed")
    init_processor = contracts.get("browser.auth_init.processor", {})
    init_failures = " ".join(init_processor.get("failure_branches", []))
    startup_failures = " ".join(init_processor.get("startup_failure_branches", []))
    for required in (
        "duplicate create",
        "auth_ctx.user_id remains null",
        "AUTOCOMMIT",
        "done=true",
    ):
        if required not in init_failures:
            failures.append(f"Auth Init failure branch missing: {required}")
    if "partially initialized durable state" not in startup_failures:
        failures.append("Auth Init partial bootstrap branch changed")
    session = contracts.get("browser.server_session", {})
    if session.get("lifetime", {}).get("tracked_seconds") != 604800 or session.get(
        "storage", {}
    ).get("record_encoding") != "Python Pickle despite the initially selected msgpack format":
        failures.append("server-side session baseline changed")

    dispositions = _records_by_id(
        catalog.get("security_dispositions"), "security_dispositions", failures
    )
    if set(dispositions) != EXPECTED_SECURITY_IDS:
        failures.append("reviewed security disposition set changed")
    for identifier, item in dispositions.items():
        if item.get("migration") not in {"correct", "preserve_and_strengthen", "remove_internal_only"}:
            failures.append(f"invalid migration disposition for {identifier}")
        if not isinstance(item.get("baseline"), str) or not isinstance(item.get("requirement"), str):
            failures.append(f"incomplete migration disposition for {identifier}")
    observed_migrations = {
        identifier: item.get("migration") for identifier, item in dispositions.items()
    }
    if observed_migrations != EXPECTED_SECURITY_MIGRATIONS:
        failures.append("reviewed security migration disposition map changed")
    if "done=true" not in str(dispositions.get("session.processor_failure", {}).get("baseline", "")):
        failures.append("processor partial-authentication defect is no longer explicit")
    if dispositions.get("rpc.boundary", {}).get("migration") != "remove_internal_only":
        failures.append("internal RPC removal disposition changed")
    if "/auth/auth" not in str(
        dispositions.get("rpc.boundary", {}).get("requirement", "")
    ):
        failures.append("ForwardAuth ingress compatibility path changed")
    if "no internal auth_authorize RPC" not in str(
        dispositions.get("rpc.boundary", {}).get("requirement", "")
    ):
        failures.append("auth_authorize RPC removal is no longer explicit")
    cors_disposition = dispositions.get("cors.options_replacement", {})
    if "Server: Centry" not in str(cors_disposition.get("baseline", "")) or (
        "never combine wildcard origin with credentials"
        not in str(cors_disposition.get("requirement", ""))
    ):
        failures.append("CORS OPTIONS security correction changed")
    if "synthetic public principal" not in str(
        dispositions.get("dependency.failure", {}).get("baseline", "")
    ):
        failures.append("dependency fail-open defect is no longer explicit")
    unsupported_wire = dispositions.get("wire.unsupported_method", {})
    if "Location header" not in str(unsupported_wire.get("baseline", "")) or (
        "not byte-for-byte parity" not in str(unsupported_wire.get("requirement", ""))
        or "valid-client behavior" not in str(unsupported_wire.get("requirement", ""))
    ):
        failures.append("unsupported-method wire correction disposition changed")
    if "permissive default" not in str(
        dispositions.get("credential.basic_decoding", {}).get("baseline", "")
    ) or "valid Basic clients" not in str(
        dispositions.get("credential.basic_decoding", {}).get("requirement", "")
    ):
        failures.append("strict Basic-decoding disposition changed")
    if "duplicate security-sensitive" not in str(
        dispositions.get("input.duplicates", {}).get("requirement", "")
    ):
        failures.append("duplicate input disposition changed")
    proxy_requirement = str(dispositions.get("proxy.trust", {}).get("requirement", ""))
    if not all(value in proxy_requirement for value in ("client IP", "proto", "host")):
        failures.append("trusted-proxy normalization disposition changed")
    regex_requirement = str(
        dispositions.get("public_rules.regex_engine", {}).get("requirement", "")
    )
    if "RE2" not in regex_requirement or "tracked valid rules" not in regex_requirement:
        failures.append("public-rule regex-engine disposition changed")

    routes = catalog.get("effective_routes")
    if not isinstance(routes, dict):
        failures.append("effective_routes must be an object")
        routes = {}
    observed_routes: dict[str, Any] = {}
    for record in routes.values():
        record = _check_exact_keys(
            record,
            {
                "blueprint_prefix",
                "declared_methods",
                "effective_route",
                "inner_flask_methods",
                "source_route",
            },
            "effective route record",
            failures,
        )
        if not isinstance(record.get("effective_route"), str):
            failures.append("effective route record is malformed")
            continue
        observed_routes[record["effective_route"]] = record.get("inner_flask_methods")
    if observed_routes != EXPECTED_EFFECTIVE_ROUTES:
        failures.append("effective browser/Form route matrix changed")
    if routes != EXPECTED_ROUTE_RECORDS:
        failures.append("source-derived browser/Form route records changed")

    route_composition = catalog.get("route_composition")
    expected_route_composition = {
        "auth_core_blueprint_prefix": "/",
        "auth_form_blueprint_prefix": "/auth_form",
        "auth_form_descriptor_name": "auth_form",
        "auth_pylon_context_url_prefix": "/auth",
        "auth_pylon_server_path": "/auth/",
        "composition_chain": [
            "pylon_main Auth before_request attempts auth_authorize; the configured /auth public rule keeps negative decisions non-blocking",
            "pylon_main exposure accepts the configured /auth prefix",
            "exposure forwards the original WSGI path to the pylon_auth process",
            "pylon_auth strips its context URL prefix before app-router dispatch",
            "ModuleDescriptor registers Auth Core at its configured blueprint prefix",
            "Auth Form derives /auth_form from Auth Core get_relative_url_prefix",
            "Flask combines the blueprint prefix with the literal route decorator",
        ],
        "external_base_path": "/auth",
        "external_listener_methods": [
            "DELETE",
            "GET",
            "HEAD",
            "OPTIONS",
            "PATCH",
            "POST",
            "PUT",
        ],
        "main_exposure_prefixes": ["/auth"],
        "pylon_runtime_ref": "1.2.25",
        "pylon_web_runtime": "gevent",
        "root_auth_alias": False,
    }
    if route_composition != expected_route_composition:
        failures.append("derived Pylon/Flask route composition changed")

    framework = catalog.get("framework_contract")
    expected_framework = {
        "pinned_versions": {
            "Flask": "3.1.3",
            "Flask-Session": "0.8.0",
            "PyJWT": "2.7.0",
            "Werkzeug": "3.1.6",
            "gevent": "25.9.1",
        },
        "pylon_runtime_commit": "cc12c3ec92f5b15d52ad62f557aed2012bca3aec",
        "pylon_runtime_ref": "1.2.25",
        "route_method_semantics": (
            "Flask supplies HEAD for GET routes and automatic OPTIONS; the outer Pylon "
            "exposure listener accepts seven methods before inner Flask dispatch"
        ),
        "route_normalization_dependency": (
            "Pylon's exposure subpath rule contains a double slash and relies on "
            "Werkzeug Map merge_slashes=True to serve the single-slash external path"
        ),
        "unsupported_method_semantics": (
            "inner Werkzeug MethodNotAllowed is handled by Auth Core's generic exception "
            "handler, which returns the configured access-denied redirect response with "
            "its status overridden to 400"
        ),
        "werkzeug_merge_slashes": True,
    }
    if framework != expected_framework:
        failures.append("pinned browser-auth framework contract changed")

    outcomes = _records_by_id(catalog.get("http_outcomes"), "http_outcomes", failures)
    if set(outcomes) != EXPECTED_HTTP_OUTCOME_IDS:
        failures.append("reviewed HTTP outcome set changed")
    denied_location = "${APP_PROTO}://${APP_HOST}/access_denied"
    if outcomes.get("browser.info.denied", {}).get("response") != {
        "location": denied_location,
        "status": 302,
    }:
        failures.append("unknown/malformed info mapper outcome changed")
    if outcomes.get("browser.info.empty_target", {}).get("response") != {
        "location": denied_location,
        "status": 302,
    }:
        failures.append("explicit empty info target outcome changed")
    if outcomes.get("browser_auth.empty_target", {}).get("response") != {
        "location": denied_location,
        "status": 302,
    } or "reaches the success mapper" not in str(
        outcomes.get("browser_auth.empty_target", {}).get("request", {}).get(
            "precondition", ""
        )
    ):
        failures.append("explicit empty ForwardAuth target outcome changed")
    if outcomes.get("exposure.unsupported_inner_method", {}).get("response") != {
        "body": "Flask redirect HTML",
        "location": denied_location,
        "status": 400,
    }:
        failures.append("inner unsupported-method outcome changed")
    if outcomes.get("exposure.outer_unsupported_method", {}).get("response") != {
        "status": 405
    }:
        failures.append("outer unsupported-method outcome changed")
    if outcomes.get("inner.automatic_options", {}).get("response") != {
        "allow": "the route's inner_flask_methods set",
        "status": 200,
    }:
        failures.append("automatic OPTIONS outcome changed")
    main_cors_outcome = outcomes.get("main.cors_options_replacement", {})
    if main_cors_outcome.get("response", {}).get("headers") != {
        "Access-Control-Allow-Credentials": "true",
        "Access-Control-Allow-Headers": "*",
        "Access-Control-Allow-Methods": "*",
        "Access-Control-Allow-Origin": "*",
    } or "the original status, body, and headers are discarded" not in main_cors_outcome.get(
        "side_effects", []
    ):
        failures.append("Main CORS replacement HTTP outcome changed")
    if outcomes.get("inner.not_found", {}).get("response", {}).get("status") != 404:
        failures.append("inner NotFound outcome changed")
    if outcomes.get("exposure.registry_miss", {}).get("response") != {"status": 404}:
        failures.append("exposure registry-miss outcome changed")
    if outcomes.get("exposure.timeout", {}).get("response") != {"status": 504}:
        failures.append("exposure timeout outcome changed")
    invalid_credential = outcomes.get("browser_auth.invalid_credential", {})
    if invalid_credential.get("browser_session_traversed") is not False or invalid_credential.get(
        "tracked_effective_response"
    ) != {"location": denied_location, "reason": "reviewed Auth Core public-rule set is empty", "status": 302}:
        failures.append("direct ForwardAuth invalid-credential outcome changed")
    if "done=true" not in str(
        outcomes.get("form.authorize.processor_failure", {}).get("state", "")
    ) or "user_id to '-'" not in str(
        outcomes.get("form.authorize.processor_failure", {}).get("state", "")
    ):
        failures.append("Form processor-failure observable session outcome changed")
    if outcomes.get("application.common_headers", {}).get("headers") != {"Server": "Centry"}:
        failures.append("pylon_auth application response headers changed")

    deployment = _check_exact_keys(
        catalog.get("deployment_contract"),
        {
            "auth_core",
            "auth_form",
            "auth_init",
            "public_rule_ownership",
            "pylon_auth_selected",
            "pylon_main",
            "runtime_composition",
            "runtime_override_limit",
            "tracked_environment_defaults",
        },
        "deployment_contract",
        failures,
    )
    if deployment.get("auth_core") != {
        "additional_headers": {"Server": "Centry"},
        "allow_auth_traversal": True,
        "auth_denied_url": denied_location,
        "db_isolation_level": "AUTOCOMMIT",
        "default_login_url": "${APP_PROTO}://${APP_HOST}/",
        "default_logout_url": "${APP_PROTO}://${APP_HOST}/",
        "other_auth_headers": {},
        "provider": "form",
        "register_error_handler": True,
        "url_prefix": "/",
    }:
        failures.append("effective tracked Auth Core configuration changed")
    form_config = deployment.get("auth_form", {})
    if form_config != {
        "configured_user_count": 1,
        "configured_user_keys": [["login", "password"]],
        "credential_sources": ["environment_expansion"],
        "credential_values_exported": False,
    }:
        failures.append("redacted tracked Form configuration changed")
    if deployment.get("auth_init") != {
        "initial_global_admin_provider_references": ["admin"],
        "initial_root_permissions": [],
    }:
        failures.append("effective tracked Auth Init configuration changed")
    defaults = deployment.get("tracked_environment_defaults", {})
    if defaults != {
        "APP_PROTO": "http",
        "COOKIES_LIFETIME": "604800",
        "COOKIES_SECURE": "false",
        "NAME_PREFIX": "centry",
    }:
        failures.append("tracked non-secret auth environment defaults changed")
    if deployment.get("auth_core", {}).get("provider") != "form":
        failures.append("tracked default auth provider changed")
    if deployment.get("pylon_main") != {
        "allow_cors": True,
        "auth_mode": "rpc",
        "browser_auth_exposure": {
            "exposure.event_node.type": "RedisEventNode",
            "exposure.handle.enabled": "true",
            "exposure.handle.prefixes": ["/auth"],
        },
        "public_uri_rules": [
            "/forward\\-auth/.*",
            "/applications/application_icon.*",
            "/datasources/datasource_icon.*",
            "/prompt_lib/prompt_icon.*",
        ],
    }:
        failures.append("tracked pylon_main auth mode changed")
    expected_public_rule_ownership = {
        "auth_core_direct": {
            "initial_rules": [],
            "reviewed_dynamic_registration_sites": [],
        },
        "main_local": {
            "configured_registration_order": EXPECTED_CONFIGURED_PUBLIC_RULES,
            "dynamic_registration_sites": EXPECTED_DYNAMIC_PUBLIC_RULES,
            "main_rpc_mode_forwards_rules_to_auth_core": False,
            "ordering": {
                "configured": "Auth Module.init appends the YAML rules in the listed order",
                "dynamic": (
                    "each site appends during its plugin lifecycle; cross-plugin scheduler "
                    "order is not inferred, while plugin_local_order pins order within a plugin"
                ),
                "evaluation": (
                    "Main evaluates every rule in its local insertion-ordered list into one "
                    "public boolean without short-circuiting; each mapping requires every "
                    "regex value to fullmatch its corresponding source field"
                ),
            },
        },
    }
    if deployment.get("public_rule_ownership") != expected_public_rule_ownership:
        failures.append("tracked Main/Auth Core public-rule ownership changed")
    if deployment.get("pylon_auth_selected") != {
        "application.APPLICATION_ROOT": "/auth/",
        "application.PERMANENT_SESSION_LIFETIME": "${COOKIES_LIFETIME}",
        "application.PREFERRED_URL_SCHEME": "${APP_PROTO}",
        "application.SECRET_KEY_configured": True,
        "application.SERVER_NAME": "${APP_HOST}",
        "application.SESSION_COOKIE_DOMAIN": "${APP_HOST}",
        "application.SESSION_COOKIE_HTTPONLY": "true",
        "application.SESSION_COOKIE_NAME": "${NAME_PREFIX}_auth_session",
        "application.SESSION_COOKIE_PATH": "/",
        "application.SESSION_COOKIE_SAMESITE": None,
        "application.SESSION_COOKIE_SECURE": "${COOKIES_SECURE}",
        "exposure.event_node.type": "RedisEventNode",
        "exposure.expose": "true",
        "rpc.redis_configured": True,
        "server.path": "/auth/",
        "server.proxy.x_for": "1",
        "server.proxy.x_host": "1",
        "server.proxy.x_proto": "1",
        "sessions.prefix": "${NAME_PREFIX}_auth_session_",
        "sessions.redis_configured": True,
    }:
        failures.append("selected pylon_auth session/RPC/exposure configuration changed")
    if deployment.get("runtime_composition") != {
        "pylon_auth.image": "ghcr.io/eliteaai/pylon:1.2.25",
        "pylon_auth.web_runtime": "gevent",
        "pylon_main.image": "ghcr.io/eliteaai/pylon:1.2.25",
        "pylon_main.web_runtime": "gevent",
        "pylon_source_ref": "1.2.25",
    }:
        failures.append("tracked Pylon image/runtime composition changed")
    if deployment.get("runtime_override_limit") != (
        "config-provider payloads and local override.env are intentionally excluded; "
        "production parity requires a redacted effective-config export"
    ):
        failures.append("runtime effective-configuration limitation changed")

    ui_contract = _check_exact_keys(
        catalog.get("ui_contract"),
        {
            "api_auth_surface",
            "change_budget",
            "logout_consumer_sources",
            "logout_consumers",
            "reauthentication",
            "security_boundary",
        },
        "ui_contract",
        failures,
    )
    if ui_contract.get("logout_consumers") != EXPECTED_LOGOUT_CONSUMERS:
        failures.append("direct browser logout consumer descriptions changed")
    if ui_contract.get("logout_consumer_sources") != [
        "admin_ui/frontend/src/components/Layout/Sidebar.jsx",
        "ui/src/[fsd]/pages/settings/index.jsx",
        "ui/src/[fsd]/widgets/sidebar-root/ui/button/UserButton.jsx",
    ]:
        failures.append("direct browser logout consumer source set changed")
    if ui_contract.get("api_auth_surface") != {
        "api_slice_path": "/auth",
        "documented_api_base": "/api/v2/",
        "effective_prefix": "/api/v2/auth",
        "relationship_to_browser_auth": (
            "EliteaUI API-relative /auth requests are not a root browser /auth alias"
        ),
    }:
        failures.append("EliteaUI API-relative Auth surface changed")
    if ui_contract.get("reauthentication", {}).get("callback_route") != "/auth-callback":
        failures.append("EliteaUI reauthentication callback route changed")
    expected_ui_contract = {
        "api_auth_surface": {
            "api_slice_path": "/auth",
            "documented_api_base": "/api/v2/",
            "effective_prefix": "/api/v2/auth",
            "relationship_to_browser_auth": (
                "EliteaUI API-relative /auth requests are not a root browser /auth alias"
            ),
        },
        "change_budget": (
            "preserving redirect paths and the callback query allows the first backend "
            "cutover with no intentional UI route-shape change"
        ),
        "logout_consumer_sources": [
            "admin_ui/frontend/src/components/Layout/Sidebar.jsx",
            "ui/src/[fsd]/pages/settings/index.jsx",
            "ui/src/[fsd]/widgets/sidebar-root/ui/button/UserButton.jsx",
        ],
        "logout_consumers": EXPECTED_LOGOUT_CONSUMERS,
        "reauthentication": {
            "callback_query": (
                "auth_state must survive the authentication return target unchanged"
            ),
            "callback_route": "/auth-callback",
            "detection": (
                "a fetch redirect URL containing both /auth/ and /login"
            ),
            "retry": "retry the cloned original request after popup success",
            "success_channels": [
                "same-origin postMessage",
                "BroadcastChannel",
                "localStorage",
            ],
        },
        "security_boundary": (
            "UI auth_state is popup correlation, not provider assertion validation or "
            "server-side Form/OIDC/SAML CSRF protection"
        ),
    }
    if ui_contract != expected_ui_contract:
        failures.append("reviewed UI browser-auth contract changed")

    source_hashes = catalog.get("source_files_sha256")
    if not isinstance(source_hashes, dict) or not source_hashes:
        failures.append("source_files_sha256 must be a non-empty object")
    else:
        if any(
            not isinstance(path, str) or not SHA256_RE.fullmatch(str(digest))
            for path, digest in source_hashes.items()
        ):
            failures.append("reviewed source hashes are malformed")
        if (
            len(source_hashes) != EXPECTED_SOURCE_COUNT
            or _keyset_sha256(source_hashes) != EXPECTED_SOURCE_KEYSET_SHA256
        ):
            failures.append("reviewed source file set changed")
        if any(path.startswith("centry/") for path in source_hashes):
            failures.append("secret-bearing Centry files must not be full-byte hashed")

    source_inventory = _check_exact_keys(
        catalog.get("source_inventory"),
        {
            "full_byte_sources",
            "optional_runtime_configs",
            "selected_config_sha256",
            "selected_config_sources",
            "tracked_auth_mapper_contract",
        },
        "source_inventory",
        failures,
    )
    if isinstance(source_hashes, dict) and source_inventory.get("full_byte_sources") != sorted(
        source_hashes
    ):
        failures.append("source inventory does not exactly match full-byte source hashes")
    if source_inventory.get("selected_config_sources") != EXPECTED_SELECTED_CONFIG_SOURCES:
        failures.append("selected non-secret configuration source set changed")
    if source_inventory.get("optional_runtime_configs") != EXPECTED_OPTIONAL_RUNTIME_CONFIGS:
        failures.append("optional runtime configuration presence changed")
    if source_inventory.get("tracked_auth_mapper_contract") != EXPECTED_TRACKED_AUTH_MAPPER_CONTRACT:
        failures.append("tracked auth mapper semantic contract changed")
    selected_hashes = source_inventory.get("selected_config_sha256")
    if not isinstance(selected_hashes, dict) or set(selected_hashes) != set(
        EXPECTED_SELECTED_CONFIG_SOURCES
    ):
        failures.append("selected configuration digest set changed")
    elif any(not SHA256_RE.fullmatch(str(value)) for value in selected_hashes.values()):
        failures.append("selected configuration digest is malformed")

    fingerprints = catalog.get("behavior_fingerprints")
    if not isinstance(fingerprints, dict) or not fingerprints:
        failures.append("behavior_fingerprints must be a non-empty object")
    else:
        if (
            len(fingerprints) != EXPECTED_FINGERPRINT_COUNT
            or _keyset_sha256(fingerprints) != EXPECTED_FINGERPRINT_KEYSET_SHA256
        ):
            failures.append("reviewed behavior fingerprint set changed")
        for value in fingerprints.values():
            line = value.get("line") if isinstance(value, dict) else None
            if (
                not isinstance(value, dict)
                or set(value) != {"ast_sha256", "line"}
                or not SHA256_RE.fullmatch(str(value.get("ast_sha256", "")))
                or isinstance(line, bool)
                or not isinstance(line, int)
                or line <= 0
            ):
                failures.append("behavior fingerprint is malformed")
                break

    provenance = catalog.get("provenance")
    if not isinstance(provenance, dict) or set(provenance) != set(EXPECTED_PROVENANCE):
        failures.append("reviewed provenance repository set changed")
        provenance = {}
    for name, (expected_head, expected_root, expected_selected) in EXPECTED_PROVENANCE.items():
        record = _check_exact_keys(
            provenance.get(name),
            {
                "contract_source_dirty_paths",
                "contract_sources_reconstructable_from_pinned_head",
                "pinned_head",
                "selected_contract_matches_pinned_head",
                "source_ref",
                "source_root",
            },
            f"{name} provenance",
            failures,
        )
        if record.get("pinned_head") != expected_head:
            failures.append(f"{name} pinned provenance changed")
        if record.get("source_root") != expected_root:
            failures.append(f"{name} source root changed")
        expected_ref = "1.2.25" if name == "pylon_repo" else "HEAD"
        if record.get("source_ref") != expected_ref:
            failures.append(f"{name} source ref changed")
        if record.get("contract_sources_reconstructable_from_pinned_head") is not True:
            failures.append(f"{name} contract is not reconstructable from its pinned head")
        if record.get("contract_source_dirty_paths") != []:
            failures.append(f"{name} reviewed contract has dirty source paths")
        selected = record.get("selected_contract_matches_pinned_head")
        if not isinstance(selected, dict) or set(selected) != expected_selected or any(
            value is not True for value in selected.values()
        ):
            failures.append(f"{name} selected provenance changed")

    serialized = json.dumps(catalog, sort_keys=True)
    if "DEFAULT_ADMIN_PASSWORD" in serialized or "credential_value\"" in serialized:
        failures.append("fixture contains a Form credential value or variable name")
    if _canonical_sha256(catalog) != EXPECTED_CATALOG_SHA256:
        failures.append("browser auth reviewed snapshot changed")
    return failures


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--catalog",
        type=Path,
        default=Path("testdata/baseline/browser-auth-http-contracts.json"),
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        catalog = json.loads(args.catalog.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print(f"cannot read browser auth evidence: {exc}", file=sys.stderr)
        return 1
    if not isinstance(catalog, dict):
        print("browser auth evidence root must be an object", file=sys.stderr)
        return 1
    failures = check_catalog(catalog)
    if failures:
        for failure in failures:
            print(f"- {failure}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
