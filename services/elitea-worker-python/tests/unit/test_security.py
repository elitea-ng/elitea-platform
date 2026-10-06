from __future__ import annotations

import datetime
import ssl
import subprocess
import sys
from pathlib import Path

import pytest
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

from elitea_worker.execution.errors import InvalidInput
from elitea_worker.transport.nats_jetstream import NatsTlsPaths, nats_client_context


def test_private_plane_ssl_context_survives_process_global_wrapper() -> None:
    script = """
import ssl

original = ssl.SSLContext

class InjectedSSLContext(original):
    pass

ssl.SSLContext = InjectedSSLContext

import elitea_worker

assert elitea_worker._PRIVATE_PLANE_SSL_CONTEXT is original
context = elitea_worker._PRIVATE_PLANE_SSL_CONTEXT(ssl.PROTOCOL_TLS_CLIENT)
elitea_worker._PRIVATE_PLANE_SSL_CONTEXT_BASE.minimum_version.__set__(
    context,
    ssl.TLSVersion.TLSv1_3,
)
elitea_worker._PRIVATE_PLANE_SSL_CONTEXT_BASE.verify_mode.__set__(
    context,
    ssl.CERT_REQUIRED,
)
assert context.minimum_version == ssl.TLSVersion.TLSv1_3
assert context.verify_mode == ssl.CERT_REQUIRED
"""

    completed = subprocess.run(
        [sys.executable, "-c", script],
        check=False,
        capture_output=True,
        text=True,
    )

    assert completed.returncode == 0, completed.stderr


def _write_identity(root: Path) -> NatsTlsPaths:
    """A private CA and an elitea-worker client certificate it signed."""

    now = datetime.datetime.now(datetime.timezone.utc)
    ca_key = ec.generate_private_key(ec.SECP256R1())
    ca_name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "test nats ca")])
    ca = (
        x509.CertificateBuilder()
        .subject_name(ca_name)
        .issuer_name(ca_name)
        .public_key(ca_key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=1))
        .not_valid_after(now + datetime.timedelta(days=1))
        .add_extension(x509.BasicConstraints(ca=True, path_length=None), critical=True)
        .sign(ca_key, hashes.SHA256())
    )
    key = ec.generate_private_key(ec.SECP256R1())
    leaf = (
        x509.CertificateBuilder()
        .subject_name(x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "elitea-worker")]))
        .issuer_name(ca_name)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=1))
        .not_valid_after(now + datetime.timedelta(days=1))
        .add_extension(
            x509.SubjectAlternativeName(
                [x509.UniformResourceIdentifier("spiffe://elitea.internal/nats/elitea-worker")]
            ),
            critical=False,
        )
        .sign(ca_key, hashes.SHA256())
    )
    paths = NatsTlsPaths(
        ca_path=root / "ca.crt",
        certificate_path=root / "tls.crt",
        private_key_path=root / "tls.key",
    )
    paths.ca_path.write_bytes(ca.public_bytes(serialization.Encoding.PEM))
    paths.certificate_path.write_bytes(leaf.public_bytes(serialization.Encoding.PEM))
    paths.private_key_path.write_bytes(
        key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.PKCS8,
            serialization.NoEncryption(),
        )
    )
    paths.private_key_path.chmod(0o600)
    return paths


def test_nats_client_context_is_tls13_exact_ca_with_the_worker_identity(
    tmp_path: Path,
) -> None:
    paths = _write_identity(tmp_path)

    context = nats_client_context(paths)

    assert context.minimum_version == ssl.TLSVersion.TLSv1_3
    assert context.verify_mode == ssl.CERT_REQUIRED
    assert context.check_hostname is True
    # Exactly the deployed CA: no system roots merged in.
    assert context.cert_store_stats()["x509_ca"] == 1


def test_nats_client_context_is_rebuilt_from_disk_each_time(tmp_path: Path) -> None:
    paths = _write_identity(tmp_path)
    first = nats_client_context(paths)
    _write_identity(tmp_path)  # rotated material at the same paths

    second = nats_client_context(paths)

    assert first is not second
    assert first.get_ca_certs() != second.get_ca_certs()


@pytest.mark.parametrize("broken", ["ca_path", "certificate_path", "private_key_path"])
def test_nats_client_context_refuses_missing_material(
    tmp_path: Path,
    broken: str,
) -> None:
    paths = _write_identity(tmp_path)
    getattr(paths, broken).unlink()

    with pytest.raises(InvalidInput):
        nats_client_context(paths)


def test_nats_context_passes_asyncio_start_tls_after_truststore_injection(
    tmp_path: Path,
) -> None:
    """The pinned SDK injects truststore, which replaces ``ssl.SSLContext``.

    asyncio's ``start_tls`` (what nats-py upgrades with) refuses a context
    that is not an instance of the CURRENT class. The worker's exact-CA
    context must pass that check while staying the standard library's.
    """

    paths = _write_identity(tmp_path)
    script = f"""
import ssl
from pathlib import Path

import truststore

original = ssl.SSLContext
truststore.inject_into_ssl()
assert ssl.SSLContext is not original

from elitea_worker.transport.nats_jetstream import NatsTlsPaths, nats_client_context

context = nats_client_context(NatsTlsPaths(
    ca_path=Path({str(paths.ca_path)!r}),
    certificate_path=Path({str(paths.certificate_path)!r}),
    private_key_path=Path({str(paths.private_key_path)!r}),
))
# asyncio.base_events.BaseEventLoop.start_tls performs exactly this check.
assert isinstance(context, ssl.SSLContext)
# ...and nothing truststore overrides is on the real type.
assert not any(
    klass.__module__.startswith("truststore") for klass in type(context).__mro__
)
assert context.cert_store_stats()["x509_ca"] == 1
"""

    completed = subprocess.run(
        [sys.executable, "-c", script],
        check=False,
        capture_output=True,
        text=True,
    )

    assert completed.returncode == 0, completed.stderr
