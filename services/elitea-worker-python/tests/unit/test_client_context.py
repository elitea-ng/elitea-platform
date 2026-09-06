from __future__ import annotations

import asyncio

import pytest

from elitea_worker.agents.client_context import (
    ClaimBoundEliteaClientContextFactory,
    EliteaClientContext,
    IndexExecutionClaim,
    RedeemedRuntimeContext,
)
from elitea_worker.execution.errors import DependencyUnavailable, InvalidInput


def test_factory_fetches_fresh_token_for_each_exact_claim() -> None:
    async def run() -> None:
        observed: list[IndexExecutionClaim] = []

        async def fetch(claim: IndexExecutionClaim) -> RedeemedRuntimeContext:
            observed.append(claim)
            return RedeemedRuntimeContext(
                auth_token=f"token-{len(observed)}",
                secrets_header_value=f"header-{len(observed)}",
            )

        factory = ClaimBoundEliteaClientContextFactory(
            base_url="https://elitea.internal/",
            token_fetcher=fetch,
        )
        claim = _claim()

        first = await factory(claim)
        second = await factory(claim)

        assert observed == [claim, claim]
        assert first.project_id == 42
        assert first.base_url == "https://elitea.internal"
        assert first.auth_token == "token-1"
        assert second.auth_token == "token-2"
        assert "token-1" not in repr(first)
        # The project X-SECRET value travels with the token, and is redacted in
        # the same way (issue 408).
        assert first.secrets_header_value == "header-1"
        assert second.secrets_header_value == "header-2"
        assert "header-1" not in repr(first)

    asyncio.run(run())


def test_context_carries_no_secrets_header_value_by_default() -> None:
    """A project with no X-SECRET value is a usable context, not a failure.

    The turn still runs; only the sub-agent version-details read is refused,
    and the platform refuses it with a message that names the repair.
    """

    context = EliteaClientContext(42, "https://elitea.internal", "token")
    assert context.secrets_header_value == ""


@pytest.mark.parametrize("value", ["value\r\ninjected: 1", "v" * (4 * 1024 + 1)])
def test_context_rejects_a_secrets_header_value_no_header_may_carry(value) -> None:
    with pytest.raises(DependencyUnavailable):
        EliteaClientContext(42, "https://elitea.internal", "token", value)


def test_factory_rejects_noncanonical_project_or_missing_token() -> None:
    async def run() -> None:
        async def missing(_: IndexExecutionClaim) -> RedeemedRuntimeContext:
            return RedeemedRuntimeContext(auth_token="")

        factory = ClaimBoundEliteaClientContextFactory(
            base_url="https://elitea.internal",
            token_fetcher=missing,
        )
        with pytest.raises(DependencyUnavailable):
            await factory(_claim())

        with pytest.raises(InvalidInput):
            await factory(
                IndexExecutionClaim(
                    execution_id="execution-1",
                    generation=1,
                    claim_id="claim-1",
                    fence_token=b"f" * 32,
                    resource_project_id="042",
                )
            )

    asyncio.run(run())


def test_factory_rejects_plaintext_platform_origin() -> None:
    async def fetch(_: IndexExecutionClaim) -> RedeemedRuntimeContext:
        return RedeemedRuntimeContext(auth_token="token")

    with pytest.raises(ValueError, match="policy is incomplete"):
        ClaimBoundEliteaClientContextFactory(
            base_url="http://elitea.internal",
            token_fetcher=fetch,
        )


def _claim() -> IndexExecutionClaim:
    return IndexExecutionClaim(
        execution_id="execution-1",
        generation=1,
        claim_id="claim-1",
        fence_token=b"f" * 32,
        resource_project_id="42",
    )
