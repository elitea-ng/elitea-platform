from __future__ import annotations

import base64
import hashlib
import json
from pathlib import Path

import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from elitea.runtime.v1 import common_pb2, envelope_pb2

from elitea_worker.execution.errors import AuthorizationFailure
from elitea_worker.protocol.codec import (
    Ed25519CommandAuthenticator,
    _ed25519_worker_command_signing_input,
)
from elitea_worker.security import load_ed25519_keyring


def test_python_verifier_supports_rotation_overlap_and_explicit_retirement(
    tmp_path: Path,
) -> None:
    old_private = Ed25519PrivateKey.generate()
    new_private = Ed25519PrivateKey.generate()
    old_signed = _signed("runtime-signing-old", old_private, b"worker-command")
    new_signed = _signed("runtime-signing-new", new_private, b"worker-command")

    overlap = Ed25519CommandAuthenticator(
        load_ed25519_keyring(
            _write_keyring(
                tmp_path / "overlap.json",
                {
                    "runtime-signing-old": old_private,
                    "runtime-signing-new": new_private,
                },
            )
        )
    )
    overlap.authenticate(old_signed)
    overlap.authenticate(new_signed)

    cutover = Ed25519CommandAuthenticator(
        load_ed25519_keyring(
            _write_keyring(
                tmp_path / "new-only.json",
                {"runtime-signing-new": new_private},
            )
        )
    )
    with pytest.raises(AuthorizationFailure):
        cutover.authenticate(old_signed)
    cutover.authenticate(new_signed)

    wrong_key = _signed("runtime-signing-new", old_private, b"worker-command")
    with pytest.raises(AuthorizationFailure):
        cutover.authenticate(wrong_key)


def test_python_verifier_authenticates_immutable_replay_but_rejects_cross_command_signature(
    tmp_path: Path,
) -> None:
    private = Ed25519PrivateKey.generate()
    authenticator = Ed25519CommandAuthenticator(
        load_ed25519_keyring(
            _write_keyring(tmp_path / "active.json", {"active": private})
        )
    )
    signed = _signed("active", private, b"worker-command")

    # Authentication is stateless, so an exact command-bus redelivery verifies
    # again. Durable command/claim/fence state owns replay safety.
    authenticator.authenticate(signed)
    authenticator.authenticate(signed)

    changed = envelope_pb2.SignedWorkerCommandEnvelopeV1()
    changed.CopyFrom(signed)
    changed.worker_command_bytes = b"different-command"
    changed.worker_command_digest.value = hashlib.sha256(
        changed.worker_command_bytes
    ).digest()
    with pytest.raises(AuthorizationFailure):
        authenticator.authenticate(changed)


def _write_keyring(
    path: Path,
    keys: dict[str, Ed25519PrivateKey],
) -> Path:
    path.write_text(
        json.dumps(
            {
                "schema_version": "elitea.runtime-ed25519-keyring.v1",
                "keys": [
                    {
                        "key_id": key_id,
                        "public_key_base64": base64.b64encode(
                            private.public_key().public_bytes(
                                serialization.Encoding.Raw,
                                serialization.PublicFormat.Raw,
                            )
                        ).decode("ascii"),
                    }
                    for key_id, private in keys.items()
                ],
            }
        ),
        encoding="utf-8",
    )
    path.chmod(0o644)
    return path


def _signed(
    key_id: str,
    private: Ed25519PrivateKey,
    command: bytes,
) -> envelope_pb2.SignedWorkerCommandEnvelopeV1:
    return envelope_pb2.SignedWorkerCommandEnvelopeV1(
        envelope_schema_revision="elitea.runtime.signed-worker-command.v1",
        signature_profile=envelope_pb2.SIGNATURE_PROFILE_V1_ED25519,
        key_id=key_id,
        worker_command_bytes=command,
        worker_command_digest=common_pb2.DigestV1(
            algorithm=common_pb2.DIGEST_ALGORITHM_V1_SHA256,
            value=hashlib.sha256(command).digest(),
        ),
        signature=private.sign(_ed25519_worker_command_signing_input(command)),
    )
