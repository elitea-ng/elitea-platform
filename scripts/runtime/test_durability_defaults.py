#!/usr/bin/env python3
"""Guard: the default deployments run with durability and the Code runtime ON (PLAN §8.3).

Renders the DEFAULT standalone compose (deploy/docker-compose.standalone-full.yml, no
overlay, no environment) and the Code material deploy/scripts/code-material.py renders for
it, then runs the Helm guard (deploy/helm/tests/render-durability-defaults.sh), and fails
if any of these is off or missing:

  * agent node recovery and model-checkpoint recovery: not settings any more. The worker
    runtime JSON must not name them (the worker refuses them) and must name the
    checkpoint connection every recovery path needs;
  * Main's Code owner recovery (no switch: always on with Agent dispatch), workspace, platform and debug consumers;
  * the Code sandbox: seven supervisor profiles, every audience granted by Main, the
    four languages each with dependency preparation;
  * PostgreSQL TLS (supervisor ledger) and server keepalives 20/5/3 (FU-221), and the
    pooler's keepalives.

No container is started, no secret is read. Needs docker compose; the Helm part needs helm.
Run: python3 scripts/runtime/test_durability_defaults.py
"""
import importlib.util
import json
import os
import pathlib
import shutil
import subprocess
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
DEPLOY = ROOT / "deploy"
AUDIENCES = {
    "dns:elitea-sandbox-deno", "dns:elitea-sandbox-rust", "dns:elitea-sandbox-deno-native",
    "dns:elitea-sandbox-preparation", "dns:elitea-sandbox-javascript-preparation",
    "dns:elitea-sandbox-typescript-preparation", "dns:elitea-sandbox-rust-preparation",
}
PROFILES = ("deno", "rust", "denonative", "prep", "jsprep", "tsprep", "rsprep")
REMOVED_KEYS = ("agent_node_recovery", "agent_model_checkpoint_recovery")
DENO = "sha256:" + "d" * 64
RUST = "sha256:" + "e" * 64


def load_material_module():
    spec = importlib.util.spec_from_file_location("code_material", DEPLOY / "scripts/code-material.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def render_compose():
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("ELITEA_", "STANDALONE_", "COMPOSE_"))}
    result = subprocess.run(
        ["docker", "compose", "-p", "durability-guard", "-f", "docker-compose.standalone-full.yml",
         "config", "--format", "json"],
        cwd=DEPLOY, env=env, capture_output=True, text=True, check=False, timeout=120)
    if result.returncode != 0:
        raise AssertionError(f"default compose does not render: {result.stderr[-400:]}")
    return json.loads(result.stdout)


@unittest.skipUnless(shutil.which("docker"), "needs docker compose")
class DefaultComposeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rendered = render_compose()
        cls.services = cls.rendered["services"]

    def test_main_runs_every_code_consumer_and_grants_every_audience(self):
        env = self.services["elitea-main"]["environment"]
        # The Code consumers have no switch (PLAN §8.7): Main refuses the removed
        # variables and requires their settings with Agent dispatch.
        self.assertEqual(env.get("ELITEA_RUNTIME_AGENT_EXECUTION_DISPATCH_ENABLED"), "true")
        for name in ("OWNER_RECOVERY", "WORKSPACE", "PLATFORM", "DEBUG_ARTIFACTS"):
            self.assertNotIn(f"ELITEA_RUNTIME_CODE_{name}_ENABLED", env, name)
        for name in ("ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG", "ELITEA_RUNTIME_CODE_WORKSPACE_CONFIG",
                     "ELITEA_RUNTIME_CODE_PLATFORM_CONFIG", "ELITEA_CODE_DEBUG_AGENTSTATE_DSN_FILE"):
            self.assertTrue(env.get(name), name)
        self.assertEqual(set(env["ELITEA_RUNTIME_SANDBOX_AUDIENCES"].split(",")), AUDIENCES)
        owner = json.loads(env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"])
        self.assertEqual(owner["main_workload_identity"], "dns:elitea-code-owner-main")
        self.assertEqual({s["audience"] for s in owner["supervisors"]},
                         {"dns:elitea-sandbox-deno", "dns:elitea-sandbox-deno-native", "dns:elitea-sandbox-rust"})
        material = self.services["runtime-material"].get("environment") or {}
        self.assertFalse([k for k in material if k.startswith("ELITEA_RUNTIME_CODE_")])

    def test_supervisor_serves_seven_profiles_and_the_worker_waits_for_it(self):
        sandbox = self.services["elitea-sandbox"]
        configs = [sandbox["command"][i + 1] for i, arg in enumerate(sandbox["command"]) if arg == "--config"]
        self.assertEqual(configs, [f"/run/elitea-sandbox/{p}/config.json" for p in PROFILES])
        self.assertEqual(set(sandbox["networks"]["default"]["aliases"]),
                         {a.removeprefix("dns:") for a in AUDIENCES})
        self.assertEqual(sandbox["build"]["target"], "supervisor")
        self.assertEqual(sandbox["user"], "10001:10001")
        self.assertTrue(sandbox["read_only"])
        self.assertEqual(sandbox["cap_drop"], ["ALL"])
        self.assertIn("elitea-sandbox", self.services["elitea-worker"]["depends_on"])
        for runner in ("elitea-code-runner-deno", "elitea-code-runner-rust"):
            self.assertEqual(self.services[runner]["network_mode"], "none")
            self.assertIn(runner, sandbox["depends_on"])
        self.assertIn("code-preparation", self.rendered["networks"])
        self.assertNotIn("code-preparation", self.services["elitea-worker"]["networks"])
        self.assertNotIn("code-preparation", self.services["elitea-sandbox"]["networks"])

    def test_worker_reads_the_rendered_runtime(self):
        mounts = {v["target"]: v["source"] for v in self.services["elitea-worker"]["volumes"]}
        self.assertTrue(mounts["/run/elitea/runtime.json"].endswith("deploy/certs/code/worker-runtime.json"))

    def test_postgres_tls_and_keepalives(self):
        command = self.services["postgres"]["command"]
        settings = {command[i + 1] for i, arg in enumerate(command) if arg == "-c"}
        for expected in ("ssl=on", "tcp_keepalives_idle=20", "tcp_keepalives_interval=5", "tcp_keepalives_count=3"):
            self.assertIn(expected, settings)
        pooler = (DEPLOY / "runtime/pgbouncer.ini").read_text().splitlines()
        for expected in ("tcp_keepalive = 1", "tcp_keepidle = 20", "tcp_keepintvl = 5", "tcp_keepcnt = 3"):
            self.assertIn(expected, pooler)

    def test_no_overlay_is_needed(self):
        for retired in ("docker-compose.sandbox.yml", "docker-compose.code-consumers.yml",
                        "docker-compose.sandbox-preparation.yml"):
            self.assertFalse((DEPLOY / retired).exists(), retired)


class CodeMaterialTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.material = load_material_module()

    def test_base_worker_runtime_names_no_recovery_switch(self):
        for name in ("worker-runtime.rust.json", "worker-runtime.rust-validate.json"):
            config = json.loads((DEPLOY / "runtime" / name).read_text())
            for key in REMOVED_KEYS:
                self.assertNotIn(key, config, name)
            self.assertTrue(config.get("agent_checkpoint_connection_path"), name)

    def test_every_language_has_dependency_preparation(self):
        native = {"os": "linux", "arch": "amd64", "abi": "gnu"}
        entries = self.material.sandbox_runtimes({"deno": DENO, "rust": RUST}, native)
        self.assertEqual([e["language"] for e in entries], ["python", "javascript", "typescript", "rust"])
        for entry in entries:
            self.assertIn("preparation", entry, entry["language"])
            prep = entry["preparation"]
            self.assertEqual(entry["language"] == "python", "native_platform" not in prep, entry["language"])
            self.assertIn(prep["audience"], AUDIENCES)
            self.assertIn(entry["audience"], AUDIENCES)
            for key in REMOVED_KEYS:
                self.assertNotIn(key, entry)

    def test_profiles_match_the_worker_targets(self):
        native = {"os": "linux", "arch": "arm64", "abi": "gnu"}
        configs = {p: self.material.supervisor_config(p, "guard", {"deno": DENO, "rust": RUST}, native, "net")
                   for p in PROFILES}
        self.assertEqual({c["audience"] for c in configs.values()}, AUDIENCES)
        listeners = {c["audience"]: c["listen_address"].rsplit(":", 1)[1] for c in configs.values()}
        for entry in self.material.sandbox_runtimes({"deno": DENO, "rust": RUST}, native):
            for target in (entry, entry["preparation"]):
                self.assertEqual(listeners[target["audience"]], target["target"].rsplit(":", 1)[1])
                profile = next(c for c in configs.values() if c["audience"] == target["audience"])
                self.assertEqual(profile["image_digest"], target["image_digest"])
                self.assertEqual(profile["policy_revision"], target["policy_revision"])
                self.assertEqual(profile["timeout_seconds"], target["timeout_seconds"])
        for name, config in configs.items():
            if config["purpose"] == "preparation":
                self.assertEqual(config["preparation_network"], "net", name)
                self.assertNotIn("code_owner_requester", config, name)
            else:
                self.assertEqual(config["code_owner_requester"], "dns:elitea-code-owner-main", name)
                self.assertNotIn("preparation_network", config, name)
            if "native_workspace_bytes" in config:
                self.assertGreaterEqual(config["memory_bytes"], config["native_workspace_bytes"] + 128 * 1024 * 1024)


@unittest.skipUnless(shutil.which("helm"), "needs helm")
class HelmGuardTests(unittest.TestCase):
    def test_helm_defaults_guard(self):
        script = DEPLOY / "helm/tests/render-durability-defaults.sh"
        self.assertTrue(script.is_file(), "the Helm half of this guard is missing")
        result = subprocess.run(["bash", str(script)], cwd=ROOT, capture_output=True, text=True, check=False,
                                timeout=600)
        self.assertEqual(result.returncode, 0, result.stdout[-2000:] + result.stderr[-2000:])


if __name__ == "__main__":
    unittest.main()
