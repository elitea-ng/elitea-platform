"""Real CLI subprocess lifecycle against an unreachable NATS server.

This verifies production config/trust bootstrap, retry and SIGTERM handling
with the real nats-py client and the real mTLS context. It does not claim to be
the Go/Python/PostgreSQL/NATS cross-process E2E test (tests/service holds the
live secured-NATS proof).
"""

from __future__ import annotations

import base64
import json
import os
import signal
import socket
import subprocess
import sys
import time
from datetime import UTC, datetime, timedelta
from pathlib import Path

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ed25519, rsa
from cryptography.x509.oid import NameOID

from elitea_worker.constants import LIMITS_REVISION
from elitea_worker.config import load_deploy_config
from elitea_worker.security import RuntimeTrustMaterial


_ROOT = Path(__file__).parents[4]


def test_serve_subprocess_retries_safely_then_drains_on_sigterm(tmp_path: Path) -> None:
    fake_packages = tmp_path / "fake-packages"
    fake_packages.mkdir()
    # This test owns the NATS retry and signal lifecycle. The immutable image
    # capability gate has dedicated unit and container tests; shadow it here so
    # host OS binaries/libraries cannot make this subprocess test non-hermetic.
    (fake_packages / "sitecustomize.py").write_text(
        """
import sys
import types

module = types.ModuleType("elitea_worker.indexing_runtime_capabilities")
module.require_indexing_runtime_capabilities = lambda: "test-profile"
sys.modules[module.__name__] = module

agent_module = types.ModuleType("elitea_worker.agent_current_runtime_capabilities")
agent_module.require_agent_current_runtime_capabilities = lambda: "test-profile"
sys.modules[agent_module.__name__] = agent_module
""".lstrip(),
        encoding="utf-8",
    )
    config_path = _write_runtime_material(tmp_path)
    trust = RuntimeTrustMaterial.load(load_deploy_config(config_path))
    # The private-plane context contains only the deployment CA, not the host
    # machine's system trust store.
    assert len(trust.http_client_context().get_ca_certs()) == 1
    python_path = os.pathsep.join(
        (
            str(fake_packages),
            str(_ROOT / "services/elitea-worker-python/src"),
            str(_ROOT / "libs/proto/gen/python"),
        )
    )
    environment = os.environ.copy()
    environment["PYTHONPATH"] = python_path
    process = subprocess.Popen(
        [sys.executable, "-m", "elitea_worker", "serve", "--config", str(config_path)],
        cwd=_ROOT,
        env=environment,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    startup_stderr: list[str] = []
    try:
        assert process.stderr is not None
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            line = process.stderr.readline()
            if line:
                startup_stderr.append(line)
                if '"event":"nats_connection_error"' in line:
                    break
            elif process.poll() is not None:
                break
        else:
            raise AssertionError("worker did not complete startup admission")
        assert process.poll() is None, "".join(startup_stderr)
        process.send_signal(signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=5)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)

    assert process.returncode == 0
    assert stdout == ""
    stderr = "".join(startup_stderr) + stderr
    assert '"event":"nats_connection_error"' in stderr
    assert '"safe_message":"A required runtime dependency is unavailable."' in stderr
    # The raw connection error (host, port, errno text) never reaches the log.
    assert "Connect call failed" not in stderr
    assert "Errno" not in stderr
    assert "Traceback" not in stderr


def _write_runtime_material(root: Path) -> Path:
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "worker.test")])
    now = datetime.now(UTC)
    certificate = (
        x509.CertificateBuilder()
        .subject_name(name)
        .issuer_name(name)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - timedelta(minutes=1))
        .not_valid_after(now + timedelta(days=1))
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .sign(key, hashes.SHA256())
    )
    certificate_pem = certificate.public_bytes(serialization.Encoding.PEM)
    private_key_pem = key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )
    ca_path = root / "ca.pem"
    certificate_path = root / "worker.pem"
    private_key_path = root / "worker-key.pem"
    ca_path.write_bytes(certificate_pem)
    certificate_path.write_bytes(certificate_pem)
    private_key_path.write_bytes(private_key_pem)
    private_key_path.chmod(0o600)

    signing_key = ed25519.Ed25519PrivateKey.generate()
    signing_public = signing_key.public_key().public_bytes(
        serialization.Encoding.Raw,
        serialization.PublicFormat.Raw,
    )
    keyring_path = root / "command-keys.json"
    keyring_path.write_text(
        json.dumps(
            {
                "schema_version": "elitea.runtime-ed25519-keyring.v1",
                "keys": [
                    {
                        "key_id": "runtime-signing-1",
                        "public_key_base64": base64.b64encode(signing_public).decode(),
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    spool_key_path = root / "spool.key"
    spool_key_path.write_bytes(os.urandom(32))
    spool_key_path.chmod(0o600)
    # A port nothing listens on: bind, read it, release it.
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
        probe.bind(("127.0.0.1", 0))
        closed_port = probe.getsockname()[1]
    spool_root = root / "spool"
    spool_root.mkdir(mode=0o700)

    config = {
        "schema_version": "elitea.runtime-deploy.v1",
        "limits_revision": LIMITS_REVISION,
        "workload_session_id": "session-1",
        "producer_id": "producer-1",
        "consumer_id": "consumer-1",
        "nats_url": f"tls://127.0.0.1:{closed_port}",
        "nats_ca_path": str(ca_path),
        "nats_certificate_path": str(certificate_path),
        "nats_private_key_path": str(private_key_path),
        "nats_stream": "ELITEA_RT_V1_VALIDATE",
        "nats_consumer": "elitea-configuration-worker-v1",
        "control_target": "control.invalid:8443",
        "output_target": "output.invalid:8444",
        "content_origin": "https://content.invalid",
        "platform_origin": "https://elitea.invalid",
        "ca_path": str(ca_path),
        "certificate_path": str(certificate_path),
        "private_key_path": str(private_key_path),
        "ed25519_keyring_path": str(keyring_path),
        "spool_root": str(spool_root),
        "spool_key_path": str(spool_key_path),
        "agent_checkpoint_connection_path": str(root / "agent-checkpoint-connection"),
        "limits": {
            "nats_fetch_batch": 1,
            "nats_fetch_expires_millis": 100,
            "nats_in_progress_interval_millis": 1000,
            "nats_retry_delay_millis": 1000,
            "dependency_retry_millis": 100,
            "delivery_max_concurrency": 1,
            "delivery_queue_capacity": 1,
            "sync_max_workers": 1,
            "sync_max_in_flight": 1,
            "admission_timeout_millis": 100,
            "grpc_deadline_millis": 100,
            "content_timeout_millis": 100,
            "http_max_connections": 1,
            "http_max_keepalive_connections": 1,
            "output_max_queued_frames": 1,
            "output_max_queued_bytes": 65536,
            "output_max_sessions": 1,
            "output_ack_timeout_millis": 100,
            "output_stream_deadline_millis": 100,
            "lease_poll_interval_millis": 100,
            "shutdown_timeout_millis": 1000,
        },
    }
    config_path = root / "runtime.json"
    config_path.write_text(json.dumps(config), encoding="utf-8")
    config_path.chmod(0o600)
    return config_path
