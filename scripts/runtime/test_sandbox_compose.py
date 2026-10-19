#!/usr/bin/env python3
"""Render the default stack's sandbox packaging without starting containers or reading deployment secrets."""
import json
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[2]
command = ["docker", "compose", "-f", "deploy/docker-compose.standalone-full.yml",
           "config", "--no-consistency", "--format", "json"]
result = subprocess.run(command, cwd=ROOT, env={k: v for k, v in os.environ.items()
                                                 if not k.startswith(("ELITEA_", "STANDALONE_"))},
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
    assert mounts["/run/elitea-sandbox"]["read_only"]
    assert service["command"].count("--config") == 7
    assert "/var/run/docker.sock" in mounts
    assert all(not volume["bind"].get("create_host_path", False)
               for volume in mounts.values() if volume["type"] == "bind")
assert all(volume["target"] != "/var/run/docker.sock"
           for volume in services["elitea-worker"]["volumes"])
assert len(services["elitea-main"]["environment"]["ELITEA_RUNTIME_SANDBOX_AUDIENCES"].split(",")) == 7
print("Sandbox Compose render and isolation checks passed; no containers started.")
