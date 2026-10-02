#!/usr/bin/env python3
"""Focused dependency-free tests for OIDC and SAML baseline evidence."""

from __future__ import annotations

import ast
import json
import sys
import unittest
from copy import deepcopy
from pathlib import Path


sys.path.insert(0, str(Path(__file__).resolve().parent))

import export_federated_auth_baseline as exporter  # noqa: E402
from check_federated_auth_baseline import check_catalog  # noqa: E402


CATALOG = Path(__file__).resolve().parents[2] / "testdata/baseline/federated-auth-contracts.json"


class FederatedAuthBaselineTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.catalog = json.loads(CATALOG.read_text(encoding="utf-8"))

    def test_checked_fixture_is_internally_valid(self) -> None:
        self.assertEqual(check_catalog(self.catalog), [])

    def test_checker_rejects_scope_and_live_deployment_overclaims(self) -> None:
        changed = deepcopy(self.catalog)
        changed["scope"]["full_auth_parity_claim"] = True
        changed["scope"]["live_configured_or_deployed_parity_claim"] = True
        activation = changed["configuration_contract"]["checked_in_activation"]
        activation["ready_or_deployed_parity_claim"] = True

        failures = check_catalog(changed)
        self.assertIn("fixture must not claim full Auth parity", failures)
        self.assertIn("fixture must not claim live configured or deployed parity", failures)
        self.assertIn("checked-in federated-provider activation evidence changed", failures)

    def test_checker_rejects_route_and_effective_method_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        route = "/auth/auth_saml/acs"
        changed["method_route_matrix"][route].pop("GET")
        record = next(
            item
            for item in changed["route_contracts"].values()
            if item["effective_route"] == route
        )
        record["effective_methods"].remove("HEAD")

        failures = check_catalog(changed)
        self.assertIn("federated provider route and effective-method matrix changed", failures)
        self.assertIn("source-derived federated route records changed", failures)

    def test_checker_rejects_config_merge_and_hidden_default_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        configuration = changed["configuration_contract"]
        configuration["merge_precedence"].reverse()
        configuration["oidc"]["hidden_code_defaults"]["require_email_verified"] = True
        configuration["saml"]["hidden_code_defaults"]["authn_verify"] = False
        configuration["saml"]["admin_schema"]["present"] = True

        failures = check_catalog(changed)
        self.assertIn("Pylon plugin configuration merge precedence changed", failures)
        self.assertIn("OIDC hidden code defaults changed", failures)
        self.assertIn("SAML hidden code defaults changed", failures)
        self.assertIn("SAML absent admin/runtime configuration evidence changed", failures)

    def test_checker_rejects_oidc_security_evidence_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        contracts = {item["id"]: item for item in changed["behavior_contracts"]}
        callback = contracts["oidc.callback"]["current"]
        callback["state"] = "state retained"
        callback["validation"] = "always verified"
        callback["claim_mapping"]["provider_reference"] = "issuer/sub"
        contracts["oidc.login"]["current"]["pending_state_growth"] = "bounded"
        contracts["oidc.login"]["current"]["response"] = "redirect only"

        failures = check_catalog(changed)
        self.assertIn("OIDC abandoned-state growth risk is no longer explicit", failures)
        self.assertIn("OIDC initiation transport and field contract changed", failures)
        self.assertIn("OIDC one-time state consumption ordering changed", failures)
        self.assertIn("OIDC unverified-token baseline risk is no longer explicit", failures)
        self.assertIn("OIDC provider-reference mapping changed", failures)

    def test_checker_rejects_saml_security_evidence_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        contracts = {item["id"]: item for item in changed["behavior_contracts"]}
        acs = contracts["saml.acs"]["current"]
        acs["positive_signature_behavior"] = "trust any XML"
        acs["protocol_checks"] = "status only"
        acs["wire_inputs"] = "bounded POST"
        acs["claim_mapping"]["provider_reference"] = "issuer/nameid"

        failures = check_catalog(changed)
        self.assertIn("SAML signed XML positive behavior changed", failures)
        self.assertIn("SAML missing protocol-semantic checks are no longer explicit", failures)
        self.assertIn("SAML callback input-bound risk changed", failures)
        self.assertIn("SAML bare NameID mapping changed", failures)

    def test_checker_rejects_partial_session_and_business_side_effect_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        contracts = {item["id"]: item for item in changed["behavior_contracts"]}
        contracts["shared.auth_context_commit"]["current"]["flaw"] = "atomic"
        contracts["shared.identity_provisioning"]["current"]["missing_mapping"] = (
            "create an unrelated identity"
        )

        failures = check_catalog(changed)
        self.assertIn("pre-provisioning authenticated-session defect is no longer explicit", failures)
        self.assertIn("identity-provisioning business side effects changed", failures)

    def test_checker_rejects_security_disposition_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        dispositions = {item["id"]: item for item in changed["security_dispositions"]}
        dispositions["oidc.token_validation"]["migration"] = "preserve"
        dispositions["saml.protocol_semantics"]["requirement"] = "status only"
        dispositions["shared.config_secret_distribution"]["baseline"] = "references only"
        dispositions["shared.provider_reference"]["requirement"] = "rename immediately"

        failures = check_catalog(changed)
        self.assertIn("reviewed federated security migration disposition map changed", failures)
        self.assertIn("SAML protocol-semantic correction changed", failures)
        self.assertIn("raw provider-configuration distribution risk changed", failures)
        self.assertIn("provider-reference compatibility and migration contract changed", failures)

    def test_checker_rejects_consumer_contract_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        changed["consumer_contract"]["profile_consumers"] = []
        changed["consumer_contract"]["gateway"]["main_rpc_consumer"] = "none"

        failures = check_catalog(changed)
        self.assertIn(
            "reviewed UI, gateway, mapper, and profile consumer contract changed",
            failures,
        )
        self.assertIn("federated-auth downstream consumer evidence is incomplete", failures)

    def test_checker_rejects_source_fingerprint_and_provenance_mutations(self) -> None:
        changed = deepcopy(self.catalog)
        changed["source_files_sha256"]["auth_oidc/routes/login.py"] = "0" * 64
        fingerprint = changed["behavior_fingerprints"][
            "auth_saml/routes/login.py#Route.acs"
        ]
        fingerprint["ast_sha256"] = "0" * 64
        changed["provenance"]["auth_oidc_repo"]["pinned_head"] = "0" * 40

        failures = check_catalog(changed)
        self.assertIn("reviewed federated source bytes changed", failures)
        self.assertIn("reviewed federated behavior implementation changed", failures)
        self.assertIn("auth_oidc_repo pinned provenance changed", failures)

    def test_checker_rejects_inventory_rename_and_absolute_source_root(self) -> None:
        changed = deepcopy(self.catalog)
        source = "auth_saml/routes/login.py"
        changed["source_files_sha256"][source + ".renamed"] = changed[
            "source_files_sha256"
        ].pop(source)
        fingerprint = "auth_oidc/routes/login.py#Route.login"
        changed["behavior_fingerprints"][fingerprint + "_renamed"] = changed[
            "behavior_fingerprints"
        ].pop(fingerprint)
        changed["provenance"]["ui_repo"]["source_root"] = "/private/work/EliteaUI"

        failures = check_catalog(changed)
        self.assertIn("reviewed federated source file set changed", failures)
        self.assertIn("reviewed federated behavior fingerprint set changed", failures)
        self.assertIn("ui_repo logical source root changed", failures)

    def test_checker_rejects_unknown_nested_field_without_echoing_value(self) -> None:
        changed = deepcopy(self.catalog)
        sentinel = "SYNTHETIC_PRIVATE_KEY_MUST_NOT_APPEAR"
        changed["configuration_contract"]["unexpected"] = sentinel

        failures = check_catalog(changed)
        self.assertIn("reviewed federated auth snapshot changed", failures)
        self.assertNotIn(sentinel, " ".join(failures))

    def test_secret_bearing_config_selectors_export_only_presence_state(self) -> None:
        sentinel = "SYNTHETIC_SECRET_VALUE"
        endpoint = "https://sensitive.invalid/provider"
        oidc = exporter._safe_oidc_base(
            f"authorization_endpoint: {endpoint}\n"
            f"token_endpoint: {endpoint}/token\n"
            "client_id: example\n"
            f"client_secret: {sentinel}\n"
            "jwt_public_key: ${OIDC_KEY}\n"
        )
        oidc_runtime = exporter._safe_oidc_runtime(
            f"metadata_endpoint: {endpoint}/metadata\nclient_secret: {sentinel}\n"
        )
        saml = exporter._safe_saml_base(
            f"sp_key: {sentinel}\nsp_cert: {sentinel}\nidp_cert: {sentinel}\n"
            f"authn_destination: {endpoint}\nlogout_destination: {endpoint}/logout\n"
            "saml_issuer: example\n"
        )
        rendered = json.dumps([oidc, oidc_runtime, saml])

        self.assertNotIn(sentinel, rendered)
        self.assertNotIn(endpoint, rendered)
        self.assertEqual(
            oidc["fields"]["client_secret"], "inline_value_present_but_redacted"
        )
        self.assertEqual(
            oidc["fields"]["jwt_public_key"],
            "environment_template_present_but_redacted",
        )

    def test_yaml_parser_accepts_indentationless_sequences(self) -> None:
        values, sequences = exporter._yaml_paths(
            "initial_global_admins:\n- admin\n"
            "exposure:\n  handle:\n    prefixes:\n    - /auth\n"
        )
        self.assertEqual(values, {})
        self.assertEqual(sequences["initial_global_admins"], ["admin"])
        self.assertEqual(sequences["exposure.handle.prefixes"], ["/auth"])

    def test_main_public_rule_selector_pins_uri_without_yaml_dependency(self) -> None:
        selected = exporter._safe_main_auth(
            "auth_mode: rpc\n"
            "public_rules:\n"
            "  - uri: '/forward\\-auth/.*'\n"
            "  - uri: '/health'\n"
        )
        self.assertEqual(selected["auth_mode"], "rpc")
        self.assertEqual(
            selected["configured_public_uri_rules"],
            ["/forward\\-auth/.*", "/health"],
        )

    def test_runtime_selectors_ignore_unrelated_server_tuning(self) -> None:
        baseline = (
            "application:\n  APPLICATION_ROOT: /auth/\n"
            "server:\n  path: /auth/\n  kwargs:\n    spawn: 4\n"
        )
        tuned = baseline.replace("spawn: 4", "spawn: 512")
        routed = baseline.replace("path: /auth/", "path: /different/")
        self.assertEqual(
            exporter._safe_auth_pylon(baseline), exporter._safe_auth_pylon(tuned)
        )
        self.assertNotEqual(
            exporter._safe_auth_pylon(baseline), exporter._safe_auth_pylon(routed)
        )

    def test_route_extraction_and_framework_methods_are_explicit(self) -> None:
        tree = ast.parse(
            "from pylon.core.tools import web\n"
            "class Route:\n"
            "    @web.route('/callback', methods=['GET', 'POST'])\n"
            "    def callback(self):\n"
            "        return None\n"
        )
        node = exporter._method(tree, "Route", "callback")
        route, methods = exporter._route_spec(node)
        self.assertEqual((route, methods), ("/callback", ["GET", "POST"]))
        self.assertEqual(
            exporter._effective_methods(methods), ["GET", "HEAD", "OPTIONS", "POST"]
        )

    def test_ast_fingerprint_ignores_comments_but_detects_logic(self) -> None:
        first = ast.parse("class Route:\n    def login(self):\n        # one\n        return 1\n")
        second = ast.parse("class Route:\n    def login(self):\n        return 1  # two\n")
        changed = ast.parse("class Route:\n    def login(self):\n        return 2\n")

        def fingerprint(tree: ast.Module) -> str:
            return exporter._fingerprint(
                exporter._method(tree, "Route", "login"),
                "synthetic.py",
                "Route",
                "login",
            )["ast_sha256"]

        self.assertEqual(fingerprint(first), fingerprint(second))
        self.assertNotEqual(fingerprint(first), fingerprint(changed))

    def test_fixture_never_raw_hashes_secret_bearing_configs(self) -> None:
        inventory = self.catalog["source_inventory"]
        self.assertTrue(
            set(inventory["secret_bearing_config_paths"]).isdisjoint(
                inventory["full_byte_sources"]
            )
        )
        self.assertTrue(
            set(inventory["secret_bearing_config_paths"]).issubset(
                inventory["selected_config_sources"]
            )
        )


if __name__ == "__main__":
    unittest.main()
