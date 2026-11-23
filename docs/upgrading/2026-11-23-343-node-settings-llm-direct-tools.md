# Runtime: Worker ships before Main for LLM and direct-tool per-node settings (5b-2, 5b-3, FU-343, FU-067)

Pipeline `llm`, `toolkit`, and `mcp` nodes now admit the shared `settings:` block (`retry_on_fail`, `max_tries`, `wait_between_tries_ms`, `timeout_ms`, `on_error`, `error_transition`, `always_output_data`, `execute_once`).

- **What changed:**
  - `libs/proto/contracts/pipeline-node-settings-v1.json` admits all 8 `settings:` keys on `llm`, `toolkit`, and `mcp` across Worker, Main, and Web.
  - `elitea-worker-rust` propagates typed `model_gateway.*` and `anthropic_gateway.*` failure classes through `adk-graph` (`FU-343`), surfaces bounded `Retry-After` hints from `429`/`503` responses (capped at `300_000` ms), retries LLM nodes only before the first visible output delta, classifies direct tools as read-only strictly from platform metadata (`Toolkit && tool.is_read_only()`; MCP `readOnlyHint` is never trusted), journals effectful direct tools with `Reconcile` (`0` blind repeats), and binds `DirectToolCommittedProjector` (`FU-067`) under `elitea.runtime.direct-tool-receipt.v1`.
  - `elitea-agent-runtime` extends the node-recovery journal codec to `SCHEMA_V5` (`FailureWithRetryAfter`) while staying backwards-compatible with `SCHEMA_V1`–`SCHEMA_V4` journal rows.
  - `libs/go/faults/registry/worker.json` registers 8 operator fault codes (`pipeline.llm.{failed,rate_limited,timeout,unavailable}`, `pipeline.mcp.{failed,timeout}`, `pipeline.toolkit.{failed,timeout}`).
- **Who is affected:** Deployments rolling out `elitea-worker-rust`, `elitea-main`, and `elitea-web` independently where authors add `settings:` blocks to `llm`, `toolkit`, or `mcp` pipeline nodes.
- **What to do:** **Deploy `elitea-worker-rust` before `elitea-main` and `elitea-web`.**
  - *New Worker + Old Main:* safe; existing pipelines keep identical configuration digests when `settings:` is absent or holds only default values, and Old Main still refuses `settings:` on `llm`/`toolkit`/`mcp` at save time until Main upgrades.
  - *Old Worker + New Main:* pipelines saved with `settings:` on `llm`, `toolkit`, or `mcp` are refused by Old Worker with `pipeline.node_settings.invalid` (`family_not_yet`) until Worker upgrades.
  - *Downgrade note:* an active tool-free LLM retry journal that appended a `SCHEMA_V5` (`FailureWithRetryAfter`) entry is rejected fail-closed by an older Worker binary (`corrupt_journal`); let in-flight `Retry-After` retries finish before rolling back Worker.
