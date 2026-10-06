from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from elitea_worker.config import load_deploy_config, read_regular_file
from elitea_worker.constants import (
    CLAIM_LEASE_TTL_MILLIS,
    LIMITS_REVISION,
    COMMAND_BUS_ACK_WAIT_MILLIS,
    MAX_LEASE_POLL_INTERVAL_MILLIS,
)
from elitea_worker.execution.errors import InvalidInput


def _config(tmp_path: Path) -> dict[str, object]:
    return {
        "schema_version": "elitea.runtime-deploy.v1",
        "limits_revision": LIMITS_REVISION,
        "workload_session_id": "session-1",
        "producer_id": "producer-1",
        "consumer_id": "consumer-1",
        "nats_url": "tls://nats.elitea.svc:4222",
        "nats_ca_path": str(tmp_path / "nats-ca.crt"),
        "nats_certificate_path": str(tmp_path / "nats-tls.crt"),
        "nats_private_key_path": str(tmp_path / "nats-tls.key"),
        "nats_stream": "ELITEA_RT_V1_INDEX",
        "nats_consumer": "elitea-index-worker-v1",
        "control_target": "control.internal:8443",
        "output_target": "output.internal:8444",
        "content_origin": "https://content.internal",
        "platform_origin": "https://elitea.internal",
        "ca_path": str(tmp_path / "ca.pem"),
        "certificate_path": str(tmp_path / "worker.pem"),
        "private_key_path": str(tmp_path / "worker-key.pem"),
        "ed25519_keyring_path": str(tmp_path / "command-keys.json"),
        "spool_root": str(tmp_path / "spool"),
        "spool_key_path": str(tmp_path / "spool.key"),
        "agent_checkpoint_connection_path": str(tmp_path / "agent-checkpoint-connection"),
        "limits": {
            "nats_fetch_batch": 8,
            "nats_fetch_expires_millis": 1000,
            "nats_in_progress_interval_millis": 5000,
            "nats_retry_delay_millis": 60000,
            "dependency_retry_millis": 250,
            "delivery_max_concurrency": 4,
            "delivery_queue_capacity": 8,
            "sync_max_workers": 2,
            "sync_max_in_flight": 4,
            "admission_timeout_millis": 1000,
            "grpc_deadline_millis": 5000,
            "content_timeout_millis": 15000,
            "http_max_connections": 8,
            "http_max_keepalive_connections": 4,
            "output_max_queued_frames": 2,
            "output_max_queued_bytes": 131072,
            "output_max_sessions": 2,
            "output_ack_timeout_millis": 15000,
            "output_stream_deadline_millis": 300000,
            "lease_poll_interval_millis": 10000,
            "shutdown_timeout_millis": 30000,
        },
    }


def _write_config(path: Path, value: dict[str, object]) -> None:
    path.write_text(json.dumps(value), encoding="utf-8")
    path.chmod(0o600)


def test_deploy_config_is_strict_file_only_and_credential_free(tmp_path: Path) -> None:
    path = tmp_path / "runtime.json"
    value = _config(tmp_path)
    _write_config(path, value)

    loaded = load_deploy_config(path)

    assert loaded.nats_url == "tls://nats.elitea.svc:4222"
    assert loaded.nats_tls is not None
    assert loaded.nats_tls.ca_path == tmp_path / "nats-ca.crt"
    assert loaded.workload_session_id == "session-1"
    assert loaded.agent_checkpoint_connection_path == tmp_path / "agent-checkpoint-connection"
    assert loaded.limits.delivery_max_concurrency == 4
    assert loaded.limits.max_transport_message_bytes == 64 * 1024
    assert loaded.limits.max_transport_payload_bytes == 48 * 1024
    assert loaded.limits.grpc_max_request_bytes == 64 * 1024
    assert loaded.limits.grpc_max_response_bytes == 80 * 1024
    assert loaded.limits.content_max_body_bytes == 256 * 1024
    assert loaded.limits.output_max_frame_bytes == 64 * 1024
    assert not {
        "max_transport_message_bytes",
        "max_transport_payload_bytes",
        "grpc_max_request_bytes",
        "grpc_max_response_bytes",
        "content_max_body_bytes",
        "output_max_frame_bytes",
    }.intersection(loaded.limits.model_dump())

    value["nats_password"] = "must-never-be-inline"
    _write_config(path, value)
    with pytest.raises(InvalidInput):
        load_deploy_config(path)


def test_deploy_config_allows_non_agent_pool_without_checkpoint_database(
    tmp_path: Path,
) -> None:
    path = tmp_path / "runtime.json"
    value = _config(tmp_path)
    del value["agent_checkpoint_connection_path"]
    _write_config(path, value)

    loaded = load_deploy_config(path)

    assert loaded.agent_checkpoint_connection_path is None


@pytest.mark.parametrize(
    "nats_url",
    [
        "tls://nats.internal",
        "tls://nats.internal:04222",
        "tls://user:password@nats.internal:4222",
        "tls://elitea-worker@nats.internal:4222",
        "tls://token@nats.internal:4222",
        "tls://nats.internal:4222/",
        "tls://nats.internal:4222/path",
        "tls://nats.internal:4222?token=secret",
        "tls://nats.internal:4222#fragment",
        "tls://nats%2Dinternal:4222",
        "tls://nàts.internal:4222",
        " tls://nats.internal:4222",
        "rediss://nats.internal:4222",
        "wss://nats.internal:4222",
        "tls://nats.internal:4222,",
        "tls://a.internal:4222,nats://b.internal:4222",
        "tls://nats.internal:0",
        "tls://nats.internal:65536",
    ],
)
def test_deploy_config_requires_credential_free_nats_url(
    tmp_path: Path,
    nats_url: str,
) -> None:
    value = _config(tmp_path)
    value["nats_url"] = nats_url
    path = tmp_path / "runtime.json"
    _write_config(path, value)

    with pytest.raises(InvalidInput):
        load_deploy_config(path)


def test_deploy_config_accepts_a_tls_seed_list(tmp_path: Path) -> None:
    value = _config(tmp_path)
    value["nats_url"] = "tls://nats-0.nats:4222,tls://nats-1.nats:4222,tls://[::1]:4222"
    path = tmp_path / "runtime.json"
    _write_config(path, value)

    assert load_deploy_config(path).nats_url.count("tls://") == 3


def test_plaintext_nats_is_only_without_material_and_tls_only_with_it(
    tmp_path: Path,
) -> None:
    path = tmp_path / "runtime.json"
    plain = _config(tmp_path)
    plain["nats_url"] = "nats://nats.internal:4222"
    for key in ("nats_ca_path", "nats_certificate_path", "nats_private_key_path"):
        del plain[key]
    _write_config(path, plain)
    loaded = load_deploy_config(path)
    assert loaded.nats_tls is None

    # tls:// with no material, and nats:// with material, are both refused.
    without = dict(plain, nats_url="tls://nats.internal:4222")
    _write_config(path, without)
    with pytest.raises(InvalidInput):
        load_deploy_config(path)
    with_material = _config(tmp_path)
    with_material["nats_url"] = "nats://nats.internal:4222"
    _write_config(path, with_material)
    with pytest.raises(InvalidInput):
        load_deploy_config(path)


@pytest.mark.parametrize(
    "missing",
    ["nats_ca_path", "nats_certificate_path", "nats_private_key_path"],
)
def test_nats_tls_material_is_all_three_or_none(tmp_path: Path, missing: str) -> None:
    value = _config(tmp_path)
    del value[missing]
    path = tmp_path / "runtime.json"
    _write_config(path, value)

    with pytest.raises(InvalidInput):
        load_deploy_config(path)


def test_nats_tls_material_paths_must_be_absolute(tmp_path: Path) -> None:
    value = _config(tmp_path)
    value["nats_ca_path"] = "relative/ca.crt"
    path = tmp_path / "runtime.json"
    _write_config(path, value)

    with pytest.raises(InvalidInput):
        load_deploy_config(path)


@pytest.mark.parametrize(
    ("stream", "consumer"),
    [
        ("ELITEA_RT_V1_VALIDATE", "elitea-configuration-worker-v1"),
        ("ELITEA_RT_V1_AGENT", "elitea-agent-worker-v1"),
        ("ELITEA_RT_V1_INDEX", "elitea-index-worker-v1"),
    ],
)
def test_every_contract_stream_and_its_durable_are_accepted(
    tmp_path: Path,
    stream: str,
    consumer: str,
) -> None:
    value = _config(tmp_path)
    value["nats_stream"] = stream
    value["nats_consumer"] = consumer
    path = tmp_path / "runtime.json"
    _write_config(path, value)

    loaded = load_deploy_config(path)
    assert (loaded.nats_stream, loaded.nats_consumer) == (stream, consumer)


@pytest.mark.parametrize(
    ("stream", "consumer"),
    [
        ("ELITEA_RT_V1_INDEX", "elitea-agent-worker-v1"),
        ("ELITEA_RT_V1_AGENT", "elitea-index-worker-v1"),
        ("ELITEA_RT_V1_VALIDATE", "elitea-python-workers"),
        ("ELITEA_RT_V1_TOOLKIT", "elitea-toolkit-worker-v1"),
        ("elitea.runtime.commands.v1", "elitea-index-worker-v1"),
        ("ELITEA_RT_V1_INDEX", ""),
    ],
)
def test_any_other_stream_and_durable_pairing_is_refused(
    tmp_path: Path,
    stream: str,
    consumer: str,
) -> None:
    value = _config(tmp_path)
    value["nats_stream"] = stream
    value["nats_consumer"] = consumer
    path = tmp_path / "runtime.json"
    _write_config(path, value)

    with pytest.raises(InvalidInput):
        load_deploy_config(path)


@pytest.mark.parametrize(
    "legacy",
    ["redis_url", "redis_password_path", "redis_stream", "redis_group"],
)
def test_the_redis_fields_are_refused_not_ignored(tmp_path: Path, legacy: str) -> None:
    value = _config(tmp_path)
    value[legacy] = "anything"
    path = tmp_path / "runtime.json"
    _write_config(path, value)

    with pytest.raises(InvalidInput):
        load_deploy_config(path)


def test_lease_poll_interval_is_bounded_by_claim_authority(tmp_path: Path) -> None:
    assert CLAIM_LEASE_TTL_MILLIS == 30_000
    assert MAX_LEASE_POLL_INTERVAL_MILLIS == 10_000
    assert CLAIM_LEASE_TTL_MILLIS == 3 * MAX_LEASE_POLL_INTERVAL_MILLIS

    value = _config(tmp_path)
    value["limits"]["lease_poll_interval_millis"] = (  # type: ignore[index]
        MAX_LEASE_POLL_INTERVAL_MILLIS
    )
    path = tmp_path / "runtime.json"
    _write_config(path, value)
    assert load_deploy_config(path).limits.lease_poll_interval_millis == 10_000

    value["limits"]["lease_poll_interval_millis"] = (  # type: ignore[index]
        MAX_LEASE_POLL_INTERVAL_MILLIS + 1
    )
    _write_config(path, value)
    with pytest.raises(InvalidInput):
        load_deploy_config(path)


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("nats_fetch_batch", 0),
        ("nats_fetch_batch", 65),
        ("nats_fetch_expires_millis", 99),
        ("nats_fetch_expires_millis", 30_001),
        ("nats_in_progress_interval_millis", 999),
        ("nats_in_progress_interval_millis", 15_001),
        ("nats_retry_delay_millis", 999),
        ("nats_retry_delay_millis", 300_001),
        ("redis_read_batch", 8),
        ("redis_block_millis", 1000),
        ("redis_reclaim_idle_millis", 60_000),
        ("redis_reclaim_interval_millis", 5_000),
    ],
)
def test_command_bus_limits_follow_the_contract_bounds(
    tmp_path: Path,
    field: str,
    value: int,
) -> None:
    config = _config(tmp_path)
    config["limits"][field] = value  # type: ignore[index]
    path = tmp_path / "runtime.json"
    _write_config(path, config)

    with pytest.raises(InvalidInput):
        load_deploy_config(path)


def test_command_bus_limits_accept_exact_contract_boundaries(tmp_path: Path) -> None:
    # +WPI at most a quarter of AckWait, which is twice the claim lease.
    assert COMMAND_BUS_ACK_WAIT_MILLIS == 2 * CLAIM_LEASE_TTL_MILLIS
    assert 15_000 * 4 == COMMAND_BUS_ACK_WAIT_MILLIS
    path = tmp_path / "runtime.json"
    for field, bounds in (
        ("nats_fetch_batch", (1, 64)),
        ("nats_fetch_expires_millis", (100, 30_000)),
        ("nats_in_progress_interval_millis", (1_000, 15_000)),
        ("nats_retry_delay_millis", (1_000, 300_000)),
    ):
        for bound in bounds:
            config = _config(tmp_path)
            config["limits"][field] = bound  # type: ignore[index]
            _write_config(path, config)
            assert getattr(load_deploy_config(path).limits, field) == bound


def test_config_and_private_material_reject_symlink_and_open_permissions(
    tmp_path: Path,
) -> None:
    real = tmp_path / "real.json"
    _write_config(real, _config(tmp_path))
    linked = tmp_path / "linked.json"
    linked.symlink_to(real)
    with pytest.raises(InvalidInput, match="unsafe"):
        load_deploy_config(linked)

    private = tmp_path / "spool.key"
    private.write_bytes(os.urandom(32))
    private.chmod(0o640)
    with pytest.raises(InvalidInput, match="unsafe"):
        read_regular_file(
            private,
            max_bytes=32,
            private=True,
            description="output spool key",
        )


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("max_transport_message_bytes", 64 * 1024 - 1),
        ("max_transport_payload_bytes", 48 * 1024 - 1),
        ("grpc_max_request_bytes", 128 * 1024),
        ("grpc_max_response_bytes", 128 * 1024),
        ("content_max_body_bytes", 1024 * 1024),
        ("output_max_frame_bytes", 128 * 1024),
    ],
)
def test_fixed_protocol_limits_are_not_deployment_overrides(
    tmp_path: Path,
    field: str,
    value: int,
) -> None:
    config = _config(tmp_path)
    config["limits"][field] = value  # type: ignore[index]
    path = tmp_path / "runtime.json"
    _write_config(path, config)

    with pytest.raises(InvalidInput):
        load_deploy_config(path)
