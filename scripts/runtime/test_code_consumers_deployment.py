#!/usr/bin/env python3
"""Check Code consumer packaging without services, secrets, or runtime admission."""
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
PREFIXES = ("ELITEA_RUNTIME_CODE_OWNER_RECOVERY_", "ELITEA_RUNTIME_CODE_WORKSPACE_",
            "ELITEA_RUNTIME_CODE_PLATFORM_", "ELITEA_RUNTIME_CODE_DEBUG_ARTIFACTS_")
DEBUG_DSN = "ELITEA_CODE_DEBUG_AGENTSTATE_DSN_FILE"
POLICY = {"revision": 1, "max_files": 1024, "max_file_bytes": 1048576,
          "max_total_bytes": 16777216, "max_manifest_bytes": 524288,
          "max_path_bytes": 255, "max_depth": 16, "max_projections": 64,
          "max_acquisition_seconds": 300}
BROKER = {"revision": 1, "max_calls": 32, "max_total_bytes": 1048576}
CODE = {"sandboxAudiences": ["dns:elitea-sandbox-deno", "dns:elitea-sandbox-rust"],
        "codeOwnerRecovery": {"enabled": True, "mainWorkloadIdentity": "dns:elitea-main",
                              "supervisors": [
                                  {"audience": "dns:elitea-sandbox-deno", "httpsOrigin": "https://elitea-sandbox-deno:9446"},
                                  {"audience": "dns:elitea-sandbox-rust", "httpsOrigin": "https://elitea-sandbox-rust:9447"}]},
        "codeWorkspace": {"enabled": True, "repositoryCapabilities": ["github"],
                          "egressAllowlist": ["api.github.com:443"], "policy": POLICY},
        "codePlatform": {"enabled": True, "brokerPolicies": [BROKER]},
        "codeDebugArtifacts": {"enabled": True}}


def render(extra):
    with tempfile.TemporaryDirectory(prefix="elitea-code-chart-") as work:
        override = Path(work) / "values.yaml"
        override.write_text(yaml.safe_dump({"postgresql": {"maxConnections": 250}, **extra}))
        return subprocess.run(
            ["helm", "template", "code", "deploy/helm/elitea", "-f",
             "deploy/helm/elitea/values-standalone.yaml", "-f", str(override),
             "--set", "llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1",
             "--set", "llmGateway.egressPosture=public-unrestricted"],
            cwd=ROOT, capture_output=True, text=True, timeout=30, check=False)


def code_environment(output):
    candidates = [doc["data"] for doc in yaml.safe_load_all(output)
                  if doc and doc.get("kind") == "ConfigMap"
                  and "ELITEA_RUNTIME_ENABLED" in doc.get("data", {})]
    if len(candidates) != 1:
        raise AssertionError("Expected exactly one Main runtime configuration")
    return {name: value for name, value in candidates[0].items()
            if name.startswith(PREFIXES) or name == DEBUG_DSN}


class PackagingTests(unittest.TestCase):
    def assert_private_material_mounts(self, authored, normalized):
        targets = {"/src/code-owner-client.crt", "/src/code-owner-client.key",
                   "/src/code-platform-content-keys.json"}
        for mounts in (authored, normalized):
            self.assertEqual(len(mounts), len(targets))
            self.assertEqual({mount["target"] for mount in mounts}, targets)
            for mount in mounts:
                self.assertEqual(mount["type"], "bind")
                self.assertIs(mount["read_only"], True)
        for mount in authored:
            self.assertIs(mount.get("bind", {}).get("create_host_path"), False,
                          "Private material must explicitly disable host-path creation")
        for mount in normalized:
            # Compose versions can omit false-valued fields from normalized JSON.
            # The authored safeguard above must remain explicit.
            self.assertIs(mount.get("bind", {}).get("create_host_path", False), False)

    def test_private_material_mount_normalization_preserves_host_path_guard(self):
        overlay = yaml.safe_load((ROOT / "deploy/docker-compose.code-consumers.yml").read_text())
        authored = overlay["services"]["runtime-material"]["volumes"]
        self.assert_private_material_mounts(authored, authored)
        for omitted in ("create_host_path", "bind"):
            normalized = copy.deepcopy(authored)
            for mount in normalized:
                if omitted == "bind":
                    mount.pop("bind")
                else:
                    mount["bind"].pop("create_host_path")
            with self.subTest(normalized_omits=omitted):
                self.assert_private_material_mounts(authored, normalized)
        for value in (None, True):
            unsafe = copy.deepcopy(authored)
            if value is None:
                unsafe[0]["bind"].pop("create_host_path")
            else:
                unsafe[0]["bind"]["create_host_path"] = value
            with self.subTest(authored_create_host_path=value), self.assertRaises(AssertionError):
                self.assert_private_material_mounts(unsafe, authored)
        unsafe = copy.deepcopy(authored)
        unsafe[0]["bind"]["create_host_path"] = True
        with self.assertRaises(AssertionError):
            self.assert_private_material_mounts(authored, unsafe)

    def test_default_chart_does_not_enable_code_consumers(self):
        result = render({})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(code_environment(result.stdout), {})

    def test_enabled_chart_uses_typed_policy_and_one_material_directory(self):
        result = render({"main": {"runtime": CODE}})
        self.assertEqual(result.returncode, 0, result.stderr)
        env = code_environment(result.stdout)
        self.assertEqual(len(env), 8)
        owner = json.loads(env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"])
        self.assertEqual(owner, {"main_workload_identity": "dns:elitea-main",
                                "certificate_chain_path": "/run/elitea-runtime/code-owner-client.crt",
                                "private_key_path": "/run/elitea-runtime/code-owner-client.key",
                                "server_ca_path": "/run/elitea-runtime/runtime-ca.crt",
                                "supervisors": [{"audience": item["audience"], "https_origin": item["httpsOrigin"]}
                                                for item in CODE["codeOwnerRecovery"]["supervisors"]]})
        self.assertEqual(json.loads(env["ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG"]),
                         {"revision": 1, "repository_capabilities": ["github"],
                          "egress_allowlist": ["api.github.com:443"], "policy": POLICY})
        self.assertEqual(json.loads(env["ELITEA_RUNTIME_CODE_PLATFORM_CONFIG"]),
                         {"revision": 1, "content_keys_file": "/run/elitea-runtime/code-platform-content-keys.json",
                          "broker_policies": [BROKER]})
        self.assertEqual(env[DEBUG_DSN], "/run/elitea-runtime/agent-checkpoint-connection")
        for name, value in env.items():
            if name.endswith("_ENABLED"):
                self.assertEqual(value, "true")

    def test_invalid_owner_or_consumer_contract_fails_rendering(self):
        cases = []
        def altered(path, value):
            config = copy.deepcopy(CODE)
            target = config
            for name in path[:-1]:
                target = target[name]
            target[path[-1]] = value
            cases.append({"main": {"runtime": config}})
        altered(["enabled"], False)
        altered(["codeOwnerRecovery", "enabled"], False)
        altered(["codeOwnerRecovery", "enabled"], "true")
        altered(["codeOwnerRecovery", "supervisors", 0, "httpsOrigin"], "http://elitea-sandbox-deno:9446")
        altered(["codeOwnerRecovery", "supervisors", 0, "audience"], "dns:unselected")
        altered(["codeWorkspace", "enabled"], False)
        altered(["codeWorkspace", "repositoryCapabilities"], ["github", "github"])
        altered(["codeWorkspace", "egressAllowlist"], ["*.github.com:443"])
        altered(["codeWorkspace", "egressAllowlist"], ["api.github.com:443", "api.github.com:443"])
        altered(["codeWorkspace", "egressAllowlist"], ["api.github.com:65536"])
        altered(["codeWorkspace", "unexpected"], True)
        altered(["codeWorkspace", "policy", "max_files"], 4097)
        altered(["codeWorkspace", "policy", "max_file_bytes"], "1048576")
        altered(["codeWorkspace", "policy", "max_file_bytes"], 1048576.5)
        altered(["codeWorkspace", "policy", "max_files"], True)
        altered(["codeWorkspace", "policy", "max_files"], 0)
        altered(["codeWorkspace", "policy", "revision"], 2)
        altered(["codeWorkspace", "policy", "unexpected"], 1)
        altered(["codePlatform", "enabled"], False)
        altered(["codePlatform", "brokerPolicies"], [BROKER, BROKER])
        altered(["codePlatform", "brokerPolicies", 0, "max_calls"], 4097)
        altered(["codePlatform", "brokerPolicies", 0, "max_total_bytes"], "1048576")
        cases.append({"main": {"env": {"ELITEA_RUNTIME_CODE_WORKSPACE_ENABLED": "true"}}})
        for index, config in enumerate(cases):
            with self.subTest(case=index):
                result = render(config)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("code", result.stderr.lower())

    def test_debug_receipt_pool_has_one_explicit_four_connection_budget(self):
        # Standalone uses four Main replicas, each with 42 product/runtime connections.
        # Debug adds four independent receipt connections per replica.
        for budget, allowed in ((208, False), (209, True)):
            with self.subTest(budget=budget):
                result = render({"main": {"runtime": CODE},
                                 "postgresql": {"maxConnections": budget}})
                if allowed:
                    self.assertEqual(result.returncode, 0, result.stderr)
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("184 PostgreSQL connections", result.stderr)

    def test_debug_and_compiled_receipts_share_one_budget(self):
        runtime = copy.deepcopy(CODE)
        runtime["rustCompiledSnapshots"] = {
            "enabled": True, "profilesSha256": "a" * 64,
            "globalEntries": 100, "globalBytes": 1073741824,
            "tenantEntries": 10, "tenantBytes": 268435456,
            "publishingTtlSeconds": 60, "readyTtlSeconds": 3600}
        result = render({"main": {"runtime": runtime},
                         "postgresql": {"maxConnections": 209},
                         "worker": {"enabled": False},
                         "sandboxKubernetes": {"supervisor": {"enabled": False}}})
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_compose_contract_matches_chart_and_keeps_material_in_main(self):
        rendered = render({"main": {"runtime": CODE}})
        self.assertEqual(rendered.returncode, 0, rendered.stderr)
        expected = code_environment(rendered.stdout)
        values = {"ELITEA_CODE_OWNER_RECOVERY_CONFIG": expected["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"],
                  "ELITEA_CODE_WORKSPACE_CONFIG": expected["ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG"],
                  "ELITEA_CODE_PLATFORM_CONFIG": expected["ELITEA_RUNTIME_CODE_PLATFORM_CONFIG"],
                  "ELITEA_CODE_OWNER_CLIENT_CERT_FILE": "/tmp/code-owner-client.crt",
                  "ELITEA_CODE_OWNER_CLIENT_KEY_FILE": "/tmp/code-owner-client.key",
                  "ELITEA_CODE_PLATFORM_CONTENT_KEYS_FILE": "/tmp/code-platform-content-keys.json"}
        result = subprocess.run(
            ["docker", "compose", "--env-file", "/dev/null", "-f", "deploy/docker-compose.code-consumers.yml",
             "config", "--no-consistency", "--format", "json"],
            cwd=ROOT, env={"PATH": os.environ["PATH"], "COMPOSE_DISABLE_ENV_FILE": "1", **values},
            capture_output=True, text=True, timeout=30, check=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        services = json.loads(result.stdout)["services"]
        self.assertEqual(set(services), {"runtime-material", "elitea-main"})
        self.assertEqual(services["elitea-main"]["environment"], expected)
        material = services["runtime-material"]
        overlay = yaml.safe_load((ROOT / "deploy/docker-compose.code-consumers.yml").read_text())
        authored = overlay["services"]["runtime-material"]["volumes"]
        self.assert_private_material_mounts(authored, material["volumes"])
        self.assertNotIn("ELITEA_RUNTIME_CODE_WORKSPACE_ENABLED", material["environment"])


if __name__ == "__main__":
    unittest.main()
