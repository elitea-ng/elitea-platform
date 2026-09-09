"""Claim-scoped in-memory authorization for the existing EliteA SDK client."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from urllib.parse import urlsplit

from elitea_worker.execution.errors import DependencyUnavailable, InvalidInput, WorkerError


@dataclass(frozen=True, slots=True)
class RuntimeExecutionClaim:
    execution_id: str
    generation: int
    claim_id: str
    fence_token: bytes
    resource_project_id: str

    def __post_init__(self) -> None:
        required_text = (
            self.execution_id,
            self.claim_id,
            self.resource_project_id,
        )
        if (
            any(not _bounded_text(value, 256) for value in required_text)
            or self.generation < 1
            or len(self.fence_token) != 32
        ):
            raise InvalidInput("The execution claim identity is malformed.")


# MAX_SECRETS_HEADER_VALUE_BYTES bounds one HTTP header value. The platform
# generates 43 characters of base64url; the bound is large enough for a value an
# operator set by hand and small enough that no request line grows without
# limit.
MAX_SECRETS_HEADER_VALUE_BYTES = 4 * 1024


@dataclass(frozen=True, slots=True)
class RedeemedRuntimeContext:
    """What one claim redemption returns from the runtime-context route.

    It carries two credentials, and they answer different things. ``auth_token``
    is the execution's whole identity: with no token nothing runs.
    ``secrets_header_value`` is the project's ``X-SECRET`` value, which
    authenticates ONE route — the version-details read the sub-agent toolkit
    makes (issue 408).

    ``secrets_header_value`` is therefore allowed to be empty, and an empty
    value is not a failure of the redemption. A project whose vault holds no
    value, and a platform that is older than the field, both arrive here as an
    empty string, and both mean the same thing: the SDK cannot authenticate a
    version-details read, and that route refuses it with 403.
    """

    auth_token: str = field(repr=False)
    secrets_header_value: str = field(default="", repr=False)

    def __post_init__(self) -> None:
        if not _bounded_text(self.auth_token, 64 * 1024):
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            )
        if self.secrets_header_value and not _bounded_text(
            self.secrets_header_value, MAX_SECRETS_HEADER_VALUE_BYTES
        ):
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            )


@dataclass(frozen=True, slots=True)
class EliteaClientContext:
    project_id: int
    base_url: str
    auth_token: str = field(repr=False)
    # The project's own X-SECRET value (issue 408). It is empty when the project
    # has none; the SDK client is then built without the header, and the
    # platform refuses the one route that reads it.
    secrets_header_value: str = field(default="", repr=False)
    # The runtime execution this client is scoped to (issue 875). Forwarded to
    # the LLM call path as X-Elitea-Execution-Id so the gateway's request log
    # (shared migration 0100) can carry it — the value
    # internal/llmproxy/identity.go's HeaderExecutionID reads off the inbound
    # request and re-signs, and the only producer the agent-cost read
    # (internal/api/v2/analytics/estimate.go) has. Empty for a client built
    # outside an execution claim (there is none today; every construction site
    # is claim-bound), which is the same "not every caller carries one" case
    # the header's own edge-side validation already tolerates.
    execution_id: str = ""

    def __post_init__(self) -> None:
        if self.project_id < 1 or not _valid_base_url(self.base_url):
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            )
        if not _bounded_text(self.auth_token, 64 * 1024):
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            )
        if self.secrets_header_value and not _bounded_text(
            self.secrets_header_value, MAX_SECRETS_HEADER_VALUE_BYTES
        ):
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            )

ClaimBoundTokenFetcher = Callable[
    [RuntimeExecutionClaim], Awaitable[RedeemedRuntimeContext]
]


class ClaimBoundEliteaClientContextFactory:
    """Combine trusted nonsecret origin with a claim-bound execution actor PAT."""

    def __init__(
        self,
        *,
        base_url: str,
        token_fetcher: ClaimBoundTokenFetcher,
    ) -> None:
        if not _valid_base_url(base_url) or token_fetcher is None:
            raise ValueError("claim-bound SDK client context policy is incomplete")
        self._base_url = base_url.rstrip("/")
        self._token_fetcher = token_fetcher

    async def __call__(self, claim: RuntimeExecutionClaim) -> EliteaClientContext:
        try:
            project_id = int(claim.resource_project_id)
        except (TypeError, ValueError) as exc:
            raise InvalidInput("The execution project identity is malformed.") from exc
        if str(project_id) != claim.resource_project_id or project_id < 1:
            raise InvalidInput("The execution project identity is malformed.")
        try:
            redeemed = await self._token_fetcher(claim)
        except WorkerError:
            raise
        except Exception as exc:
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            ) from exc
        if not isinstance(redeemed, RedeemedRuntimeContext):
            raise DependencyUnavailable(
                "The claim-scoped SDK client context is unavailable."
            )
        return EliteaClientContext(
            project_id=project_id,
            base_url=self._base_url,
            auth_token=redeemed.auth_token,
            secrets_header_value=redeemed.secrets_header_value,
            execution_id=claim.execution_id,
        )


# Backward-compatible name for the already-shipped indexing composition. The
# claim is capability-neutral; the Go authorizer decides which capabilities may
# redeem it.
IndexExecutionClaim = RuntimeExecutionClaim


def _valid_base_url(value: object) -> bool:
    if not _bounded_text(value, 2048):
        return False
    parsed = urlsplit(value)
    return bool(
        parsed.scheme == "https"
        and parsed.hostname
        and parsed.username is None
        and parsed.password is None
        and parsed.path in {"", "/"}
        and not parsed.query
        and not parsed.fragment
    )


def _bounded_text(value: object, maximum_bytes: int) -> bool:
    if not isinstance(value, str) or not value or any(
        character in value for character in ("\r", "\n", "\x00")
    ):
        return False
    try:
        return len(value.encode("utf-8")) <= maximum_bytes
    except UnicodeEncodeError:
        return False
