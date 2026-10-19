#!/usr/bin/env python3
"""Verify sandbox receipts using disposable PostgreSQL; never read deployment secrets."""
import os
import argparse
from pathlib import Path
import secrets
import subprocess
import time
import tempfile

parser = argparse.ArgumentParser()
parser.add_argument("--test-filter", choices=["sandbox_dispatch_journal", "sandbox_receipts_fence", "sandbox_supervisor_recovers", "sandbox_supervisor_submits", "sandbox_supervisor_mtls"], default="sandbox_receipts_fence")
parser.add_argument("--adapter-image", action="store_true", help="use the supplied prebuilt language-adapter image instead of creating a test-only adapter")
parser.add_argument("--language", choices=["python", "rust"], default="python")
args = parser.parse_args()
if args.language == "rust" and not args.adapter_image:
    parser.error("Rust verification requires --adapter-image")

if args.test_filter == "sandbox_supervisor_mtls" and not args.adapter_image:
    parser.error("mTLS verification requires --adapter-image")

def pinned_toolchain(path):
    """The exact `channel` of a rust-toolchain.toml; refuse anything unreadable."""
    import re
    match = re.search(r'^\s*channel\s*=\s*"(\d+\.\d+\.\d+)"\s*$', path.read_text(), re.MULTILINE)
    if not match:
        raise RuntimeError(f"{path} names no exact toolchain channel")
    return match.group(1)


root = Path(__file__).resolve().parents[2]
name = "elitea-sandbox-ledger-test-" + secrets.token_hex(5)
password = secrets.token_hex(24)
env = os.environ.copy()
env["ELITEA_TEST_ADAPTER_LANGUAGE"] = args.language
env["ELITEA_TEST_REAL_ADAPTER"] = "1" if args.adapter_image else "0"
env["POSTGRES_PASSWORD"] = password
tls_dir = tempfile.TemporaryDirectory(prefix="elitea-sandbox-tls-")
created = False
fixture_builder = None
fixture_image = None
try:
    if args.test_filter == "sandbox_supervisor_mtls":
        certs = Path(tls_dir.name)
        def openssl(*arguments):
            subprocess.run(["openssl", *arguments], cwd=certs, check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
        openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", "ca.key",
                "-out", "ca.pem", "-days", "1", "-subj", "/CN=Disposable sandbox CA",
                "-addext", "basicConstraints=critical,CA:TRUE")
        # `peer`, not `name`: `name` is the database container, removed in `finally`.
        for peer, dns, usage in [("server", "supervisor.test", "serverAuth"),
                                 ("worker", "worker.test", "clientAuth"),
                                 ("other", "other.test", "clientAuth")]:
            openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-keyout", peer+".key",
                    "-out", peer+".csr", "-subj", "/CN="+dns)
            (certs / (peer+".ext")).write_text(
                "subjectAltName=DNS:"+dns+"\nbasicConstraints=critical,CA:FALSE\n"
                "keyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage="+usage+"\n")
            openssl("x509", "-req", "-in", peer+".csr", "-CA", "ca.pem", "-CAkey", "ca.key",
                    "-CAcreateserial", "-out", peer+".pem", "-days", "1", "-extfile", peer+".ext")
        env["ELITEA_TEST_TLS_DIR"] = str(certs)
    if args.test_filter == "sandbox_supervisor_submits" and not args.adapter_image:
        base_image = env.get("ELITEA_CODE_RUNNER_TEST_IMAGE", "")
        if not base_image.startswith("sha256:"):
            raise RuntimeError("set ELITEA_CODE_RUNNER_TEST_IMAGE to the cached runner image ID")
        fixture_builder = name + "-adapter"
        subprocess.run(["docker", "create", "--pull=never", "--name", fixture_builder, base_image],
                       check=True, stdout=subprocess.DEVNULL, timeout=30)
        subprocess.run(["docker", "cp", str(root / "services/elitea-code-runner/tests/fixtures/code-execute.py"),
                        fixture_builder + ":/usr/local/bin/elitea-code-execute"], check=True, timeout=30)
        fixture_tag = name + ":adapter"
        subprocess.run(["docker", "commit", "--pause=false", fixture_builder, fixture_tag],
                       check=True, stdout=subprocess.DEVNULL, timeout=30)
        fixture_image = subprocess.check_output(
            ["docker", "image", "inspect", "--format", "{{.Id}}", fixture_tag],
            text=True, timeout=10).strip()
        if len(fixture_image) != 71 or not fixture_image.startswith("sha256:"):
            raise RuntimeError("Docker did not return a canonical fixture image ID")
        subprocess.run(["docker", "rm", fixture_builder], check=True, stdout=subprocess.DEVNULL, timeout=30)
        fixture_builder = None
        env["ELITEA_CODE_RUNNER_TEST_IMAGE"] = fixture_image
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
    # Build exactly what ci-rust.yml builds: --all-features, from the worker's
    # own directory, on the worker's own pinned toolchain. rustup picks the
    # compiler from the rust-toolchain.toml nearest the CURRENT directory, so
    # running cargo from the repository root with --manifest-path once used
    # the runner's default stable rustc and recompiled every dependency
    # (7m27s, a different test binary). The cwd fixes that; RUSTUP_TOOLCHAIN,
    # read from the worker's file, keeps it fixed even if the cwd moves or the
    # root toolchain file is removed.
    worker = root / "services/elitea-worker-rust"
    env["RUSTUP_TOOLCHAIN"] = pinned_toolchain(worker / "rust-toolchain.toml")
    result = subprocess.run(
        ["cargo", "test", "--locked", "--offline", "--all-features",
         "--lib", args.test_filter,
         "--", "--ignored", "--nocapture"],
        cwd=worker, env=env, timeout=600,
    )
    raise SystemExit(result.returncode)
finally:
    tls_dir.cleanup()
    if created:
        subprocess.run(
            ["docker", "rm", "-f", "-v", name],
            stdout=subprocess.DEVNULL, check=True, timeout=30,
        )

    if fixture_builder:
        subprocess.run(["docker", "rm", "-f", fixture_builder], stdout=subprocess.DEVNULL, timeout=30)
    if fixture_image:
        # No force: retain the image if a failed test left a container for inspection.
        subprocess.run(["docker", "image", "rm", fixture_image], stdout=subprocess.DEVNULL, timeout=30)
