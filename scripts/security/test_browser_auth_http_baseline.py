#!/usr/bin/env python3
"""Focused dependency-free tests for browser/Form authentication evidence."""

from __future__ import annotations

import ast
import json
import sys
import tempfile
import unittest
from copy import deepcopy
from pathlib import Path


sys.path.insert(0, str(Path(__file__).resolve().parent))

import export_browser_auth_http_baseline as exporter  # noqa: E402
from check_browser_auth_http_baseline import (  # noqa: E402
    EXPECTED_TRACKED_AUTH_MAPPER_CONTRACT,
    check_catalog,
)


CATALOG = Path(__file__).resolve().parents[2] / "testdata/baseline/browser-auth-http-contracts.json"


class BrowserAuthHTTPBaselineTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.catalog = json.loads(CATALOG.read_text(encoding="utf-8"))

    def test_checked_fixture_is_internally_valid(self) -> None:
        self.assertEqual(check_catalog(self.catalog), [])

    def test_checker_rejects_scope_and_route_overclaims(self) -> None:
        changed = deepcopy(self.catalog)
        changed["scope"]["full_auth_parity_claim"] = True
        changed["scope"]["current_root_auth_alias"] = True
        route = next(iter(changed["effective_routes"].values()))
        route["effective_route"] = "/auth"
        failures = check_catalog(changed)
        self.assertIn("fixture must not claim full Auth parity", failures)
        self.assertIn("fixture must not invent a current root /auth alias", failures)
        self.assertIn("effective browser/Form route matrix changed", failures)

    def test_checker_rejects_form_and_processor_defect_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        contracts = {item["id"]: item for item in changed["behavior_contracts"]}
        contracts["form.authorize.post"]["failure"]["target_preserved"] = True
        contracts["form.authorize.post"]["configuration_shape"][
            "top_level_email_consumed_by_route"
        ] = True
        dispositions = {item["id"]: item for item in changed["security_dispositions"]}
        dispositions["session.processor_failure"]["baseline"] = "no partial state"
        failures = check_catalog(changed)
        self.assertIn("Form failed-login target-loss behavior changed", failures)
        self.assertIn("Form runtime/admin-schema mismatch changed", failures)
        self.assertIn("processor partial-authentication defect is no longer explicit", failures)

    def test_checker_rejects_provenance_and_snapshot_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        changed["provenance"]["auth_form_repo"]["pinned_head"] = "0" * 40
        changed["source_files_sha256"]["auth_form/routes/form.py"] = "0" * 64
        failures = check_catalog(changed)
        self.assertIn("auth_form_repo pinned provenance changed", failures)
        self.assertIn("browser auth reviewed snapshot changed", failures)

    def test_checker_rejects_source_and_fingerprint_inventory_renames(self) -> None:
        changed = deepcopy(self.catalog)
        source = "auth_form/routes/form.py"
        renamed_source = "auth_form/routes/form_v2.py"
        changed["source_files_sha256"][renamed_source] = changed[
            "source_files_sha256"
        ].pop(source)
        inventory = changed["source_inventory"]["full_byte_sources"]
        inventory[inventory.index(source)] = renamed_source
        inventory.sort()

        fingerprint = "auth_form/routes/form.py#Route.authorize"
        renamed_fingerprint = "auth_form/routes/form.py#Route.authorize_v2"
        changed["behavior_fingerprints"][renamed_fingerprint] = changed[
            "behavior_fingerprints"
        ].pop(fingerprint)

        failures = check_catalog(changed)
        self.assertIn("reviewed source file set changed", failures)
        self.assertIn("reviewed behavior fingerprint set changed", failures)

    def test_checker_rejects_route_key_rename_with_unchanged_effective_url(self) -> None:
        changed = deepcopy(self.catalog)
        source_key = "auth_core/routes/auth.py#Route.auth"
        changed["effective_routes"][f"{source_key}_renamed"] = changed[
            "effective_routes"
        ].pop(source_key)

        failures = check_catalog(changed)
        self.assertNotIn("effective browser/Form route matrix changed", failures)
        self.assertIn("source-derived browser/Form route records changed", failures)

    def test_checker_rejects_allowed_but_wrong_security_disposition(self) -> None:
        changed = deepcopy(self.catalog)
        dispositions = {item["id"]: item for item in changed["security_dispositions"]}
        dispositions["form.csrf"]["migration"] = "preserve_and_strengthen"

        failures = check_catalog(changed)
        self.assertIn("reviewed security migration disposition map changed", failures)

    def test_checker_rejects_new_wire_compatibility_disposition_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        dispositions = {item["id"]: item for item in changed["security_dispositions"]}
        dispositions["wire.unsupported_method"]["requirement"] = "plain 400"
        dispositions["credential.basic_decoding"]["baseline"] = "strict decoder"
        dispositions["input.duplicates"]["requirement"] = "accept duplicates"
        dispositions["proxy.trust"]["requirement"] = "trust proxy"
        dispositions["public_rules.regex_engine"]["requirement"] = "compile regex"

        failures = check_catalog(changed)
        self.assertIn("unsupported-method wire correction disposition changed", failures)
        self.assertIn("strict Basic-decoding disposition changed", failures)
        self.assertIn("duplicate input disposition changed", failures)
        self.assertIn("trusted-proxy normalization disposition changed", failures)
        self.assertIn("public-rule regex-engine disposition changed", failures)

    def test_checker_rejects_removed_fail_open_and_partial_state_evidence(self) -> None:
        changed = deepcopy(self.catalog)
        contracts = {item["id"]: item for item in changed["behavior_contracts"]}
        main_rpc = contracts["browser.main_rpc_authorize"]
        main_rpc["credential_failure_divergence"] = "deny"
        main_rpc["transport_failure"] = "deny"
        auth_init = contracts["browser.auth_init.processor"]
        auth_init["failure_branches"] = []
        auth_init["startup_failure_branches"] = []

        failures = check_catalog(changed)
        self.assertIn("main RPC credential traversal divergence changed", failures)
        self.assertIn("main RPC public-after-invalid branch changed", failures)
        self.assertIn("main RPC fail-open transport defect is no longer explicit", failures)
        self.assertIn("Auth Init failure branch missing: AUTOCOMMIT", failures)
        self.assertIn("Auth Init partial bootstrap branch changed", failures)

    def test_checker_rejects_newly_reviewed_boundary_semantic_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        contracts = {item["id"]: item for item in changed["behavior_contracts"]}
        contracts["browser.auth.get"]["target_query_semantics"][
            "explicit_empty"
        ] = "same as absent"
        main_rpc = contracts["browser.main_rpc_authorize"]
        main_rpc["cache_contract"]["key_omissions"] = []
        main_rpc["local_public_override"]["rpc_short_circuited"] = True
        main_rpc["migration_transport"] = "retain Redis RPC"
        contracts["browser.cors_options_replacement"][
            "auth_core_code_capability"
        ] = "all headers preserved"

        failures = check_catalog(changed)
        self.assertIn("absent versus empty ForwardAuth target semantics changed", failures)
        self.assertIn("unsafe Main authorization-cache key disposition changed", failures)
        self.assertIn("Main local-public override ordering changed", failures)
        self.assertIn("auth_authorize in-process merge disposition changed", failures)
        self.assertIn("Auth Core CORS Server-header loss is no longer explicit", failures)

    def test_checker_rejects_http_status_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        outcomes = {item["id"]: item for item in changed["http_outcomes"]}
        outcomes["browser.info.denied"]["response"]["status"] = 401
        outcomes["exposure.outer_unsupported_method"]["response"]["status"] = 400
        outcomes["exposure.timeout"]["response"]["status"] = 503

        failures = check_catalog(changed)
        self.assertIn("unknown/malformed info mapper outcome changed", failures)
        self.assertIn("outer unsupported-method outcome changed", failures)
        self.assertIn("exposure timeout outcome changed", failures)

    def test_checker_rejects_ui_auth_surface_and_logout_consumer_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        changed["ui_contract"]["api_auth_surface"]["effective_prefix"] = "/auth"
        changed["ui_contract"]["logout_consumer_sources"].pop()

        failures = check_catalog(changed)
        self.assertIn("direct browser logout consumer source set changed", failures)
        self.assertIn("EliteaUI API-relative Auth surface changed", failures)

    def test_checker_rejects_unknown_nested_fields_without_echoing_values(self) -> None:
        changed = deepcopy(self.catalog)
        sentinel = "SYNTHETIC_SECRET_MUST_NOT_APPEAR"
        changed["deployment_contract"]["unexpected"] = sentinel
        fingerprint = next(iter(changed["behavior_fingerprints"].values()))
        fingerprint["unexpected"] = sentinel
        fingerprint["line"] = True

        failures = check_catalog(changed)
        self.assertIn("deployment_contract structure changed", failures)
        self.assertIn("behavior fingerprint is malformed", failures)
        self.assertNotIn(sentinel, " ".join(failures))

    def test_checker_rejects_runtime_and_selected_config_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        changed["framework_contract"]["pylon_runtime_ref"] = "worktree"
        changed["provenance"]["pylon_repo"]["source_ref"] = "HEAD"
        changed["deployment_contract"]["runtime_composition"][
            "pylon_auth.web_runtime"
        ] = "waitress"
        selected_sources = changed["source_inventory"]["selected_config_sources"]
        selected_sources.append(selected_sources[0])
        changed["source_inventory"]["optional_runtime_configs"][0]["present"] = True

        failures = check_catalog(changed)
        self.assertIn("pinned browser-auth framework contract changed", failures)
        self.assertIn("pylon_repo source ref changed", failures)
        self.assertIn("tracked Pylon image/runtime composition changed", failures)
        self.assertIn("selected non-secret configuration source set changed", failures)
        self.assertIn("optional runtime configuration presence changed", failures)

    def test_fixture_pins_auth_mapper_permission_evidence(self) -> None:
        sources = self.catalog["source_files_sha256"]
        for source in (
            "auth_mappers/methods/tools.py",
            "auth_mappers/requirements.txt",
        ):
            self.assertIn(source, sources)

        fingerprints = self.catalog["behavior_fingerprints"]
        for fingerprint in (
            "auth_core/rpc/roles.py#RPC.get_token_permissions",
            "auth_core/rpc/roles.py#RPC.get_user_permissions",
            "auth_mappers/methods/tools.py#Method.have_requirement",
        ):
            self.assertIn(fingerprint, fingerprints)

        self.assertEqual(
            self.catalog["source_inventory"]["tracked_auth_mapper_contract"],
            EXPECTED_TRACKED_AUTH_MAPPER_CONTRACT,
        )

    def test_fixture_pins_resolved_form_snapshot_bridge(self) -> None:
        self.assertIn(
            "auth_form/form_export.py",
            self.catalog["source_files_sha256"],
        )
        self.assertIn(
            "auth_form/module.py#Module.deinit",
            self.catalog["behavior_fingerprints"],
        )
        contracts = {
            contract["id"]: contract
            for contract in self.catalog["behavior_contracts"]
        }
        bridge = contracts["form.resolved_snapshot_bridge"]
        self.assertEqual(bridge["payload"]["maximum_bytes"], 1 << 20)
        self.assertEqual(bridge["payload"]["maximum_users"], 256)
        self.assertEqual(
            bridge["lifecycle"]["in_place_reconfiguration"],
            "unsupported; restart or plugin reload required",
        )

    def test_optional_auth_mapper_runtime_config_inventory_never_reads_contents(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            centry_root = Path(temp_dir)
            config_path = centry_root / "pylon_auth/configs/auth_mappers.yml"

            absent = exporter._optional_runtime_config_inventory(centry_root)
            self.assertEqual(absent[0]["present"], False)

            config_path.parent.mkdir(parents=True)
            config_path.write_text(
                "credential: SYNTHETIC_SECRET_MUST_NOT_APPEAR\n", encoding="utf-8"
            )
            present = exporter._optional_runtime_config_inventory(centry_root)

            self.assertEqual(present[0]["present"], True)
            self.assertEqual(present[0]["contents_exported"], False)
        self.assertNotIn("SYNTHETIC_SECRET_MUST_NOT_APPEAR", json.dumps(present))

    def test_checker_rejects_tracked_auth_mapper_semantic_mutation(self) -> None:
        changed = deepcopy(self.catalog)
        changed["source_inventory"]["tracked_auth_mapper_contract"]["header"][
            "requirements_present"
        ] = True
        self.assertIn(
            "tracked auth mapper semantic contract changed", check_catalog(changed)
        )

    def test_checker_rejects_main_public_rule_ownership_order_and_value_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        ownership = changed["deployment_contract"]["public_rule_ownership"]
        ownership["auth_core_direct"]["initial_rules"] = [
            {"uri": "/must-not-be-main-local"}
        ]
        configured = ownership["main_local"]["configured_registration_order"]
        configured[0], configured[1] = configured[1], configured[0]
        dynamic = ownership["main_local"]["dynamic_registration_sites"]
        next(item for item in dynamic if item["id"] == "artifacts.s3_sigv4")[
            "rule"
        ]["uri"] = "/artifacts/.*"
        next(item for item in dynamic if item["id"] == "elitea_core.public_messages")[
            "effective_in_tracked_base_config"
        ] = False

        failures = check_catalog(changed)
        self.assertIn("tracked Main/Auth Core public-rule ownership changed", failures)
        self.assertIn("browser auth reviewed snapshot changed", failures)

    def test_public_route_config_selectors_pin_conditional_and_prefix_values(self) -> None:
        self.assertEqual(
            exporter._selected_litellm_public_config("url_prefix: /llm\nunrelated: yes\n"),
            {"url_prefix": "/llm"},
        )
        self.assertEqual(
            exporter._selected_elitea_core_public_config(
                "public_messages_route: true\nunrelated: yes\n"
            ),
            {"public_messages_route": True},
        )
        self.assertEqual(
            exporter._selected_elitea_core_public_override("unrelated: yes\n"),
            {"public_messages_route": None},
        )
        with self.assertRaisesRegex(ValueError, "explicit boolean"):
            exporter._selected_elitea_core_public_config(
                "public_messages_route: ${PUBLIC_MESSAGES}\n"
            )

    def test_route_extraction_pins_literal_methods(self) -> None:
        tree = ast.parse(
            "from pylon.core.tools import web\n"
            "class Route:\n"
            "    @web.route('/authorize', methods=['POST'])\n"
            "    def authorize(self):\n"
            "        return None\n"
        )
        node = exporter._method(tree, "Route", "authorize")
        self.assertEqual(exporter._route_spec(node), ("/authorize", ["POST"]))

    def test_ast_fingerprint_ignores_comments_but_detects_logic(self) -> None:
        first = ast.parse("class Route:\n    def login(self):\n        # one\n        return 1\n")
        second = ast.parse("class Route:\n    def login(self):\n        return 1  # two\n")
        changed = ast.parse("class Route:\n    def login(self):\n        return 2\n")
        first_hash = exporter._fingerprint(exporter._method(first, "Route", "login"))[
            "ast_sha256"
        ]
        second_hash = exporter._fingerprint(exporter._method(second, "Route", "login"))[
            "ast_sha256"
        ]
        changed_hash = exporter._fingerprint(exporter._method(changed, "Route", "login"))[
            "ast_sha256"
        ]
        self.assertEqual(first_hash, second_hash)
        self.assertNotEqual(first_hash, changed_hash)

    def test_yaml_list_accepts_same_indent_as_mapping_key(self) -> None:
        self.assertEqual(
            exporter._yaml_list("initial_global_admins:\n- admin\n- operator\n", "initial_global_admins"),
            ["admin", "operator"],
        )
        self.assertEqual(
            exporter._yaml_list(
                "exposure:\n  handle:\n    prefixes:\n    - /auth\n",
                "exposure.handle.prefixes",
            ),
            ["/auth"],
        )

    def test_form_config_never_exports_credential_bytes(self) -> None:
        evidence = exporter._safe_form_config(
            "users:\n- login: admin\n  password: SYNTHETIC_TEST_CREDENTIAL\n"
        )
        self.assertEqual(evidence["credential_sources"], ["inline_value_present_but_redacted"])
        self.assertNotIn("SYNTHETIC_TEST_CREDENTIAL", json.dumps(evidence))
        self.assertFalse(evidence["credential_values_exported"])

    def test_form_config_digest_uses_only_redacted_source_sentinels(self) -> None:
        first = exporter._safe_form_config(
            "users:\n- login: admin\n  password: SYNTHETIC_CREDENTIAL_ONE\n"
        )
        second = exporter._safe_form_config(
            "users:\n- login: admin\n  password: SYNTHETIC_CREDENTIAL_TWO\n"
        )
        environment = exporter._safe_form_config(
            "users:\n- login: admin\n  password: ${FORM_ADMIN_PASSWORD}\n"
        )

        self.assertEqual(first, second)
        self.assertEqual(
            exporter._canonical_sha256(first),
            exporter._canonical_sha256(second),
        )
        self.assertNotEqual(first, environment)
        self.assertEqual(
            environment["credential_sources"],
            ["environment_expansion"],
        )

    def test_form_config_is_scoped_to_top_level_users(self) -> None:
        evidence = exporter._safe_form_config(
            "metadata:\n"
            "- login: ignored\n"
            "  password: IGNORED_SECRET\n"
            "users:\n"
            "- login: expected\n"
            "  password: EXPECTED_SECRET\n"
            "  attributes:\n"
            "    password: NESTED_SECRET\n"
            "groups:\n"
            "- login: ignored_after_users\n"
        )
        self.assertEqual(evidence["configured_user_count"], 1)
        self.assertEqual(
            evidence["configured_user_keys"], [["attributes", "login", "password"]]
        )
        self.assertEqual(
            evidence["credential_sources"], ["inline_value_present_but_redacted"]
        )
        serialized = json.dumps(evidence)
        self.assertNotIn("IGNORED_SECRET", serialized)
        self.assertNotIn("EXPECTED_SECRET", serialized)
        self.assertNotIn("NESTED_SECRET", serialized)

    def test_main_auth_config_is_scoped_to_top_level_public_rules(self) -> None:
        selected = exporter._selected_main_auth_config(
            "unrelated:\n"
            "- uri: /ignored-before\n"
            "auth_mode: rpc\n"
            "public_rules:\n"
            "- uri: /expected\n"
            "  methods: [GET]\n"
            "other:\n"
            "- uri: /ignored-after\n"
        )
        self.assertEqual(
            selected, {"auth_mode": "rpc", "public_uri_rules": ["/expected"]}
        )

    def test_runtime_service_selector_ignores_unrelated_services(self) -> None:
        selected = exporter._selected_runtime_services(
            "services:\n"
            "  pylon_auth:\n"
            "    image: ghcr.io/eliteaai/pylon:1.2.25\n"
            "    environment:\n"
            "      - PYLON_WEB_RUNTIME=gevent\n"
            "  unrelated:\n"
            "    image: example.invalid/ignored:latest\n"
            "    environment:\n"
            "      - PYLON_WEB_RUNTIME=ignored\n"
            "  pylon_main:\n"
            "    image: ghcr.io/eliteaai/pylon:1.2.25\n"
            "    environment:\n"
            "      - PYLON_WEB_RUNTIME=gevent\n"
        )
        self.assertEqual(
            selected,
            {
                "pylon_auth.image": "ghcr.io/eliteaai/pylon:1.2.25",
                "pylon_auth.web_runtime": "gevent",
                "pylon_main.image": "ghcr.io/eliteaai/pylon:1.2.25",
                "pylon_main.web_runtime": "gevent",
            },
        )

    def test_route_composition_mismatch_is_value_free(self) -> None:
        changed = deepcopy(self.catalog["deployment_contract"])
        sentinel = "/SYNTHETIC_SECRET_PATH"
        changed["pylon_main"]["browser_auth_exposure"][
            "exposure.handle.prefixes"
        ] = [sentinel]
        with self.assertRaises(ValueError) as raised:
            exporter._route_composition(changed)
        self.assertNotIn(sentinel, str(raised.exception))

    def test_selected_pylon_contract_ignores_unrelated_server_tuning(self) -> None:
        baseline = """
server:
  path: /auth/
  proxy:
    x_for: 1
    x_proto: 1
    x_host: 1
sessions:
  prefix: auth_
application:
  APPLICATION_ROOT: /auth/
"""
        tuned = baseline.replace("  path: /auth/", "  path: /auth/\n  kwargs:\n    spawn: 512")
        changed = baseline.replace("  prefix: auth_", "  prefix: changed_")
        self.assertEqual(exporter._selected_auth_pylon(baseline), exporter._selected_auth_pylon(tuned))
        self.assertNotEqual(exporter._selected_auth_pylon(baseline), exporter._selected_auth_pylon(changed))

    def test_fixture_keeps_ui_correlation_outside_server_auth(self) -> None:
        self.assertIn(
            "not provider assertion validation",
            self.catalog["ui_contract"]["security_boundary"],
        )
        self.assertEqual(
            self.catalog["ui_contract"]["reauthentication"]["callback_route"],
            "/auth-callback",
        )


if __name__ == "__main__":
    unittest.main()
