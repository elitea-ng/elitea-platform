"""The project X-SECRET value reaches the SDK client (issue 408).

The SDK reads ``kwargs.get('XSECRET', 'secret')`` and puts the result in every
platform request it makes (``elitea_sdk/runtime/clients/client.py``). Until this
change nothing in this repository supplied that argument, so every call from
this worker carried the literal ``secret``, and the platform accepted it on any
project whose vault held no value.

The platform now refuses that literal, so these tests guard the one channel that
replaces it. The fake below repeats the SDK's own header rule, so a test that
passes here states what the real client would build.
"""

from __future__ import annotations

from typing import Any

import pytest

from elitea_worker.agents import sdk_adapter as sdk_adapter_module
from elitea_worker.agents.client_context import EliteaClientContext
from elitea_worker.agents.sdk_adapter import (
    EliteaSdkAgentAdapter,
    EliteaSdkIndexingAdapter,
)


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
        self.headers = {
            "Authorization": f"Bearer {auth_token}",
            "X-SECRET": kwargs.get("XSECRET", "secret"),
        }
        if api_extra_headers is not None:
            self.headers.update(api_extra_headers)
        self.api_extra_headers = dict(api_extra_headers) if api_extra_headers else {}


@pytest.fixture(autouse=True)
def _admitted_client(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        sdk_adapter_module, "_indexing_client_type", lambda: FakeEliteAClient
    )


def _context(secrets_header_value: str = "") -> EliteaClientContext:
    return EliteaClientContext(
        42, "https://elitea.internal", "actor-pat", secrets_header_value
    )


@pytest.mark.parametrize(
    "build",
    [
        lambda context: EliteaSdkAgentAdapter.from_context(context),
        lambda context: EliteaSdkIndexingAdapter.from_context(context),
    ],
    ids=["agent", "indexing"],
)
def test_the_project_value_reaches_the_sdk_client_header(build) -> None:
    value = "Yk9tZS1yYW5kb20tcHJvamVjdC12YWx1ZQ"

    adapter = build(_context(value))
    client = adapter._client

    assert client.headers["X-SECRET"] == value
    assert client.kwargs["XSECRET"] == value
    # The value authenticates ONE platform route, so it must not be copied onto
    # the model call, which is what api_extra_headers does.
    assert client.api_extra_headers == {}
    # The identity the value travels beside is unchanged.
    assert client.auth_token == "actor-pat"
    assert client.project_id == 42


@pytest.mark.parametrize(
    "build",
    [
        lambda context: EliteaSdkAgentAdapter.from_context(context),
        lambda context: EliteaSdkIndexingAdapter.from_context(context),
    ],
    ids=["agent", "indexing"],
)
def test_no_project_value_adds_no_argument(build) -> None:
    """The worker does not invent a header when the project has none.

    The SDK then sends its own literal and the platform answers 403 with the
    repair, which is one refusal an operator can read. A header this worker made
    up would be a second, silent story.
    """

    client = build(_context())._client

    assert "XSECRET" not in client.kwargs
    assert client.headers["X-SECRET"] == "secret"
