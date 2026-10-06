"""Strict, file-only production worker deployment configuration."""

from __future__ import annotations

import json
import os
import stat
from pathlib import Path
from typing import Any, Literal
from urllib.parse import urlsplit

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator

from elitea_worker.constants import (
    LIMITS_REVISION,
    MAX_GRPC_REQUEST_BYTES,
    MAX_GRPC_RESPONSE_BYTES,
    MAX_LEASE_POLL_INTERVAL_MILLIS,
)
from elitea_worker.execution.errors import InvalidInput
from elitea_worker.transport.nats_jetstream import NatsTlsPaths, validate_route


_MAX_CONFIG_BYTES = 64 * 1024
_MAX_IDENTITY_BYTES = 256
# docs/runtime-command-bus.md: the stream's MaxMsgSize (body plus headers)
# and the signed envelope body. limits.proto fields 5 and 4.
_V1_TRANSPORT_MESSAGE_BYTES = 64 * 1024
_V1_TRANSPORT_PAYLOAD_BYTES = 48 * 1024
_V1_INPUT_CONTENT_BYTES = 256 * 1024
_V1_OUTPUT_FRAME_BYTES = 64 * 1024


class RuntimeLimits(BaseModel):
    """All queue, body, deadline and shutdown bounds used by ``serve``."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    # The command bus (docs/runtime-command-bus.md, "Worker configuration").
    # Messages per pull: never more than the consumer's MaxRequestBatch (64)
    # nor than the free delivery permits at the moment of the pull.
    nats_fetch_batch: int = Field(ge=1, le=64)
    # How long one pull waits (the consumer's MaxRequestExpires is 30s).
    nats_fetch_expires_millis: int = Field(ge=100, le=30_000)
    # The +WPI period: at most a quarter of the 60s AckWait.
    nats_in_progress_interval_millis: int = Field(ge=1_000, le=15_000)
    # NakWithDelay for a command the claim said to retry later.
    nats_retry_delay_millis: int = Field(ge=1_000, le=300_000)
    dependency_retry_millis: int = Field(ge=100, le=60_000)
    delivery_max_concurrency: int = Field(gt=0, le=128)
    delivery_queue_capacity: int = Field(gt=0, le=512)
    sync_max_workers: int = Field(gt=0, le=128)
    sync_max_in_flight: int = Field(gt=0, le=512)
    admission_timeout_millis: int = Field(gt=0, le=60_000)
    grpc_deadline_millis: int = Field(gt=0, le=300_000)
    content_timeout_millis: int = Field(gt=0, le=300_000)
    http_max_connections: int = Field(gt=0, le=512)
    http_max_keepalive_connections: int = Field(ge=0, le=512)
    output_max_queued_frames: int = Field(gt=0, le=128)
    output_max_queued_bytes: int = Field(gt=0, le=64 * 1024 * 1024)
    output_max_sessions: int = Field(gt=0, le=8)
    output_ack_timeout_millis: int = Field(gt=0, le=300_000)
    output_stream_deadline_millis: int = Field(gt=0, le=3_600_000)
    lease_poll_interval_millis: int = Field(
        gt=0,
        le=MAX_LEASE_POLL_INTERVAL_MILLIS,
    )
    shutdown_timeout_millis: int = Field(gt=0, le=300_000)

    @model_validator(mode="after")
    def validate_related_bounds(self) -> RuntimeLimits:
        if self.delivery_queue_capacity < self.delivery_max_concurrency:
            raise ValueError("delivery queue must hold at least one item per worker")
        if self.sync_max_in_flight < self.sync_max_workers:
            raise ValueError("sync in-flight bound cannot be below the thread count")
        if _V1_OUTPUT_FRAME_BYTES > self.output_max_queued_bytes:
            raise ValueError("one output frame must fit inside the output queue")
        if self.http_max_keepalive_connections > self.http_max_connections:
            raise ValueError("HTTP keepalive connections cannot exceed all connections")
        return self

    # These fixed properties are protocol-revision facts, not deployment knobs.
    # Consumers remain explicit while operators cannot select an incompatible
    # local wire profile.
    @property
    def max_transport_message_bytes(self) -> int:
        return _V1_TRANSPORT_MESSAGE_BYTES

    @property
    def max_transport_payload_bytes(self) -> int:
        return _V1_TRANSPORT_PAYLOAD_BYTES

    @property
    def grpc_max_request_bytes(self) -> int:
        return MAX_GRPC_REQUEST_BYTES

    @property
    def grpc_max_response_bytes(self) -> int:
        return MAX_GRPC_RESPONSE_BYTES

    @property
    def content_max_body_bytes(self) -> int:
        return _V1_INPUT_CONTENT_BYTES

    @property
    def output_max_frame_bytes(self) -> int:
        return _V1_OUTPUT_FRAME_BYTES


class RuntimeDeployConfig(BaseModel):
    """Deployment-selected identities and paths; it never embeds credentials."""

    model_config = ConfigDict(extra="forbid", frozen=True)

    schema_version: Literal["elitea.runtime-deploy.v1"]
    limits_revision: str = Field(min_length=1, max_length=256)
    workload_session_id: str
    producer_id: str
    consumer_id: str
    # The command bus. consumer_id above is the NATS connection name
    # (observability only, never an identity: the client certificate is).
    nats_url: str = Field(min_length=1, max_length=2048)
    # The elitea-worker identity's mTLS material: all three or none, and
    # none only with nats:// (compose); tls:// requires all three.
    nats_ca_path: Path | None = None
    nats_certificate_path: Path | None = None
    nats_private_key_path: Path | None = None
    nats_stream: str = Field(min_length=1, max_length=64)
    nats_consumer: str = Field(min_length=1, max_length=64)
    control_target: str = Field(min_length=1, max_length=512)
    output_target: str = Field(min_length=1, max_length=512)
    content_origin: str = Field(min_length=1, max_length=2048)
    platform_origin: str = Field(min_length=1, max_length=2048)
    ca_path: Path
    certificate_path: Path
    private_key_path: Path
    ed25519_keyring_path: Path
    spool_root: Path
    spool_key_path: Path
    # Only worker pools admitting agent capabilities need the current
    # ``agentstate`` checkpoint database. Other capability pools remain valid
    # without receiving an unrelated database credential.
    agent_checkpoint_connection_path: Path | None = None
    limits: RuntimeLimits

    @field_validator("workload_session_id", "producer_id", "consumer_id")
    @classmethod
    def validate_identity(cls, value: str) -> str:
        if not _bounded_text(value, _MAX_IDENTITY_BYTES):
            raise ValueError("runtime identity is malformed")
        return value

    @field_validator("nats_url")
    @classmethod
    def validate_nats_url(cls, value: str) -> str:
        servers = value.split(",")
        schemes: set[str] = set()
        for server in servers:
            if not _canonical_nats_url(server):
                raise ValueError(
                    "NATS must be a canonical nats:// or tls://host:port list "
                    "without user information"
                )
            schemes.add(server.split("://", 1)[0])
        if len(schemes) != 1:
            raise ValueError("every NATS seed URL must use the same scheme")
        return value

    @field_validator("control_target", "output_target")
    @classmethod
    def validate_grpc_target(cls, value: str) -> str:
        if (
            not _bounded_text(value, 512)
            or "://" in value
            or "/" in value
            or "@" in value
            or value.startswith(":")
        ):
            raise ValueError("gRPC target is malformed")
        return value

    @field_validator("content_origin", "platform_origin")
    @classmethod
    def validate_content_origin(cls, value: str) -> str:
        parsed = urlsplit(value)
        if (
            parsed.scheme != "https"
            or not parsed.hostname
            or parsed.username is not None
            or parsed.password is not None
            or parsed.path not in ("", "/")
            or parsed.query
            or parsed.fragment
        ):
            raise ValueError("runtime service origins must be HTTPS origins")
        return value.rstrip("/")

    @field_validator(
        "ca_path",
        "certificate_path",
        "private_key_path",
        "ed25519_keyring_path",
        "spool_root",
        "spool_key_path",
    )
    @classmethod
    def validate_absolute_path(cls, value: Path) -> Path:
        if not value.is_absolute():
            raise ValueError("runtime material paths must be absolute")
        return value

    @field_validator(
        "agent_checkpoint_connection_path",
        "nats_ca_path",
        "nats_certificate_path",
        "nats_private_key_path",
    )
    @classmethod
    def validate_optional_absolute_path(cls, value: Path | None) -> Path | None:
        if value is not None and not value.is_absolute():
            raise ValueError("runtime material paths must be absolute")
        return value

    @model_validator(mode="after")
    def validate_revision(self) -> RuntimeDeployConfig:
        if self.limits_revision != LIMITS_REVISION:
            raise ValueError("runtime limits revision is not compatible")
        validate_route(self.nats_stream, self.nats_consumer)
        material = (
            self.nats_ca_path,
            self.nats_certificate_path,
            self.nats_private_key_path,
        )
        present = sum(path is not None for path in material)
        if present not in (0, 3):
            raise ValueError("NATS TLS material must be all three paths or none")
        uses_tls = self.nats_url.startswith("tls://")
        if uses_tls and present == 0:
            raise ValueError("tls:// requires the NATS client TLS material")
        if not uses_tls and present == 3:
            raise ValueError("NATS client TLS material requires tls:// URLs")
        return self

    @property
    def nats_tls(self) -> NatsTlsPaths | None:
        if self.nats_ca_path is None:
            return None
        assert self.nats_certificate_path is not None
        assert self.nats_private_key_path is not None
        return NatsTlsPaths(
            ca_path=self.nats_ca_path,
            certificate_path=self.nats_certificate_path,
            private_key_path=self.nats_private_key_path,
        )


def load_deploy_config(path: Path) -> RuntimeDeployConfig:
    """Load a bounded regular JSON file without following symlinks."""

    try:
        raw = read_regular_file(
            path,
            max_bytes=_MAX_CONFIG_BYTES,
            private=False,
            description="runtime deployment configuration",
        )
        value: Any = json.loads(raw)
        if not isinstance(value, dict):
            raise ValueError("deployment configuration must be an object")
        return RuntimeDeployConfig.model_validate(value)
    except InvalidInput:
        raise
    except Exception as exc:
        raise InvalidInput("The runtime deployment configuration is invalid.") from exc


def read_regular_file(
    path: Path,
    *,
    max_bytes: int,
    private: bool,
    description: str,
) -> bytes:
    """Read one bounded file and reject symlinks and unsafe permissions."""

    if max_bytes < 1 or not description:
        raise ValueError("file policy is invalid")
    try:
        absolute = path.absolute()
        if path.resolve(strict=True) != absolute:
            raise OSError("symlinked paths are not accepted")
        flags = os.O_RDONLY
        if hasattr(os, "O_CLOEXEC"):
            flags |= os.O_CLOEXEC
        if hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        descriptor = os.open(absolute, flags)
        try:
            info = os.fstat(descriptor)
            if not stat.S_ISREG(info.st_mode) or info.st_size < 1 or info.st_size > max_bytes:
                raise OSError("file type or size is invalid")
            permissions = stat.S_IMODE(info.st_mode)
            if permissions & 0o022:
                raise OSError("file is group/world writable")
            if private and permissions & 0o077:
                raise OSError("private file is group/world accessible")
            chunks = bytearray()
            while len(chunks) < info.st_size:
                chunk = os.read(descriptor, min(64 * 1024, info.st_size - len(chunks)))
                if not chunk:
                    break
                chunks.extend(chunk)
            if len(chunks) != info.st_size:
                raise OSError("file changed while being read")
            return bytes(chunks)
        finally:
            os.close(descriptor)
    except (OSError, RuntimeError, ValueError) as exc:
        raise InvalidInput(f"The {description} is unavailable or unsafe.") from exc


def validate_private_directory(path: Path, *, description: str) -> Path:
    """Validate an existing owner-private directory without changing its mode."""

    try:
        absolute = path.absolute()
        if path.resolve(strict=True) != absolute:
            raise OSError("symlinked paths are not accepted")
        info = absolute.lstat()
        if (
            not stat.S_ISDIR(info.st_mode)
            or stat.S_IMODE(info.st_mode) & 0o077
            or info.st_uid != os.geteuid()
        ):
            raise OSError("directory ownership or permissions are unsafe")
        return absolute
    except (OSError, RuntimeError, ValueError) as exc:
        raise InvalidInput(f"The {description} is unavailable or unsafe.") from exc


def _bounded_text(value: str, max_bytes: int) -> bool:
    return (
        bool(value)
        and len(value.encode("utf-8")) <= max_bytes
        and not any(character in value for character in ("\r", "\n", "\x00"))
    )


def _canonical_nats_url(value: str) -> bool:
    """``nats://host:port`` or ``tls://host:port``: no userinfo, path, query.

    User information is refused outright: nats-py would send it as a user or a
    token, and the only identity this worker presents is its certificate.
    """

    if not value or not all(
        0x21 <= ord(character) <= 0x7E and character not in ("%", "?", "#", "@")
        for character in value
    ):
        return False
    try:
        parsed = urlsplit(value)
        port = parsed.port
    except ValueError:
        return False
    return bool(
        parsed.scheme in ("nats", "tls")
        and parsed.hostname
        and port is not None
        and 0 < port < 65536
        and parsed.username is None
        and parsed.password is None
        and parsed.path == ""
        and not parsed.query
        and not parsed.fragment
        and _canonical_explicit_port(parsed.netloc, port)
    )


def _canonical_explicit_port(host_and_port: str, port: int) -> bool:
    if host_and_port.startswith("["):
        closing = host_and_port.find("]")
        return closing > 0 and host_and_port[closing + 1 :] == f":{port}"
    host, separator, port_text = host_and_port.rpartition(":")
    return bool(host and separator and port_text == str(port))
