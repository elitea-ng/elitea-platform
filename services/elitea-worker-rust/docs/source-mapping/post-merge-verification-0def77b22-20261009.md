# Post-merge verification of merged main `0def77b22` — 2026-10-09

The batch merged on 2026-10-09 skipped the CI queue, which was backlogged by about 20 PRs. This document records how
that batch was verified on merged `main` instead: the full local suites, then a real-browser pass on a fresh stack
with real models.

**PRs covered:**
- #1144, #1145, #1146, #1147, #1148, #1149, #1150;
- #1157, #1158, #1159, #1160, #1161, #1162;
- #1168, #1169, #1170, #1172, #1173.

Each PR's own source mapping still holds that PR's design and per-PR evidence. This document adds only the merged-tree
evidence. No product code changed.

## Stack

- **Compose project:** `elitea-verify-0def77b2`, built from `deploy/docker-compose.standalone-full.yml` and
  `deploy/docker-compose.standalone-rust-agent.yml` at `0def77b22`. Images were built one at a time, all tagged
  `main-0def77b22-verify`.
  - Main `sha256:3606e044e59d…`, also used by both migrate jobs.
  - Worker (Rust) `sha256:ac344ae3dc56…`.
  - Web `sha256:93bc19de99af…`.
- **Binary identity**, extracted with `docker create` + `docker cp`, then checked with `strings`:
  - The Worker contains `elitea.graph.aggregate.config.v1` (#1157), `native_agent.instruction_bytes_exceeded` and
    `graph.pipeline.yaml_bytes_exceeded` (#1158).
  - Main contains `infra/egress` (#1149) and `agent_interrupt_already_resolved` / `execution_interrupt_audit`
    (#1150).
  - `elitea-migrate` embeds `execution_interrupt_responses` (0157).
- **Database:** a restore of a product-database snapshot that holds real model configurations, migrated with the
  stack's own `elitea-migrate -all-tenants`.
  - Shared migrations went from 153 to 157 and tenant migrations from 146 to 148, which is `main`'s head. No manual
    migration SQL was run.
  - One manual step: `ALTER EXTENSION vector SET SCHEMA public`, because the snapshot had pgvector in another schema.
- **Models:** the project's real models were reached through the stack's LLM gateway: gpt-5.4, gpt-5.4-mini,
  eu.anthropic.claude-sonnet-4-6, eu.anthropic.claude-haiku-4-5 and global.openai.gpt-5.6-luna. The offline mock
  model was not seeded.
- **One configuration difference from the compose default:** Main runs with
  `ELITEA_RUNTIME_TOOLKIT_DISCOVERY_ENABLED=true`. Without it, MCP tool lists answer 503 "toolkit discovery
  unavailable" (follow-up 3).
- **Login:** through the stack's OIDC mock as the snapshot's project admin. No passwords were involved.

## Local suites on `0def77b22`

| Suite | Result |
|---|---|
| Go `vet`, 13 `go.work` modules | Pass |
| Go `test --race`, CI environment variables, real PostgreSQL 18, NATS, S3 (rustfs) | 16,736 tests ran, 59 skipped. One failure (`internal/infra/authattempt` `TestPostgresAdmitterIsAtomicAcrossInstancesAndDoesNotGrowDeniedBacklog`) was a lock-timeout flake under machine saturation: it passed in isolation, and so did the whole package. |
| Real-PostgreSQL gates: editor lifecycle (13 scenarios), compiled snapshots, `tests/integration`, `sqlc generate` / `vet` (no diff), contract static checks (18/18) | Pass |
| Worker `cargo test --offline --locked --all-targets --all-features`, real PostgreSQL 18 receipt tests on | 1,827 passed, 0 failed, 71 ignored. The three `--ignored` CI suites ran too: deadline 6, preparation 17, hydration 12, all passing. |
| `services/elitea-code-runner` tests | 136 passed, 2 ignored |
| `cargo fmt --check` | Clean |
| Web: typecheck, `lint --deny-warnings` | Pass |
| Web: vitest (node 26 / npm 11) | 16,684 passed. 24 timed out under load; all 24 passed on a re-run with `--maxWorkers=2`. |

**Skips (59).**
- **Declared (30):** these need a real LLM, the Python SDK, Rust engine binaries, staging credentials or a worker
  container. CI does not run them either.
- **Undeclared (29):**
  - secured-NATS permission tests, which need natscli (not downloaded);
  - the Azure and GCS storage conformance suites, whose emulator images are not local;
  - one NATS-backed repository test.

**Not run:**
- `services/elitea-llm-gateway` vet and tests: the module needs Go ≥ 1.26.6 and the local toolchain was 1.26.5;
- the OpenAPI drift check: `oapi-codegen` is not cached;
- golangci-lint, Rust clippy and doc, Playwright e2e, the helm workflows and image scans.

**Scanners.**
- **`cargo deny` (Worker):** only RUSTSEC-2023-0071 (rsa via sqlx-mysql), which is accepted and mitigated in #1144.
- **`npm audit --omit=dev`:** 2 low findings, `katex` and `mermaid`.
- **`govulncheck`:**
  - The standard-library findings come from the local go1.26.5 toolchain.
  - One called module finding: `services/elitea-subapp-host`, `golang.org/x/text` GO-2026-6629. It predates the
    batch (follow-up 4).

## Real-browser evidence

Chrome-based built-in browser, no response mocks. "Reload" means a full page navigation followed by a database check
of `p_2.chat_messages_text`.

| PR | Check | Result |
|---|---|---|
| Real models | Chat 867, model gpt-5.4-mini from the picker. The picker lists the five shared real models. | "VERIFIED Tokyo" (message 4430); persists after reload |
| #1168 | Pin conversation 867 in the UI, then 24 concurrent `POST`/`DELETE` calls to `/api/v2/social/pin/prompt_lib/2/conversation/867` from the page | 24 × 200, no 500; still pinned after reload |
| #1149 | Settings → Webhooks → create with `https://100.64.1.10/…` (CGNAT) and `https://[::ffff:127.0.0.1]/…` (IPv4-mapped loopback) | Both refused inline: `webhook destination refused: "<host>" does not resolve to a permitted destination …`. Only the host is echoed. |
| #1158 | Saved pipeline 153 (`id: 1`, `entry_point: 1`, a numeric node id) opened in the editor and run | The editor loads (it used to crash on numeric ids); the answer is `numeric-id-ok` (message 4433) |
| #1162 + #1161 | Pipeline 160, created in the UI: a router → `left`/`right` state_modifiers → one shared `llm` node (gpt-5.4-mini) → END | `right` → "RIGHT BRANCH JOINED" (4436); `left` → "LEFT BRANCH JOINED" (4439). The shared node runs once per pass, and the answer is the last real writer. |
| #1157 gate | Pipeline 161 with `type: split_out` on a production Worker build | The runtime refuses it with the typed "Configuration type is not supported." and a support reference; nothing runs. The editor blocks saving after reload. Gap: the first create saved the graph (follow-up 1). |
| #1170 | Admin → Configuration → Guardrails → "Add toolkit — Sensitive Action Tools" → `mcp` / `reverse` → Save | The row renders, saves, and is still there after reload |
| #1159 | Pipeline 162: a direct `mcp` node calling `reverse` (sensitive through the Guardrails row above), then a direct `echo`. Effects were counted from the mock MCP's `tools/call` log. | **Wrong output type:** after Approve, the typed "tool result does not match the node output mapping"; 1 effect, not re-run. **Corrected fixture:** the pause shows the "Sensitive Action Authorization Required" card with 0 effects while paused; Approve runs `reverse` once and `echo` once, and the answer is `{"output":"verify-main-downstream"}` (4446). Reload adds 0 effects (total 3). |
| #1159 scope | The same pipeline before the MCP was attached under Tools → MCP | Refused before any effect. The Worker logs `native_agent.invalid_input`: "a pipeline direct tool node references a tool outside its frozen scope". The user sees the generic "The execution input is invalid." (follow-up 2). |
| #1169 | MCP 101 `verifyauthmcp` (`https://mcp-mock:8443/mcp-auth`) and pipeline 163 with one direct `echo` node, both created in the UI → "Authorization required" card → **Skip Auth** | "Pipeline stopped — authorization for verifyauthmcp (tool: echo, node: auth) was skipped." (4449). The protected tool never ran: total `tools/call` stayed at 3. Authorize is disabled because the mock MCP has no OAuth server, as recorded in #1169's own mapping. |
| #1146, #1150 | — | Not reachable from the UI. The Parallel and Map compile gates are `false` on `main`, and the #1150 decision API is behind a default-off flag. Covered by the passing Worker and Main real-PostgreSQL suites above. |

**Fixtures.**
- **Created in the UI:** pipelines 160–163, MCP 101, and the Guardrails row.
- **Taken from the restored snapshot:** pipeline 153, MCP 100 (`c1 mock mcp 20261008`; Echo and Reverse were
  selected in the UI) and chat 867.
- **YAML entry:** the YAML was entered through the editor's paste handler with a scripted paste event, because typed
  input is re-indented by the editor. Each stored version was checked in `p_2.application_versions`.

## Performance, durability, resilience, security

This pass changed no code, so it adds no new mechanisms. It re-proves the merged mechanisms on the merged tree:
- **Durability (#1159):** an approved effect ran exactly once; a reload re-ran nothing; a typed failure was not
  retried. This is recovery class **I/C**, as recorded in `direct-tool-effects-20261008.md`.
- **Resilience:** every refusal in the browser was typed and readable, apart from the two generic messages listed
  under follow-ups 1 and 2.
- **Security (#1149):** the new CGNAT and IPv4-mapped refusals hold at create time and echo only the host. The frozen
  tool scope refused a toolkit that was not attached before any effect.
- **Performance (#1168):** 24 concurrent pin/unpin requests finished without a deadlock (40P01) or a 500.

## Follow-ups

1. **Create-path admission.**
   - A new pipeline whose graph the runtime refuses (`split_out` on a production build) is saved on the first create.
     Only after reopening does the editor block saving.
   - The runtime refusal "Configuration type is not supported." should name the node.
2. **Frozen-scope refusal message.** It should name the node and toolkit and say how to fix it, for example by
   attaching the toolkit under Tools.
3. **MCP setup on standalone stacks.**
   - Tool discovery is off by default, so the MCP page shows "tool list did not load".
   - The pipeline Tools → MCP picker lists only the first page of 20 tools, so newer MCPs appear only after a search.
4. **`x/text` in `subapp-host`** (GO-2026-6629) and the `x/net` floor in several modules.
5. **Not run locally:** secured-NATS permission tests, Azure and GCS conformance, llm-gateway tests (go1.26.6), and
   the OpenAPI drift check. These need downloads.
