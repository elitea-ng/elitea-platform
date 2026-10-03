"""#6732: a budget refusal names the scope that refused it, and nothing else.

The chat shows different copy, and a Usage link, for a member who is over
their own cap and for a project that is over its shared ceiling. Main tells the
two apart by the registered message under MODEL_BUDGET_EXHAUSTED; the Rust
worker and ``testdata/proto/runtime/v1/model_failure_policies.json`` carry the
same text, which is what this file pins for the Python worker.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from elitea.runtime.v1 import errors_pb2

from elitea_worker.execution.errors import (
    MEMBER_BUDGET_EXHAUSTED_MESSAGE,
    PROJECT_BUDGET_EXHAUSTED_MESSAGE,
    ModelBudgetExhausted,
    WorkerError,
)
from elitea_worker.protocol.codec import _runtime_error_message

_POLICIES = Path(__file__).resolve().parents[4] / "testdata/proto/runtime/v1/model_failure_policies.json"


@pytest.mark.parametrize(
    ("scope", "message", "public_code"),
    [
        ("project", PROJECT_BUDGET_EXHAUSTED_MESSAGE, "PROJECT_BUDGET_EXHAUSTED"),
        ("member", MEMBER_BUDGET_EXHAUSTED_MESSAGE, "MEMBER_BUDGET_EXHAUSTED"),
    ],
)
def test_scoped_budget_refusal_crosses_as_a_registered_message(
    scope: str, message: str, public_code: str
) -> None:
    payload = _runtime_error_message(ModelBudgetExhausted(scope))

    assert payload.code == errors_pb2.RUNTIME_ERROR_CODE_V1_MODEL_BUDGET_EXHAUSTED
    assert payload.safe_message == message
    assert payload.retryable is False
    registered = [
        policy
        for policy in json.loads(_POLICIES.read_text())
        if policy.get("public_code") == public_code
    ]
    assert [policy["message"] for policy in registered] == [message]


def test_an_unknown_scope_is_the_project_ceiling() -> None:
    assert ModelBudgetExhausted("bogus").safe_message == PROJECT_BUDGET_EXHAUSTED_MESSAGE


def test_a_free_text_budget_message_never_crosses() -> None:
    payload = _runtime_error_message(
        WorkerError("MODEL_BUDGET_EXHAUSTED", "raw provider text: insufficient_quota")
    )

    # The unscoped registered text, never the free text.
    assert payload.safe_message.startswith("The model budget is exhausted.")
    assert payload.code == errors_pb2.RUNTIME_ERROR_CODE_V1_MODEL_BUDGET_EXHAUSTED
