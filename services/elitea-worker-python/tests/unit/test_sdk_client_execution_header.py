"""The claim's execution id reaches the SDK client's model call (issue 875).

Until this change, `EliteaClientContext` carried no execution id and none of
the three `from_context` sites ever passed `api_extra_headers`, so
`X-Elitea-Execution-Id` never reached elitea-main's `/llm` proxy from either
worker. `internal/llmproxy/identity.go`'s `HeaderExecutionID` was therefore a
column with a reader (shared migration 0100, GetAgentAnalytics, estimate.go's
by_agent and by_tool) and no producer: `agent_dimension_available` would have
stayed `false` forever in production, not because no agent ran, but because
nothing ever said so.

The fake below repeats the SDK's own header rule (`api_extra_headers` copied
onto `default_headers` for the model call — `elitea_sdk/runtime/clients/
client.py`), the same one `test_sdk_client_secret_header.py` uses, so a test
that passes here states what the real client would build.
"""

from __future__ import annotations

from typing import Any

import pytest

from elitea_worker.agents import sdk_adapter as sdk_adapter_module
from elitea_worker.agents.client_context import EliteaClientContext
from elitea_worker.agents.sdk_adapter import (
    EliteaSdkAgentAdapter,
    EliteaSdkIndexingAdapter,
    EliteaSdkToolkitToolAdapter,
)

_EXECUTION_ID_HEADER = "X-Elitea-Execution-Id"


class FakeEliteAClient:
    """The header rule the admitted SDK client applies to its own arguments."""

    def __init__(
        self,
        *,
        project_id: int,
        base_url: str,
        auth_token: str,
        api_extra_headers: dict[str, str] | None = None,
        **kwargs: Any,
    ) -> None:
        self.project_id = project_id
        self.base_url = base_url
        self.auth_token = auth_token
        self.kwargs = dict(kwargs)
        self.api_extra_headers = dict(api_extra_headers) if api_extra_headers else {}


@pytest.fixture(autouse=True)
def _admitted_client(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        sdk_adapter_module, "_indexing_client_type", lambda: FakeEliteAClient
    )


def _context(execution_id: str = "") -> EliteaClientContext:
    return EliteaClientContext(
        42, "https://elitea.internal", "actor-pat", "", execution_id
    )


@pytest.mark.parametrize(
    "build",
    [
        lambda context: EliteaSdkAgentAdapter.from_context(context),
        lambda context: EliteaSdkIndexingAdapter.from_context(context),
        lambda context: EliteaSdkToolkitToolAdapter.from_context(context),
    ],
    ids=["agent", "indexing", "toolkit-call-tool"],
)
def test_the_execution_id_reaches_the_model_call_header(build) -> None:
    adapter = build(_context("exec-alpha-1"))
    client = adapter._client

    # api_extra_headers, not XSECRET: this value has to reach the MODEL call
    # as a header, which is exactly what api_extra_headers becomes
    # (default_headers) and XSECRET never does.
    assert client.api_extra_headers == {_EXECUTION_ID_HEADER: "exec-alpha-1"}
    assert client.auth_token == "actor-pat"
    assert client.project_id == 42


@pytest.mark.parametrize(
    "build",
    [
        lambda context: EliteaSdkAgentAdapter.from_context(context),
        lambda context: EliteaSdkIndexingAdapter.from_context(context),
        lambda context: EliteaSdkToolkitToolAdapter.from_context(context),
    ],
    ids=["agent", "indexing", "toolkit-call-tool"],
)
def test_no_execution_id_adds_no_argument(build) -> None:
    """A client built outside an execution claim must not send an empty
    header — nothing constructs one that way today, but a caller reading the
    header must not have to distinguish "empty" from "absent".
    """

    client = build(_context())._client

    assert client.api_extra_headers == {}
