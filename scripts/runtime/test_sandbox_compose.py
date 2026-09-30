#!/usr/bin/env python3
"""Render sandbox packaging without starting containers or reading deployment secrets."""
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[2]
VALUES = {
    "ELITEA_SANDBOX_SUPERVISOR_IMAGE": "supervisor:test",
    "ELITEA_SANDBOX_DOCKER_GID": "999",
    "ELITEA_SANDBOX_DOCKER_SOCKET": "/var/run/docker.sock",
    "ELITEA_SANDBOX_DENO_MATERIAL": "/tmp/deno-material",
    "ELITEA_SANDBOX_RUST_MATERIAL": "/tmp/rust-material",
    "ELITEA_SANDBOX_WORKER_CONFIG": "/tmp/worker-runtime.json",
}
command = ["docker", "compose", "-f", "deploy/docker-compose.sandbox.yml",
           "config", "--no-consistency", "--format", "json"]
result = subprocess.run(command, cwd=ROOT, env={**os.environ, **VALUES},
                        capture_output=True, text=True, check=True)
services = json.loads(result.stdout)["services"]
assert "elitea-sandbox-deno" not in services and "elitea-sandbox-rust" not in services
for name in ("elitea-sandbox",):
    service = services[name]
    assert service["user"] == "10001:10001"
    assert service["read_only"] and not service.get("ports")
    assert service["cap_drop"] == ["ALL"]
    assert "no-new-privileges:true" in service["security_opt"]
    assert service["pids_limit"] == 256
    assert service["build"]["target"] == "supervisor"
    mounts = {volume["target"]: volume for volume in service["volumes"]}
    assert mounts["/run/elitea-sandbox/deno"]["read_only"]
    assert mounts["/run/elitea-sandbox/rust"]["read_only"]
    assert service["command"].count("--config") == 2
    assert "/var/run/docker.sock" in mounts
    assert all(not volume["bind"]["create_host_path"] for volume in mounts.values())
assert all(volume["target"] != "/var/run/docker.sock"
           for volume in services["elitea-worker"]["volumes"])
assert services["elitea-main"]["environment"]["ELITEA_RUNTIME_SANDBOX_AUDIENCES"] == (
    "dns:elitea-sandbox-deno,dns:elitea-sandbox-rust")
print("Sandbox Compose render and isolation checks passed; no containers started.")
