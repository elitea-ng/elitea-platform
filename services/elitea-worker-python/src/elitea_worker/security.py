"""File-backed production trust material and exact signing-key resolution."""

from __future__ import annotations

import base64
import binascii
import json
import ssl
from dataclasses import dataclass
from pathlib import Path
from typing import Literal

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
from pydantic import BaseModel, ConfigDict, Field, model_validator

from elitea_worker import (
    _PRIVATE_PLANE_SSL_CONTEXT,
    _PRIVATE_PLANE_SSL_CONTEXT_BASE,
)
from elitea_worker.config import RuntimeDeployConfig, read_regular_file
from elitea_worker.execution.errors import InvalidInput


class _PublicKeyEntry(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)

    key_id: str = Field(min_length=1, max_length=256)
    public_key_base64: str = Field(min_length=1, max_length=128)


class _PublicKeyring(BaseModel):
    model_config = ConfigDict(extra="forbid", frozen=True)

    schema_version: Literal["elitea.runtime-ed25519-keyring.v1"]
    keys: tuple[_PublicKeyEntry, ...] = Field(min_length=1, max_length=64)

    @model_validator(mode="after")
    def validate_unique_safe_ids(self) -> _PublicKeyring:
        key_ids = [item.key_id for item in self.keys]
        if len(set(key_ids)) != len(key_ids):
            raise ValueError("duplicate signing key ID")
        if any(
            any(character in key_id for character in ("\r", "\n", "\x00"))
            for key_id in key_ids
        ):
            raise ValueError("signing key ID is malformed")
        return self


class ExactEd25519PublicKeyResolver:
    """Immutable keyring with no default, prefix or fallback lookup."""

    def __init__(self, keys: dict[str, Ed25519PublicKey]) -> None:
        if not keys or any(not key_id for key_id in keys):
            raise ValueError("at least one exact Ed25519 key is required")
        self._keys = dict(keys)

    def resolve_ed25519_public_key(self, key_id: str) -> Ed25519PublicKey:
        return self._keys[key_id]


@dataclass(frozen=True, slots=True)
class RuntimeTrustMaterial:
    ca_bytes: bytes
    certificate_bytes: bytes
    private_key_bytes: bytes
    spool_master_key: bytes
    signing_keys: ExactEd25519PublicKeyResolver
    ca_path: Path
    certificate_path: Path
    private_key_path: Path

    @classmethod
    def load(cls, config: RuntimeDeployConfig) -> RuntimeTrustMaterial:
        ca = read_regular_file(
            config.ca_path,
            max_bytes=1024 * 1024,
            private=False,
            description="runtime CA bundle",
        )
        certificate = read_regular_file(
            config.certificate_path,
            max_bytes=256 * 1024,
            private=False,
            description="workload certificate chain",
        )
        private_key = read_regular_file(
            config.private_key_path,
            max_bytes=128 * 1024,
            private=True,
            description="workload private key",
        )
        spool_key = read_regular_file(
            config.spool_key_path,
            max_bytes=32,
            private=True,
            description="output spool key",
        )
        if len(spool_key) != 32:
            raise InvalidInput("The output spool key is unavailable or unsafe.")
        resolver = load_ed25519_keyring(config.ed25519_keyring_path)
        # Parsing the chain now proves the configured certificate and key match.
        context = _base_client_context(config.ca_path)
        try:
            context.load_cert_chain(
                certfile=str(config.certificate_path),
                keyfile=str(config.private_key_path),
            )
        except (OSError, ssl.SSLError) as exc:
            raise InvalidInput("The workload TLS identity is invalid.") from exc
        return cls(
            ca_bytes=ca,
            certificate_bytes=certificate,
            private_key_bytes=private_key,
            spool_master_key=spool_key,
            signing_keys=resolver,
            ca_path=config.ca_path,
            certificate_path=config.certificate_path,
            private_key_path=config.private_key_path,
        )

    def http_client_context(self) -> ssl.SSLContext:
        context = _base_client_context(self.ca_path)
        try:
            context.load_cert_chain(
                certfile=str(self.certificate_path),
                keyfile=str(self.private_key_path),
            )
        except (OSError, ssl.SSLError) as exc:
            raise InvalidInput("The workload TLS identity is invalid.") from exc
        return context


def _base_client_context(ca_path: Path) -> ssl.SSLContext:
    try:
        # Do not merge machine/container system roots into this private-plane
        # trust domain. Only the explicitly deployed CA may authenticate it.
        context = _PRIVATE_PLANE_SSL_CONTEXT(ssl.PROTOCOL_TLS_CLIENT)
        context.load_verify_locations(cafile=str(ca_path))
    except (OSError, ssl.SSLError) as exc:
        raise InvalidInput("The runtime CA bundle is invalid.") from exc
    # truststore replaces ``ssl.SSLContext`` process-wide when the SDK is
    # imported. CPython's original property setters resolve that replaced
    # module global and recurse when invoked on the captured standard-library
    # class. Configure the underlying C descriptors directly so private-plane
    # trust remains exact-CA without mutating process-global SDK behavior.
    _PRIVATE_PLANE_SSL_CONTEXT_BASE.minimum_version.__set__(
        context,
        ssl.TLSVersion.TLSv1_3,
    )
    context.check_hostname = True
    _PRIVATE_PLANE_SSL_CONTEXT_BASE.verify_mode.__set__(
        context,
        ssl.CERT_REQUIRED,
    )
    return context


def _parse_keyring(raw: bytes) -> ExactEd25519PublicKeyResolver:
    try:
        value = json.loads(raw)
        keyring = _PublicKeyring.model_validate(value)
        keys: dict[str, Ed25519PublicKey] = {}
        for item in keyring.keys:
            encoded = item.public_key_base64
            if any(character.isspace() for character in encoded):
                raise ValueError("public key encoding contains whitespace")
            key_bytes = base64.b64decode(encoded, validate=True)
            if len(key_bytes) != 32:
                raise ValueError("Ed25519 public key must contain 32 bytes")
            keys[item.key_id] = Ed25519PublicKey.from_public_bytes(key_bytes)
        return ExactEd25519PublicKeyResolver(keys)
    except (ValueError, TypeError, json.JSONDecodeError, binascii.Error) as exc:
        raise InvalidInput("The Ed25519 verification keyring is invalid.") from exc


def load_ed25519_keyring(path: Path) -> ExactEd25519PublicKeyResolver:
    raw = read_regular_file(
        path,
        max_bytes=64 * 1024,
        private=False,
        description="Ed25519 verification keyring",
    )
    return _parse_keyring(raw)
