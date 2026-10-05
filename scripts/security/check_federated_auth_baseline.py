#!/usr/bin/env python3
"""Validate the reviewed OIDC and SAML current-baseline evidence fixture."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from pathlib import Path
from typing import Any


EXPECTED_SCHEMA_VERSION = 1
EXPECTED_CATALOG_SHA256 = "7f104dcb8f2ef3bdc59a332f98b8b276b3e39170e868103babf8fae9aa5e2ebd"
EXPECTED_SOURCE_COUNT = 47
EXPECTED_SOURCE_KEYSET_SHA256 = "89ee769351815bb9d7086c8d8d5493512bc78a15066edf51a4304a146f4b5009"
EXPECTED_SOURCE_MAP_SHA256 = "c5f5281068eecb90dd86b312144ba98bfb3794b3d0462d74ce774bbb88921e9b"
EXPECTED_FINGERPRINT_COUNT = 45
EXPECTED_FINGERPRINT_KEYSET_SHA256 = "0e5d50c8883d792d145e2e59dffdf91d8412143f28963674936fb12d8b43f9f6"
EXPECTED_FINGERPRINT_MAP_SHA256 = "2bd5c8a903c59e05ddd7ffa6b53c73fd27b3e9195d9bf98a8e4186624185b79d"
EXPECTED_SELECTED_CONFIG_SHA256 = "82dd413e3753df0c931a0cf4a6c675d4308f59088b7c16bec14ab449639f8d57"
EXPECTED_BEHAVIOR_SHA256 = "d1f0c3c183acc1662ac165052cd6e8226d0ccecdb1b47f6430178ee49b35f144"
EXPECTED_CONSUMER_SHA256 = "e1e8a5088e9dfa2ddf5b2ab2fe8893c10a7c3cc66720b4a05b38c981c752bc9f"
EXPECTED_DISPOSITION_SHA256 = "2bb2d91ed804772958aa087f6d3c1a4705719b47bdfb5fe0aec5e758fdde995b"
EXPECTED_PROVENANCE_SHA256 = "9e285db72e0030b2069edc5e137ead3dfad83faf1aa2fff5c4eb943e125bb348"

EXPECTED_TOP_LEVEL_KEYS = {
    "behavior_contracts",
    "behavior_fingerprints",
    "configuration_contract",
    "consumer_contract",
    "inference_limits",
    "method_route_matrix",
    "provenance",
    "route_contracts",
    "schema_version",
    "scope",
    "security_dispositions",
    "selected_config_sha256",
    "source_files_sha256",
    "source_inventory",
    "source_reconstruction",
}

EXPECTED_BEHAVIOR_IDS = {
    "oidc.login",
    "oidc.callback",
    "oidc.logout",
    "oidc.logout_callback",
    "saml.login",
    "saml.acs",
    "saml.logout",
    "saml.sls",
    "shared.auth_context_commit",
    "shared.identity_provisioning",
    "shared.server_session",
    "shared.error_translation",
}

EXPECTED_DISPOSITIONS = {
    "oidc.external_contract": "preserve",
    "oidc.oauth_transaction": "correct",
    "oidc.token_validation": "correct",
    "oidc.callback_and_outbound_bounds": "correct",
    "oidc.secret_and_token_handling": "correct",
    "oidc.federated_logout": "phase",
    "saml.external_contract": "preserve",
    "saml.signature_boundary": "preserve_and_strengthen",
    "saml.protocol_semantics": "correct",
    "saml.replay_and_relay": "correct",
    "saml.input_and_logging": "correct",
    "saml.federated_logout": "correct",
    "shared.auth_commit": "correct",
    "shared.identity_provisioning": "preserve_and_transactionalize",
    "shared.provider_reference": "preserve_then_migrate",
    "shared.config_secret_distribution": "externalize",
}

EXPECTED_METHOD_ROUTE_MATRIX = {
    "/auth/auth_oidc/login": {
        "GET": "oidc.login",
        "HEAD": "framework.head_via_get",
        "OPTIONS": "framework.automatic_options",
    },
    "/auth/auth_oidc/login_callback": {
        "GET": "oidc.callback",
        "HEAD": "framework.head_via_get",
        "OPTIONS": "framework.automatic_options",
        "POST": "oidc.callback",
    },
    "/auth/auth_oidc/logout": {
        "GET": "oidc.logout",
        "HEAD": "framework.head_via_get",
        "OPTIONS": "framework.automatic_options",
    },
    "/auth/auth_oidc/logout_callback": {
        "GET": "oidc.logout_callback",
        "HEAD": "framework.head_via_get",
        "OPTIONS": "framework.automatic_options",
    },
    "/auth/auth_saml/acs": {
        "GET": "saml.acs",
        "HEAD": "framework.head_via_get",
        "OPTIONS": "framework.automatic_options",
        "POST": "saml.acs",
    },
    "/auth/auth_saml/login": {
        "GET": "saml.login",
        "HEAD": "framework.head_via_get",
        "OPTIONS": "framework.automatic_options",
    },
    "/auth/auth_saml/logout": {
        "GET": "saml.logout",
        "HEAD": "framework.head_via_get",
        "OPTIONS": "framework.automatic_options",
    },
    "/auth/auth_saml/sls": {
        "GET": "saml.sls",
        "HEAD": "framework.head_via_get",
        "OPTIONS": "framework.automatic_options",
        "POST": "saml.sls",
    },
}

EXPECTED_PROVENANCE = {
    "admin_repo": ("5b75ab70801546550b5611b492dcf265a84b9942", "centry/pylon_main/plugins/admin"),
    "auth_core_repo": ("fcc4c7a35fe095fb8d67e72451e3a4f9b497f871", "centry/pylon_auth/plugins/auth_core"),
    "auth_init_repo": ("f3e47ea0d3e64dc23d96e5475032e03dee256ab4", "centry/pylon_auth/plugins/auth_init"),
    "auth_mappers_repo": ("5a6934d4f9a6953e926e47a05192d7268a5e5a96", "centry/pylon_auth/plugins/auth_mappers"),
    "auth_oidc_repo": ("902a5c413e994e9a7ee5f27a46c255bde323a59b", "centry/pylon_auth/plugins/auth_oidc"),
    "auth_saml_repo": ("636009c0c1cf61ec0ad9a84d08934e6ecf07583f", "centry/pylon_auth/plugins/auth_saml"),
    "bootstrap_repo": ("a8cdbd31a90544fda9d2b96e3f936bba3594a22d", "centry/pylon_auth/plugins/bootstrap"),
    "elitea_core_repo": ("83cd27bd34dc41b30a9696ea5128da4e939b676e", "centry/pylon_main/plugins/elitea_core"),
    "main_auth_repo": ("ff02d66a8858604e6947bb3a52bda8543dbe0e76", "centry/pylon_main/plugins/auth"),
    "pylon_repo": ("6cc508803adffcb0f38573eda7a1ad45e2d4ca39", "pylon"),
    "runtime_config_repo": ("6b3e59f7f41e41c9d5f1dcf7ca6e870d7391986c", "centry"),
    "social_repo": ("52ab70413ab0ca54e23e67326217c35135e71128", "centry/pylon_main/plugins/social"),
    "ui_repo": ("4c43287cd5321785d280ae02eb597f2e260eeb8d", "EliteaUI"),
}

EXPECTED_OIDC_DEFAULTS = {
    "expiration_override": None,
    "login_mode": "post",
    "logout_mode": "get",
    "metadata_endpoint_verify": True,
    "require_email_verified": False,
    "target_response_type": "code",
    "target_scope": "openid profile email",
    "token_endpoint_auth": "basic",
    "token_endpoint_verify": True,
    "url_mode": "default",
    "without_expiration_claim_seconds": 86400,
    "without_jwt_public_key": "signature verification disabled",
}

EXPECTED_SAML_DEFAULTS = {
    "attributes_map": {},
    "authn_acs_url": "request-derived external ACS route",
    "authn_acs_url_add": True,
    "authn_nameid_format": "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified",
    "authn_sign": True,
    "authn_verify": True,
    "expiration_override": None,
    "fallback_to_nameid_for_email": True,
    "logout_mode": "post",
    "logout_sign": True,
    "logout_verify": True,
    "response_remap": {},
    "without_session_expiration_seconds": 86400,
}

SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
SAFE_CONFIG_STATES = {
    "absent",
    "empty",
    "environment_template_present_but_redacted",
    "inline_value_present_but_redacted",
}


def _canonical_sha256(value: Any) -> str:
    data = json.dumps(value, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(data).hexdigest()


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


def _contains(value: Any, *needles: str) -> bool:
    rendered = str(value)
    return all(needle in rendered for needle in needles)


def check_catalog(catalog: dict[str, Any]) -> list[str]:  # pylint: disable=R0912,R0915
    failures: list[str] = []
    if not isinstance(catalog, dict):
        return ["federated auth evidence must be an object"]
    if set(catalog) != EXPECTED_TOP_LEVEL_KEYS:
        failures.append("federated auth evidence top-level structure changed")
    if catalog.get("schema_version") != EXPECTED_SCHEMA_VERSION:
        failures.append("federated auth evidence schema version changed")

    scope = catalog.get("scope", {})
    if not isinstance(scope, dict) or set(scope) != {
        "claim",
        "covered_providers",
        "full_auth_parity_claim",
        "live_configured_or_deployed_parity_claim",
    }:
        failures.append("federated auth scope structure changed")
        scope = {}
    if scope.get("claim") != "bounded current-baseline OIDC and SAML authentication evidence":
        failures.append("current-baseline scope naming changed")
    if scope.get("covered_providers") != ["oidc", "saml"]:
        failures.append("reviewed federated provider set changed")
    if scope.get("full_auth_parity_claim") is not False:
        failures.append("fixture must not claim full Auth parity")
    if scope.get("live_configured_or_deployed_parity_claim") is not False:
        failures.append("fixture must not claim live configured or deployed parity")

    routes = catalog.get("method_route_matrix")
    if routes != EXPECTED_METHOD_ROUTE_MATRIX:
        failures.append("federated provider route and effective-method matrix changed")
    route_contracts = catalog.get("route_contracts")
    if not isinstance(route_contracts, dict) or len(route_contracts) != 8:
        failures.append("source-derived federated route record set changed")
    elif {
        record.get("effective_route"): record.get("effective_methods")
        for record in route_contracts.values()
        if isinstance(record, dict)
    } != {
        route: list(methods)
        for route, methods in EXPECTED_METHOD_ROUTE_MATRIX.items()
    }:
        failures.append("source-derived federated route records changed")

    configuration = catalog.get("configuration_contract", {})
    if not isinstance(configuration, dict):
        failures.append("configuration_contract must be an object")
        configuration = {}
    activation = configuration.get("checked_in_activation", {})
    expected_activation = {
        "auth_core_selected_provider": "form",
        "bootstrap_loads_oidc": True,
        "bootstrap_loads_saml": True,
        "checked_in_oidc_required_material_present": False,
        "checked_in_saml_required_material_present": False,
        "live_runtime_or_deployment_examined": False,
        "oidc_selected_by_checked_in_auth_core": False,
        "ready_or_deployed_parity_claim": False,
        "saml_selected_by_checked_in_auth_core": False,
        "scope": "checked-in source and configuration only",
    }
    if activation != expected_activation:
        failures.append("checked-in federated-provider activation evidence changed")
    if configuration.get("merge_precedence") != [
        "plugin config.yml base",
        "context.settings.configs.<plugin> seed",
        "configured /data/configs/<plugin>.yml provider override",
    ] or configuration.get("merge_source") != (
        "pylon/core/tools/module/descriptor.py#ModuleDescriptor.load_config"
    ):
        failures.append("Pylon plugin configuration merge precedence changed")

    oidc = configuration.get("oidc", {})
    saml = configuration.get("saml", {})
    if not isinstance(oidc, dict) or oidc.get("hidden_code_defaults") != EXPECTED_OIDC_DEFAULTS:
        failures.append("OIDC hidden code defaults changed")
        oidc = {}
    if oidc.get("runtime_discovery_mutates_endpoints") != [
        "authorization_endpoint",
        "token_endpoint",
        "userinfo_endpoint",
        "end_session_endpoint",
    ]:
        failures.append("OIDC metadata endpoint-copy contract changed")
    oidc_admin = oidc.get("admin_schema", {})
    if oidc_admin != {
        "present": True,
        "property_keys": ["oidc_client_id", "oidc_client_secret", "oidc_metadata_endpoint"],
        "secret_field_format": "password",
    }:
        failures.append("OIDC admin configuration schema contract changed")
    if not isinstance(saml, dict) or saml.get("hidden_code_defaults") != EXPECTED_SAML_DEFAULTS:
        failures.append("SAML hidden code defaults changed")
        saml = {}
    if saml.get("admin_schema") != {
        "auth_core_provider_enum_contains_saml": False,
        "present": False,
    } or saml.get("checked_in_override") != {
        "path": "centry/pylon_auth/configs/auth_saml.yml",
        "present": False,
    }:
        failures.append("SAML absent admin/runtime configuration evidence changed")

    for name, fields in (
        ("OIDC base", oidc.get("base", {}).get("fields", {})),
        ("OIDC override", oidc.get("checked_in_override", {}).get("fields", {})),
        ("SAML base", saml.get("base", {}).get("fields", {})),
    ):
        if not isinstance(fields, dict) or any(value not in SAFE_CONFIG_STATES for value in fields.values()):
            failures.append(f"{name} secret-bearing values are not safely redacted")

    contracts = _records_by_id(catalog.get("behavior_contracts"), "behavior_contracts", failures)
    if set(contracts) != EXPECTED_BEHAVIOR_IDS:
        failures.append("reviewed federated behavior contract set changed")
    oidc_login = contracts.get("oidc.login", {}).get("current", {})
    if not _contains(oidc_login.get("pending_state_growth", ""), "no explicit bound", "TTL"):
        failures.append("OIDC abandoned-state growth risk is no longer explicit")
    if not _contains(
        oidc_login.get("response", ""),
        "login_mode=get",
        "200 auto-submit HTML POST",
        "response_type",
        "client_id",
        "redirect_uri",
        "scope",
        "state",
    ):
        failures.append("OIDC initiation transport and field contract changed")
    oidc_callback = contracts.get("oidc.callback", {}).get("current", {})
    if not _contains(oidc_callback.get("state", ""), "pop member before", "provisioning"):
        failures.append("OIDC one-time state consumption ordering changed")
    if not _contains(oidc_callback.get("validation", ""), "signature verification disabled", "require sub"):
        failures.append("OIDC unverified-token baseline risk is no longer explicit")
    if oidc_callback.get("claim_mapping", {}).get("provider_reference") != (
        "preferred_username, otherwise sub"
    ):
        failures.append("OIDC provider-reference mapping changed")
    saml_acs = contracts.get("saml.acs", {}).get("current", {})
    if not _contains(saml_acs.get("positive_signature_behavior", ""), "pins configured idp_cert", "signed_xml"):
        failures.append("SAML signed XML positive behavior changed")
    if not _contains(
        saml_acs.get("protocol_checks", ""),
        "InResponseTo",
        "issuer",
        "audience",
        "Destination",
        "Recipient",
        "Conditions",
        "SubjectConfirmation",
    ):
        failures.append("SAML missing protocol-semantic checks are no longer explicit")
    if not _contains(saml_acs.get("wire_inputs", ""), "base64/XML", "unbounded"):
        failures.append("SAML callback input-bound risk changed")
    if saml_acs.get("claim_mapping", {}).get("provider_reference") != "bare NameID":
        failures.append("SAML bare NameID mapping changed")
    shared_commit = contracts.get("shared.auth_context_commit", {}).get("current", {})
    if not _contains(shared_commit.get("flaw", ""), "done=true", "before Auth Init", "later /auth"):
        failures.append("pre-provisioning authenticated-session defect is no longer explicit")
    provisioning = contracts.get("shared.identity_provisioning", {}).get("current", {})
    if not _contains(provisioning, "link same-email user", "root group 1", "last_login", "super_admin"):
        failures.append("identity-provisioning business side effects changed")

    consumers = catalog.get("consumer_contract", {})
    if _canonical_sha256(consumers) != EXPECTED_CONSUMER_SHA256:
        failures.append("reviewed UI, gateway, mapper, and profile consumer contract changed")
    if not _contains(consumers, "/auth", "X-Auth-Type", "provider_attr.attributes.picture"):
        failures.append("federated-auth downstream consumer evidence is incomplete")

    dispositions = _records_by_id(
        catalog.get("security_dispositions"), "security_dispositions", failures
    )
    observed_dispositions = {
        identifier: record.get("migration") for identifier, record in dispositions.items()
    }
    if observed_dispositions != EXPECTED_DISPOSITIONS:
        failures.append("reviewed federated security migration disposition map changed")
    if not _contains(dispositions.get("oidc.token_validation", {}), "JWKS", "issuer", "audience", "nonce", "no unverified mode"):
        failures.append("OIDC strict token-validation correction changed")
    if not _contains(dispositions.get("saml.protocol_semantics", {}), "InResponseTo", "Destination", "Conditions"):
        failures.append("SAML protocol-semantic correction changed")
    if not _contains(dispositions.get("shared.config_secret_distribution", {}), "raw config_data", "SP keys", "client secrets", "never broadcast"):
        failures.append("raw provider-configuration distribution risk changed")
    if not _contains(dispositions.get("shared.provider_reference", {}), "globally unique raw provider_ref", "explicit data migration"):
        failures.append("provider-reference compatibility and migration contract changed")

    source_hashes = catalog.get("source_files_sha256")
    if not isinstance(source_hashes, dict):
        failures.append("source_files_sha256 must be an object")
        source_hashes = {}
    if len(source_hashes) != EXPECTED_SOURCE_COUNT or _canonical_sha256(sorted(source_hashes)) != EXPECTED_SOURCE_KEYSET_SHA256:
        failures.append("reviewed federated source file set changed")
    if any(not isinstance(value, str) or SHA256_RE.fullmatch(value) is None for value in source_hashes.values()):
        failures.append("reviewed source hash is malformed")
    if _canonical_sha256(source_hashes) != EXPECTED_SOURCE_MAP_SHA256:
        failures.append("reviewed federated source bytes changed")

    fingerprints = catalog.get("behavior_fingerprints")
    if not isinstance(fingerprints, dict):
        failures.append("behavior_fingerprints must be an object")
        fingerprints = {}
    if len(fingerprints) != EXPECTED_FINGERPRINT_COUNT or _canonical_sha256(sorted(fingerprints)) != EXPECTED_FINGERPRINT_KEYSET_SHA256:
        failures.append("reviewed federated behavior fingerprint set changed")
    for value in fingerprints.values():
        if not isinstance(value, dict) or set(value) != {"ast_sha256", "class", "function", "line", "source"} or (
            not isinstance(value.get("ast_sha256"), str)
            or SHA256_RE.fullmatch(value["ast_sha256"]) is None
            or not isinstance(value.get("line"), int)
            or isinstance(value.get("line"), bool)
        ):
            failures.append("behavior fingerprint is malformed")
            break
    if _canonical_sha256(fingerprints) != EXPECTED_FINGERPRINT_MAP_SHA256:
        failures.append("reviewed federated behavior implementation changed")

    selected_hashes = catalog.get("selected_config_sha256")
    if not isinstance(selected_hashes, dict) or any(
        not isinstance(value, str) or SHA256_RE.fullmatch(value) is None
        for value in selected_hashes.values()
    ):
        failures.append("selected configuration hash inventory is malformed")
    if _canonical_sha256(selected_hashes) != EXPECTED_SELECTED_CONFIG_SHA256:
        failures.append("reviewed selected federated configuration changed")

    provenance = catalog.get("provenance")
    if not isinstance(provenance, dict) or set(provenance) != set(EXPECTED_PROVENANCE):
        failures.append("federated provenance repository set changed")
        provenance = {}
    for name, (head, source_root) in EXPECTED_PROVENANCE.items():
        record = provenance.get(name, {})
        if record.get("pinned_head") != head:
            failures.append(f"{name} pinned provenance changed")
        if record.get("source_root") != source_root or Path(str(record.get("source_root", ""))).is_absolute():
            failures.append(f"{name} logical source root changed")
        if name == "runtime_config_repo":
            if record.get("selected_values_reconstructable_from_pinned_head") is not True:
                failures.append("selected runtime configuration no longer matches pinned Centry head")
            selected_sources = record.get("selected_sources", {})
            if not isinstance(selected_sources, dict) or any(
                item.get("selected_value_matches_pinned_head") is not True
                for item in selected_sources.values()
                if isinstance(item, dict)
            ):
                failures.append("runtime selector provenance is incomplete")
        elif record.get("contract_sources_reconstructable_from_pinned_head") is not True or record.get("contract_source_dirty_paths") != []:
            failures.append(f"{name} reviewed contract source is dirty")
    if _canonical_sha256(provenance) != EXPECTED_PROVENANCE_SHA256:
        failures.append("reviewed federated provenance snapshot changed")

    if _canonical_sha256(catalog.get("behavior_contracts")) != EXPECTED_BEHAVIOR_SHA256:
        failures.append("reviewed federated behavior narrative changed")
    if _canonical_sha256(catalog.get("security_dispositions")) != EXPECTED_DISPOSITION_SHA256:
        failures.append("reviewed federated security disposition detail changed")
    if _canonical_sha256(catalog) != EXPECTED_CATALOG_SHA256:
        failures.append("reviewed federated auth snapshot changed")
    return failures


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "catalog",
        nargs="?",
        type=Path,
        default=Path("testdata/baseline/federated-auth-contracts.json"),
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    try:
        catalog = json.loads(args.catalog.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print(json.dumps({"error": str(exc)}, sort_keys=True), file=sys.stderr)
        return 2
    failures = check_catalog(catalog)
    if failures:
        print(json.dumps({"errors": failures}, indent=2, sort_keys=True), file=sys.stderr)
        return 1
    print(json.dumps({"ok": True, "catalog": args.catalog.as_posix()}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
