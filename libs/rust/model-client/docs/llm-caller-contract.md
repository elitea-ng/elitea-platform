# The `/llm` caller contract

How a Rust service calls the platform's model edge (`elitea-main` `/llm`,
`internal/llmproxy`, in front of the LLM gateway). There are two callers:

- the agent worker: `services/elitea-worker-rust`, `transport/openai_compatible_facade.rs`;
- the engines' model client: this crate, used by DeepWiki and Inventory.

Both must call it alike, so that the edge, the budget gate and the spend
analytics cannot tell them apart.

The machine-readable half is `conformance/llm-caller/contract.json`. Both
callers assert it:

- the worker in `every_contract_refusal_has_the_contract_code_and_retry_hint`
  and `assert_exact_captured_request`;
- this crate in `tests/caller_contract.rs`.

A change to the contract changes that file, and both suites run on it.

The Python SDK's side of the gateway is covered separately: the
`elitea-sdk compatibility` section of `services/elitea-llm-gateway/CLAUDE.md`,
and the tier-1/2/3 SDK conformance gates.

## Request

| | Value |
| --- | --- |
| Route | `POST {base}/llm/v1/chat/completions` (the engine also uses `/llm/v1/embeddings`) |
| `Authorization` | `Bearer <token>`, marked sensitive. The worker uses the execution actor's token; an engine uses the callback token minted for its invocation. |
| `X-Project-Id` | The billing project (ADR-0018's primary selector). A bound token with any other selector gets 400 `project_scope_conflict`. |
| `X-Elitea-Execution-Id` | The run the call belongs to (see below) |
| Not sent | `OpenAI-Organization` (the edge's fallback selector) and `OpenAI-Project` (never a selector) |
| Body | OpenAI chat completions: `max_completion_tokens`; when streaming, `stream_options.include_usage: true`; `reasoning_effort` only when configured |

### The execution id

The edge keeps an execution id only when it can vouch for it. It never
refuses a call for it: an absent, malformed, unknown or unverifiable id is
dropped, and the call is billed with no run.

1. **Shape** (`executionIDFromHeader`): 1–128 bytes of `A-Za-z0-9-_.`. There
   is no `:`, because the evaluation namespace uses it.
2. **Runtime execution** (the worker): 32 lowercase hex characters, the
   claimed execution. It is kept when `elitea_runtime.execution_jobs` holds
   it for the resolved project and actor, and it is unsettled or settled
   within 5 minutes (`ExecutionAttributionVerifier.VerifyExecution`).
3. **Provider invocation** (an engine): `callback-<token uuid>`. The facade
   writes it into `llm_settings.execution_id` (`material.CallbackSettings`).
   It is kept only when the uuid names the token that authenticated the call
   (`auth.User.TokenID`), owned by the caller, bound to the project, and not
   expired more than 5 minutes ago (`VerifyCallbackExecution`). Only the
   holder of that bearer can claim the id, and only while the bearer lives.

A kept id is re-signed into the gateway identity (signature v2). It lands in:

- `gateway.llm_request_logs.execution_id`;
- `gateway.llm_usage_events`.

## Refusals

The gateway's body is `{"error": {"message", "type", "code", "scope"?}}`.
Every caller maps a final status to the same code:

| Status | Code | Retryable |
| --- | --- | --- |
| 402, `type: budget_exceeded`, `scope: member` (or no scope with `code: member_budget_exceeded`) | `model_gateway.member_budget_exhausted` | no |
| 402, `type: budget_exceeded`, `scope: project` | `model_gateway.project_budget_exhausted` | no |
| 402, anything else (a provider's own quota) | `model_gateway.budget_exhausted` | no |
| 401 | `model_gateway.unauthorized` | no |
| 403 | `model_gateway.forbidden` | no |
| 408, 504 | `model_gateway.upstream_timeout` | yes |
| 429 | `model_gateway.rate_limited` | yes |
| 409 | `model_gateway.conflict` | yes |
| 5xx | `model_gateway.unavailable` | yes |
| other 4xx | `model_gateway.rejected` | no |

The engine's user-facing message names the scope ("The member model budget
is exhausted …"), and its category is `invalid_input`. Neither caller ever
repeats the gateway's text unsanitised.

## Reasoning models

- **`reasoning_effort`:** `none | low | medium | high`, sent only when
  configured (`llm_settings.reasoning_effort`). With any effort other than
  `none` no `temperature` is sent; the worker refuses that pair at
  configuration.
- **Reasoning text:** read from `reasoning_content` or `reasoning`, in the
  message or in stream deltas. Both present and different is refused. It is
  never part of the answer, and never streamed as answer text.
- **Usage:** `completion_tokens_details.reasoning_tokens` (the worker's
  thinking token count) and `prompt_tokens_details.cached_tokens`.
- **Bare `</think>`:** a server without a reasoning parser (Qwen3 on vLLM)
  leaves a bare `</think>` in `content`. The engine takes the text before the
  last one as reasoning.

## Tracing

One span per model request, exported over OTLP (HTTP/protobuf). Export is on
when `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` is
set, unless `OTEL_SDK_DISABLED=true`.

| | Worker | Engine |
| --- | --- | --- |
| Span | `agent.model.request` | `engine.model.request` |
| Shared attributes | `model_adapter`, `model_name`, `billing_project_id`, `streaming`, `outcome`, `error_code`, `retryable` | the same |
| Own attributes | `turn`, `tool_count` | `engine`, `operation`, `execution_id`, `attempts`, `http_status` |
| `service.name` | `elitea-worker-rust` | `elitea-deepwiki-engine`, … |

## Limits and timeouts

| | Worker | Engine |
| --- | --- | --- |
| Transport | HTTP/2 only, through `platform-edge` over TLS | HTTP/1.1 or 2, to the callback base (`ELITEA_*_CALLBACK_BASE_URL`): cleartext in-cluster, or `platform-edge` over TLS when the chart's `callbackViaPlatformEdge` is on |
| Retries | none in the facade (the ADK decides from `retryable`) | up to `llm_settings.max_retries` (default 2), backoff 0.5 s · 2ⁿ ≤ 8 s, honours `retry-after` ≤ 60 s |
| Response headers / non-stream request | 120 s | 10 min |
| Stream idle | 120 s | 5 min |
| Stream total | none | 2 h |
| Refusal body read | 4 KiB | 16 KiB |
| SSE caps | the adk decoder's | line and event 1 MiB, stream 64 MiB, 200 000 events |

## Known differences (follow-ups, not contract)

- **Redirects:** the engine refuses them (no redirect policy), so a 3xx
  status is a final, non-retryable refusal. The worker files 3xx as
  `unavailable`, which is retryable. The code agrees; the retry does not.
- **Worker gaps found in the 2026-10-07 review:**
  - no HTTP retries of its own;
  - no total-stream timeout;
  - a client certificate that is never rotated and that the edge does not
    check;
  - no embeddings client.
- **Engine transport:** by default an engine reaches elitea-main directly
  over plain HTTP inside the cluster.
  - With `deepwiki.callbackViaPlatformEdge: true` (Helm; needs
    `worker.platformEdge`), the callback origin becomes
    `worker.runtime.platformOrigin`.
  - The host and the engine then trust the runtime CA through
    `ELITEA_DEEPWIKI_CALLBACK_CA_FILE`, which only the `runtime-ca.crt` key
    of the worker's material Secret reaches. This is the worker's posture.
  - The edge itself forwards to elitea-main over plain HTTP and does not
    verify client certificates, so the gain is TLS on the pod-to-edge hop.
