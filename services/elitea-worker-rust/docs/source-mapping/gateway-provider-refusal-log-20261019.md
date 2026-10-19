# Gateway: log why a provider refused a request (2026-10-19)

## Why

The gateway request log keeps only the status class of a failed request
(`error_code = client_error`). After a direct-HITL decision on the scoped
rehearsal stack, the continuation's model call answered 403, and nothing showed
whether the gateway or the upstream refused it. The Worker reported it as
`model_gateway.forbidden` ("The model service denied access").

## Business behaviour

No change to what a caller receives. The gateway already returned an
OpenAI-shaped error with a sanitised message. It now also writes one WARN line
per refusal.

Current platform reference: none. The current gateway (LiteLLM) has no
equivalent fixed-field refusal line, and its verbose request logging is not
ported.

## Changed paths

- `services/elitea-llm-gateway/internal/llmproxy/httpio.go`:
  - `logProviderRefusal`, called from `writeOpenAIError` (every unary and pre-stream refusal);
  - `refusalToken`, which bounds each provider field to 64 bytes and a token charset.
- `services/elitea-llm-gateway/internal/llmproxy/handler.go`: both mid-stream error frames (Chat Completions and Responses) call `logProviderRefusal`.
- `services/elitea-llm-gateway/internal/llmproxy/provider_refusal_log_test.go`: new tests.

The log line has fixed fields:

- `event = llm_provider_refusal`;
- `status`;
- `gateway_origin`;
- `error_type`, `error_code` and `error_param`, as the provider reported them;
- `provider` and `model`.

## Tests

`GOWORK=off go test ./...` in `services/elitea-llm-gateway`: 17 packages pass. `go vet ./...` is clean.

| Test | Proves |
|---|---|
| `TestProviderRefusalLogKeepsFixedFieldsOnly` | One JSON line with the fixed fields. The provider message, which can echo prompt content, never reaches the log. |
| `TestRefusalTokenRefusesFreeText` | Empty values stay empty. Tokens up to 64 bytes pass. Spaces, newlines, quotes, non-ASCII, and anything over 64 bytes become `redacted`. |

## Performance

The refusal path only: one `slog` line per refused request, and no allocation on success. Each field is at most 64 bytes.

## Durability

Not affected: the change only logs.

## Resilience

The log call cannot fail the request. A nil logger or nil error is a no-op.

## Security

- Allowlist logging: only the status, the provider's short identifiers and the routing names are logged.
- Never logged: the provider message, the request or response body, headers, and credentials.
- Provider fields pass through `refusalToken`, so free text, including injected text, is replaced by a fixed marker.
- Proven by `TestProviderRefusalLogKeepsFixedFieldsOnly`, which plants prompt-like text in the provider message and asserts it is absent from the line.

## Recovery guarantee rows

No row changes. This is observability on a path that already returned a typed failure.

## Evidence on the scoped stack

The image was built from `d5b3e88dd` as `ghcr.io/elitea-ng/elitea-llm-gateway:gwreason-d5b3e88dd`. Binary check: the extracted `/elitea-llm-gateway` contains `llm_provider_refusal`.

Only the gateway was deployed on stack `scoped`. The line it logged for the 403:

```
{"level":"WARN","msg":"provider refused the request","event":"llm_provider_refusal","status":403,"gateway_origin":false,"error_type":"","error_code":"","error_param":"","provider":"vllm","model":"gpt-5.4-mini"}
```

What the line established:

- `gateway_origin=false` means the refusal came from the upstream.
- The empty type and code mean a bare 403 from an edge in front of the provider, not a provider policy error.

A local-only diagnostic build, which was never committed and has since been removed, saved the refused bodies. Replaying them through the stack's authenticated `/llm` route and changing one element at a time showed the edge refuses combinations of ordinary content:

- a backtick-quoted word in a tool description, together with
- either a single-quoted label in the platform's block result, or a stored answer sent as one content block per streamed chunk.

Each element alone passes. The Worker-side changes are in the approved-effectful-HITL PR. This change only makes such refusals visible.

## Follow-ups

- The upstream edge (in front of the shared model endpoint) refuses benign content patterns, for example backticks in tool descriptions. That is an environment issue for the endpoint owner.
