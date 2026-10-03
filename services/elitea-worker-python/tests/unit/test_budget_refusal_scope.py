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
from elitea_sdk.runtime.exceptions import BudgetExceededError

from elitea_worker.agents.sdk_adapter import SdkBudgetExceeded, _sdk_budget_scope

from elitea_worker.execution.errors import (
    MEMBER_BUDGET_EXHAUSTED_MESSAGE,
    PROJECT_BUDGET_EXHAUSTED_MESSAGE,
    UNSCOPED_BUDGET_EXHAUSTED_MESSAGE,
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


@pytest.mark.parametrize("scope", ["unknown", "bogus", ""])
def test_an_unknown_scope_is_the_unscoped_refusal(scope: str) -> None:
    # A provider's own quota refusal names no Elitea ceiling, so it must not
    # read as "this project's shared budget is exhausted".
    payload = _runtime_error_message(ModelBudgetExhausted(scope))

    assert payload.code == errors_pb2.RUNTIME_ERROR_CODE_V1_MODEL_BUDGET_EXHAUSTED
    assert payload.safe_message == UNSCOPED_BUDGET_EXHAUSTED_MESSAGE
    assert payload.safe_message != PROJECT_BUDGET_EXHAUSTED_MESSAGE
    registered = [
        policy["message"]
        for policy in json.loads(_POLICIES.read_text())
        if policy.get("code") == "MODEL_BUDGET_EXHAUSTED" and "public_code" not in policy
    ]
    assert registered == [UNSCOPED_BUDGET_EXHAUSTED_MESSAGE]


class _ProviderError(Exception):
    def __init__(self, body: object) -> None:
        super().__init__("provider error")
        self.body = body


def _sdk_error(scope: str, cause: BaseException | None) -> BaseException:
    error = BudgetExceededError("secret proxy text", scope)
    error.__cause__ = cause
    return error


@pytest.mark.parametrize(
    ("sdk_scope", "cause_body", "expected"),
    [
        # The gate's project refusal: OpenAI client body (wrapper stripped).
        (
            "project_budget_exceeded",
            {"type": "budget_exceeded", "code": "insufficient_quota", "scope": "project"},
            "project",
        ),
        # The Anthropic client keeps the wrapper.
        (
            "project_budget_exceeded",
            {"type": "error", "error": {"type": "budget_exceeded", "scope": "project"}},
            "project",
        ),
        ("member_budget_exceeded", None, "member"),
        (
            "project_budget_exceeded",
            {"type": "budget_exceeded", "code": "member_budget_exceeded", "scope": "member"},
            "member",
        ),
        # A provider's own quota refusal: same type and code, no scope.
        (
            "project_budget_exceeded",
            {"type": "budget_exceeded", "code": "insufficient_quota"},
            "unknown",
        ),
        ("project_budget_exceeded", {"type": "budget_exceeded", "scope": "everyone"}, "unknown"),
        ("project_budget_exceeded", None, "unknown"),
    ],
)
def test_sdk_budget_scope_reads_only_the_gateway_scope(
    sdk_scope: str, cause_body: object, expected: str
) -> None:
    cause = None if cause_body is None else _ProviderError(cause_body)
    assert _sdk_budget_scope(_sdk_error(sdk_scope, cause)) == expected  # type: ignore[arg-type]
    assert SdkBudgetExceeded(expected).scope == expected


def test_sdk_budget_scope_survives_a_cause_cycle() -> None:
    first = BudgetExceededError("x", "project_budget_exceeded")
    second = Exception("y")
    first.__cause__ = second
    second.__cause__ = first

    assert _sdk_budget_scope(first) == "unknown"


def test_a_free_text_budget_message_never_crosses() -> None:
    payload = _runtime_error_message(
        WorkerError("MODEL_BUDGET_EXHAUSTED", "raw provider text: insufficient_quota")
    )

    # The unscoped registered text, never the free text.
    assert payload.safe_message.startswith("The model budget is exhausted.")
    assert payload.code == errors_pb2.RUNTIME_ERROR_CODE_V1_MODEL_BUDGET_EXHAUSTED
