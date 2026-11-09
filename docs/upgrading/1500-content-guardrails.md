---
title: "Content guardrails (#1500, off until you create an entry)"
issue: 1500
date: 2026-10-30
order: 62
---

ADR-0043 decision 1. Shared migration **0214** adds the governance type
`guardrail` to the `governance_config_type_known` constraint (still
`NOT VALID`) and creates `gateway.guardrail_verdicts`, the verdict table
of the LLM gateway. The table holds no content.

- **No behaviour change until an entry exists.** The gateway runs no
  guardrail when `gateway.governance_config` has no enabled `guardrail`
  row.
- **Upgrade order.** Apply the migration before the new gateway starts.
  A gateway that finds no verdict table drops the verdicts for a retry
  window and keeps its request log rows. An older gateway rejects a
  `guardrail` row at load and logs the reason, so create entries after
  every gateway runs this release.
- **New refusals.** A matching entry with `action: block` refuses with
  HTTP 400 `guardrail_error` / `guardrail_blocked`. A detector failure
  with `fail_mode: closed` (the default for block and mask entries)
  refuses with HTTP 503 `guardrail_error` / `guardrail_unavailable`. A
  client that retries every 5xx answer retries a closed failure too.
- **Streams.** An output entry with a `mask` detector holds back
  `stream_window_bytes` (256 by default) of each streamed answer. A
  blocked stream ends with an error event: a `data:` frame on the OpenAI
  routes and an `event: error` frame on the Responses and Anthropic
  routes.
- **HTTP adapter.** The call is outbound. Add the adapter host to the
  **Egress allowlist** governance entry, and store its credential as a
  secret of the public project (`{{secret.NAME}}`).
- **Run steps API.** The first page of `GET /api/v2/elitea_core/run_steps/
  prompt_lib/{project_id}/{run_id}` has a new optional field,
  `guardrail_verdicts`.
- **Retention.** The gateway prunes the verdicts with its request log, on
  the `gateway_request_log` window.
