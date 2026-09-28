#!/usr/bin/env python3
"""Verify sandbox receipts using disposable PostgreSQL; never read deployment secrets."""
import os
from pathlib import Path
import secrets
import subprocess
import time

root = Path(__file__).resolve().parents[2]
name = "elitea-sandbox-ledger-test-" + secrets.token_hex(5)
password = secrets.token_hex(24)
env = os.environ.copy()
env["POSTGRES_PASSWORD"] = password
created = False
try:
    subprocess.run(
        ["docker", "run", "-d", "--pull=never", "--name", name,
         "--memory=512m", "--cpus=1", "-p", "127.0.0.1::5432",
         "-e", "POSTGRES_PASSWORD", "postgres:18"],
        env=env, check=True, stdout=subprocess.DEVNULL, timeout=30,
    )
    created = True
    for attempt in range(30):
        ready = subprocess.run(
            ["docker", "exec", name, "pg_isready", "-U", "postgres"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5,
        )
        if ready.returncode == 0:
            break
        time.sleep(1)
    else:
        raise RuntimeError("temporary test database did not become ready")
    endpoint = subprocess.check_output(
        ["docker", "port", name, "5432/tcp"], text=True, timeout=5,
    ).strip()
    env["ELITEA_TEST_DATABASE_URL"] = (
        f"postgresql://postgres:{password}@{endpoint}/postgres"
    )
    result = subprocess.run(
        ["cargo", "test", "--locked", "--offline", "--manifest-path",
         "services/elitea-worker-rust/Cargo.toml", "--features",
         "sandbox-supervisor", "--lib", "sandbox_receipts_fence", "-j", "2",
         "--", "--ignored", "--nocapture"],
        cwd=root, env=env, timeout=600,
    )
    raise SystemExit(result.returncode)
finally:
    if created:
        subprocess.run(
            ["docker", "rm", "-f", "-v", name],
            stdout=subprocess.DEVNULL, check=True, timeout=30,
        )
