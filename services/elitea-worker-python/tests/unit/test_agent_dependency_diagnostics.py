from __future__ import annotations

import asyncio
import json
from types import SimpleNamespace

import pytest

from elitea.runtime.v1 import command_pb2
from elitea_worker.agents.checkpoint import CurrentAgentCheckpointFactory
from elitea_worker.execution import delivery
from elitea_worker.execution.errors import DependencyUnavailable, InvalidInput


_EXECUTION_ID = "5db9b97d38ca4bfe047236ac9f253234"
_PRIVATE_VALUE = "private-token-and-prompt-must-not-appear"


def _processor():
    processor = object.__new__(delivery.AgentExecutionDeliveryProcessor)
    processor._input_request_builder = SimpleNamespace(build=lambda reference: reference)
    return processor


def _receipt():
    return SimpleNamespace(
        identity=SimpleNamespace(
            execution_id=_EXECUTION_ID, resource_project_id="90107", generation=1,
        ),
        fence=SimpleNamespace(fence_token=b"f" * 32),
        input_bundle=SimpleNamespace(input_bundle_id="bundle"),
        input_bundle_ref=SimpleNamespace(digest=SimpleNamespace(value=b"d" * 32)),
    )


def _accepted():
    command = command_pb2.WorkerCommandV1(
        execution_id=_EXECUTION_ID,
        capability_id=delivery.AGENT_EXECUTE_APPLICATION_CAPABILITY_ID,
        resource_project_id="90107",
        projection_project_id="90107",
    )
    return SimpleNamespace(
        verified=SimpleNamespace(command=command),
        claim_id="claim",
        entry=SimpleNamespace(
            content=SimpleNamespace(digest=b"d" * 32),
            entry_id="request", immutable_version="version",
        ),
    )


def _assert_diagnostic(capsys, stage, exception_name="DependencyUnavailable"):
    captured = capsys.readouterr()
    assert captured.out == ""
    assert _PRIVATE_VALUE not in captured.err
    diagnostic = json.loads(captured.err)
    assert set(diagnostic) == {
        "event", "stage", "execution_id", "exception_module", "exception_name", "frames",
    }
    assert diagnostic["event"] == "agent_execution_internal_failure"
    assert diagnostic["stage"] == stage
    assert diagnostic["execution_id"] == _EXECUTION_ID
    assert diagnostic["exception_module"] == "elitea_worker.execution.errors"
    assert diagnostic["exception_name"] == exception_name
    assert 1 <= len(diagnostic["frames"]) <= delivery._INDEX_INTERNAL_FAILURE_FRAME_LIMIT
    for frame in diagnostic["frames"]:
        assert set(frame) == {"file", "function", "line"}
        assert "/" not in frame["file"] and "\\" not in frame["file"]
        assert isinstance(frame["line"], int)
    return diagnostic


@pytest.mark.parametrize("stage", ["input_materialization", "input_context"])
def test_input_dependency_diagnostic_preserves_the_error_and_redacts_values(
    monkeypatch, capsys, stage,
):
    processor = _processor()
    error = DependencyUnavailable(_PRIVATE_VALUE)

    async def fail(*args, **kwargs):
        raise error from RuntimeError(_PRIVATE_VALUE)

    async def fetched(*args, **kwargs):
        return b"input"

    processor._input_client = SimpleNamespace(
        fetch_materialized=fail if stage == "input_materialization" else fetched,
    )
    processor._client_context_factory = fail
    monkeypatch.setattr(delivery, "_claim_bound_reference", lambda *args, **kwargs: object())
    monkeypatch.setattr(delivery, "parse_agent_execution_input", lambda value: object())
    monkeypatch.setattr(delivery, "agent_request_from", lambda *args, **kwargs: object())

    with pytest.raises(DependencyUnavailable) as caught:
        asyncio.run(processor._resolve_inputs(_accepted(), receipt=_receipt()))

    assert caught.value is error
    diagnostic = _assert_diagnostic(capsys, stage)
    assert diagnostic["frames"][-1]["function"] == "fail"


def test_malformed_input_diagnostic_uses_a_fixed_stage_and_redacts_bytes(monkeypatch, capsys):
    processor = _processor()

    async def fetched(*args, **kwargs):
        return b"\x00" + _PRIVATE_VALUE.encode()

    async def context(claim):
        pytest.fail("malformed input must not redeem the SDK client context")

    processor._input_client = SimpleNamespace(fetch_materialized=fetched)
    processor._client_context_factory = context
    monkeypatch.setattr(delivery, "_claim_bound_reference", lambda *args, **kwargs: object())

    with pytest.raises(InvalidInput):
        asyncio.run(processor._resolve_inputs(_accepted(), receipt=_receipt()))

    _assert_diagnostic(capsys, "input_decode", "InvalidInput")


def test_checkpoint_dependency_diagnostic_preserves_the_error_and_redacts_values(
    monkeypatch, capsys,
):
    processor = _processor()
    cause = RuntimeError(_PRIVATE_VALUE)

    def connect(*args, **kwargs):
        raise cause

    factory = CurrentAgentCheckpointFactory(
        fallback_connection_string="postgresql://private-fixture",
        connection_factory=connect,
        saver_factory=lambda connection: object(),
        max_attempts=1,
    )

    def execute(request):
        with factory.open(SimpleNamespace(unsecret=lambda key: None), project_id=90107):
            pytest.fail("a failed checkpoint setup must not execute the SDK")

    callback = SimpleNamespace(
        configure_skills=lambda **kwargs: None,
        emit_agent_start=lambda **kwargs: None,
        capture_materialization_authorization=lambda error: False,
    )
    monkeypatch.setattr(delivery, "CurrentAgentNodeEventCallback", lambda *args: callback)
    monkeypatch.setattr(delivery, "nested_skill_registry_from_payload", lambda payload: None)
    monkeypatch.setattr(
        delivery.EliteaSdkAgentAdapter, "from_context", lambda *args, **kwargs: object(),
    )
    monkeypatch.setattr(
        delivery, "AgentExecutionHandler", lambda adapter: SimpleNamespace(execute=execute),
    )

    async def authorize(receipt):
        pass

    async def run_sync(function):
        return function()

    processor._authorize_invocation = authorize
    processor._supervisor = SimpleNamespace(run_sync=run_sync)
    processor._checkpoint_factory = factory
    request = SimpleNamespace(payload=SimpleNamespace(
        execution_generation="generation", thread_id="thread", conversation_id="conversation",
        applied_skills=[], attached_skills=[], invoked_skills=[],
    ))
    resolved = delivery._ResolvedAgentInputs(request=request, client_context=object())
    progress = SimpleNamespace(publish_from_sdk=lambda event: None)

    with pytest.raises(DependencyUnavailable) as caught:
        asyncio.run(processor._execute_resolved(
            _accepted(), resolved, receipt=_receipt(), progress=progress,
        ))

    assert caught.value.__cause__ is cause
    diagnostic = _assert_diagnostic(capsys, "execute_worker")
    assert diagnostic["frames"][-1]["file"] == "checkpoint.py"
    assert diagnostic["frames"][-1]["function"] == "open"
