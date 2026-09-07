"""Retrieve a claim-bound execution actor PAT for user-initiated indexing."""

from __future__ import annotations

import base64
import json
from urllib.parse import quote, urlsplit

import httpx

from elitea_worker.agents.client_context import (
    MAX_SECRETS_HEADER_VALUE_BYTES,
    IndexExecutionClaim,
    RedeemedRuntimeContext,
)
from elitea_worker.execution.errors import (
    AuthorizationFailure,
    DependencyUnavailable,
    InvalidInput,
    ResourceExhausted,
    WorkerError,
)

TOKEN_CONTEXT_SCHEMA = "elitea.runtime.elitea-client-token.v1"
MAX_TOKEN_CONTEXT_BYTES = 32 * 1024

# RUNTIME_CONTEXT_ACCEPT_HEADER tells the platform which OPTIONAL fields this
# worker can read. The platform serves such a field only to a request that names
# it, because this client refuses a response that carries a key it does not know
# (see the key-set comparison below). Without the negotiation, a platform that
# added a field would fail every redemption on every worker that a rolling
# deploy had not yet replaced.
#
# Its Go half is `runtimeContextAcceptHeader` in
# services/elitea-main/internal/infra/storage/content_server.go.
RUNTIME_CONTEXT_ACCEPT_HEADER = "x-elitea-runtime-context-accept"

# SECRETS_HEADER_FIELD is the project's own X-SECRET value (issue 408). The SDK
# sends the literal "secret" when nothing supplies one, and the platform refuses
# that literal, so this field is what makes a sub-agent call authenticate.
SECRETS_HEADER_FIELD = "secrets-header-value"


class ClaimBoundEliteaTokenClient:
    """Fetch one non-cached token using an already accepted claim and mTLS."""

    def __init__(
        self,
        client: httpx.AsyncClient,
        *,
        origin: str,
        timeout_seconds: float = 15.0,
        max_response_bytes: int = MAX_TOKEN_CONTEXT_BYTES,
        require_http2: bool = False,
    ) -> None:
        if timeout_seconds <= 0 or not 1 <= max_response_bytes <= MAX_TOKEN_CONTEXT_BYTES:
            raise ValueError("runtime client-token policy is incomplete")
        self._client = client
        self._origin = _canonical_https_origin(origin)
        self._timeout = timeout_seconds
        self._max_response_bytes = max_response_bytes
        self._require_http2 = require_http2

    async def __call__(self, claim: IndexExecutionClaim) -> RedeemedRuntimeContext:
        url = (
            f"{self._origin}/executions/{quote(claim.execution_id, safe='')}"
            f"/generations/{claim.generation}"
            "/runtime-context/elitea-client-token"
        )
        fence = base64.urlsafe_b64encode(claim.fence_token).rstrip(b"=").decode("ascii")
        body = bytearray()
        try:
            async with self._client.stream(
                "POST",
                url,
                headers={
                    "x-elitea-claim-id": claim.claim_id,
                    "x-elitea-fence": fence,
                    RUNTIME_CONTEXT_ACCEPT_HEADER: SECRETS_HEADER_FIELD,
                },
                content=b"",
                timeout=self._timeout,
                follow_redirects=False,
            ) as response:
                if self._require_http2 and response.http_version != "HTTP/2":
                    raise DependencyUnavailable(
                        "The runtime context service did not negotiate HTTP/2."
                    )
                if _response_origin(response) != self._origin:
                    raise InvalidInput("The runtime context response changed origin.")
                if response.status_code in (401, 403):
                    raise AuthorizationFailure(
                        "The claim-bound runtime context was rejected."
                    )
                if response.status_code < 200 or response.status_code >= 300:
                    raise DependencyUnavailable(
                        "The runtime context service did not accept the request."
                    )
                if response.headers.get("content-type", "").split(";", 1)[0].lower() != "application/json":
                    raise InvalidInput("The runtime context response type is malformed.")
                cache_directives = {
                    value.strip().lower()
                    for value in response.headers.get("cache-control", "").split(",")
                    if value.strip()
                }
                if not {"no-store", "no-cache"}.issubset(cache_directives):
                    raise InvalidInput("The runtime context cache policy is malformed.")
                if response.headers.get("pragma", "").strip().lower() != "no-cache":
                    raise InvalidInput("The runtime context cache policy is malformed.")
                declared = response.headers.get("content-length")
                if declared is None or not declared.isascii() or not declared.isdecimal():
                    raise InvalidInput("The runtime context length is malformed.")
                declared_length = int(declared)
                if declared_length < 1 or declared_length > self._max_response_bytes:
                    raise ResourceExhausted(
                        "The runtime context exceeds the approved limit."
                    )
                async for chunk in response.aiter_bytes():
                    if len(body) + len(chunk) > declared_length:
                        raise InvalidInput("The runtime context length is malformed.")
                    body.extend(chunk)
        except WorkerError:
            raise
        except httpx.HTTPError as exc:
            raise DependencyUnavailable("The runtime context service is unavailable.") from exc

        if len(body) != declared_length:
            raise InvalidInput("The runtime context length is malformed.")
        try:
            value = json.loads(
                body.decode("utf-8"),
                object_pairs_hook=_unique_object,
                parse_constant=_reject_constant,
            )
        except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as exc:
            raise InvalidInput("The runtime context response is malformed.") from exc
        # The key set stays CLOSED. Only the fields this worker asked for above
        # may appear, so a response carrying anything else is still refused —
        # the negotiation widens what may be served, not what may be believed.
        if not isinstance(value, dict) or not set(value) <= {
            "schema_version",
            "project_id",
            "token",
            "secrets_header_value",
        }:
            raise InvalidInput("The runtime context response is malformed.")
        if not {"schema_version", "project_id", "token"} <= set(value):
            raise InvalidInput("The runtime context response is malformed.")
        project_id = value["project_id"]
        token = value["token"]
        if (
            value["schema_version"] != TOKEN_CONTEXT_SCHEMA
            or isinstance(project_id, bool)
            or not isinstance(project_id, int)
            or project_id < 1
            or str(project_id) != claim.resource_project_id
            or not isinstance(token, str)
            or not token
            or any(character in token for character in ("\r", "\n", "\x00"))
        ):
            raise AuthorizationFailure(
                "The runtime context does not match the accepted execution."
            )
        try:
            if len(token.encode("utf-8")) > self._max_response_bytes:
                raise AuthorizationFailure(
                    "The runtime context does not match the accepted execution."
                )
        except UnicodeEncodeError as exc:
            raise AuthorizationFailure(
                "The runtime context does not match the accepted execution."
            ) from exc
        return RedeemedRuntimeContext(
            auth_token=token,
            secrets_header_value=_secrets_header_value(value),
        )


def _secrets_header_value(value: dict[str, object]) -> str:
    """Read the project's X-SECRET value, or answer with the empty string.

    An ABSENT field is a normal answer, and it has two causes that this worker
    cannot tell apart and does not need to: the project has no value, or the
    platform is older than the field. Both mean the SDK cannot authenticate a
    version-details read.

    A field that is PRESENT and malformed is a different thing. It is refused,
    because a header value with a line break in it would let the platform
    dictate a second header on every request this worker later makes, and a
    header value that is present but empty says the platform served a field it
    should have omitted.
    """

    if "secrets_header_value" not in value:
        return ""
    header_value = value["secrets_header_value"]
    if (
        not isinstance(header_value, str)
        or not header_value
        or any(character in header_value for character in ("\r", "\n", "\x00"))
    ):
        raise InvalidInput("The runtime context response is malformed.")
    try:
        if len(header_value.encode("utf-8")) > MAX_SECRETS_HEADER_VALUE_BYTES:
            raise InvalidInput("The runtime context response is malformed.")
    except UnicodeEncodeError as exc:
        raise InvalidInput("The runtime context response is malformed.") from exc
    return header_value


def _response_origin(response: httpx.Response) -> str:
    try:
        netloc = response.url.netloc.decode("ascii")
    except UnicodeDecodeError as exc:
        raise InvalidInput("The runtime context response changed origin.") from exc
    return _canonical_https_origin(f"{response.url.scheme}://{netloc}")


def _canonical_https_origin(value: str) -> str:
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
        raise ValueError("runtime context origin must be an HTTPS origin")
    try:
        port = parsed.port
        hostname = parsed.hostname.encode("ascii").decode("ascii").lower()
    except (UnicodeError, ValueError) as exc:
        raise ValueError("runtime context origin must be canonical") from exc
    host = f"[{hostname}]" if ":" in hostname else hostname
    if port is not None and port != 443:
        host = f"{host}:{port}"
    return f"https://{host}"


def _unique_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON member")
        result[key] = value
    return result


def _reject_constant(value: str) -> object:
    raise ValueError(f"non-finite JSON number: {value}")
