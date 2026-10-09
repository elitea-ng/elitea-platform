# Pipeline MCP authorization continuation

Branch `fix/pipeline-mcp-auth-continuation-routing`, 2026-10-08. Worker only; Main's wire contract is unchanged.

## Business behaviour

In the current platform (SDK `langraph_agent.py`), a pipeline node that calls an MCP tool behind delegated
authorization pauses with the "Authorization required" card. **Authorize** retries the same call with the user's
token; **Skip** stops the pipeline at that node with the "authorization ... was skipped" message and runs no
dependent node. The same applies when the tool is also a configured sensitive action: the user approves the
sensitive card first, then answers the authorization card.

Ported unchanged. Not ported: the current platform's "lost, run again" behaviour when a resume cannot be joined to
its pause. A decision that does not match the current pause is refused with a typed error.

## Defect

Reproduced in a browser before the fix: a pipeline with one direct MCP node (`type: mcp`, toolkit pointed at
`mcp-mock /mcp-auth`) pauses correctly, but **Skip Auth** and **Authorize** both fail with "The execution input is
invalid." (Worker: `native_agent.invalid_input`, "the checkpointed pipeline decision was rejected"). The same toolkit
in a saved agent (non-pipeline) works.

| Layer | Cause |
|---|---|
| Main wire (`services/elitea-main/internal/application/agentexecution/continue.go`, `CurrentContinuationAuthorization`) | Sends every MCP authorization as `should_continue=true`, `hitl_resume=true`, `hitl_action`, `hitl_value=""`, one `hitl_decisions` entry with `guardrail_type: "mcp_auth"`, plus `mcp_tokens` (Authorize) or object-form `user_declined_mcp_servers` (Skip). |
| Worker routing (`src/agents/runtime.rs`, pipeline admission) | Bound the pipeline MCP continuation only for `hitl_resume=false` with no decisions, a shape Main never sends. Main's shape went to `PipelineContinuationDecision::Sensitive` with no Toolkit decision. |
| Worker resolve (`src/agents/graph/resume.rs`, `PipelineContinuationDecision::resolve`) | The latest event is a pipeline MCP-auth pause, not an Application pause, so the Application binding failed and the missing Toolkit decision was reported as `CorruptSession` → `InvalidInput`. |
| Tests | `mcp_resume_payload` stripped `hitl_resume`/`hitl_action`/`hitl_decisions` and called the MCP continuation directly, bypassing the routing. |

A second defect on the same path, found by the new sequence test: when the tool is both sensitive and behind
authorization, **Skip/Authorize after the sensitive approval** was refused as `StaleDecision`. The executor keeps an
interrupted node pending without its updates (`adk-graph` `executor.rs`), so the consumed approval is still in the
checkpoint's `__elitea_tool_resume_v1` when the authorization card is raised, and the resolver refused any leftover
entry.

## Changes

| Change | Where |
|---|---|
| Payload routing extracted into `pipeline_continuation_start` so tests drive the production routing | `src/agents/runtime.rs` |
| A single `mcp_auth` decision also carries a `PipelineMcpAuthorizationContinuation`. It is used only when the latest event is not an Application pause; the Application path is unchanged. | `src/agents/graph/resume.rs` (`PipelineContinuationDecision::{from_payload,resolve}`) |
| `PipelineMcpAuthorizationContinuation::from_payload` accepts Main's shape: exactly one decision, `guardrail_type == "mcp_auth"`, `hitl_action` equals the decision's action, `hitl_value == ""`, empty decision value. Authorize requires tokens and no declines; Skip requires declines and no tokens. | `resume.rs` (`PipelineMcpAuthorizationCard`) |
| The decision's `interrupt_id`/`tool_call_id` must equal the persisted pause card, otherwise `StaleDecision` | `resume.rs` (`PipelineMcpAuthorizationContinuation::resolve`) |
| The authorization pause tolerates exactly one leftover entry: the consumed sensitive approval of the same node, call, definition and arguments | `resume.rs` (`validate_direct_tool_authorization_frontier`, `resume_state_is_clear`) |
| A decision that matches no current pause reports `StaleDecision`, not `CorruptSession` | `resume.rs` (`PipelineContinuationDecision::resolve`) |
| `PipelineMcpAuthEventBinding::interrupt_id` accessor | `src/agents/events.rs` |

## Tests

New, in `src/agents/graph/direct_tool_tests.rs`:

- `main_wire_mcp_authorization_resumes_direct_pipeline_node_through_routing` builds Main's real payload
  (`mcp_wire_resume_payload`), routes it through `pipeline_continuation_start`, and resolves it against the
  persisted direct MCP-auth pause, as `session.rs::resolve_pipeline_start` does. For Skip and Authorize it asserts:
  a foreign `interrupt_id` and a foreign `tool_call_id` are `StaleDecision`; the exact card resolves to a
  `__elitea_tool_resume_v1` entry with the right action and call id; the resumed graph reaches END (Skip:
  `report = null`; Authorize: the tool result).
- `sensitive_approval_then_mcp_authorization_on_same_direct_node`: sensitive card → approve → authorization card →
  Skip or Authorize → END without a further pause; Authorize runs the tool.

New, in `src/agents/graph/resume.rs`: `predecessor_tests::only_the_exact_consumed_approval_of_the_leaf_node_may_remain`
(other node, other action, extra entry and non-object are all refused).

Before the fix (production code reverted, tests kept): both graph tests failed with `CorruptSession` (what
`session.rs` reports as `native_agent.invalid_input`). After the routing fix alone, the sequence test still failed
with `StaleDecision`; the frontier change fixed it.

Also new: `main_wire_mcp_authorization_refuses_action_and_credential_disagreement` (Skip with a token, Authorize
with a decline, and a `hitl_action` that disagrees with the decision are all `InvalidInput`).

Totals (`cargo test --offline --locked --all-targets --all-features`, 2026-10-09):
- before merging `main`: 2,119 passed, 0 failed, 63 ignored;
- after merging `main`: 1,662 passed, 0 failed, 63 ignored. The difference is #1163/#1165, which moved toolkit and
  ADK-patch tests into the shared runtime crate.

The ignored tests are the existing `#[ignore]` Docker/Kubernetes and live-service tests; none were added. `cargo fmt --all -- --check` and
`cargo clippy --locked --all-targets --all-features -- -D warnings` pass.

## Performance

**Budget.** No new I/O or round trips. Admission parses one extra `hitl_decisions[0]` object (bounded by the
existing payload bounds). Resolve does the same single session read and checkpoint loads as the existing MCP path.

**Result.** One small JSON value is built and compared per MCP-auth resume. No allocation on any other path.

## Durability

| Component × phase | Class | Enforced by | Proven by |
|---|---|---|---|
| Worker × HITL pause and decision (pipeline direct MCP authorization) | **R** — resumes the exact checkpoint, the paused node re-runs once with the decision; completed nodes do not re-run | `resume.rs` `PipelineMcpAuthorizationContinuation::resolve` (checkpoint id + pending node + card binding) | `main_wire_mcp_authorization_resumes_direct_pipeline_node_through_routing`; browser reload rows below |
| Worker × HITL decision after sensitive approval (same call) | **R** | `validate_direct_tool_authorization_frontier` | `sensitive_approval_then_mcp_authorization_on_same_direct_node` |
| Worker × replayed or foreign decision | **F** — typed `StaleDecision` → `native_agent.invalid_input`; checkpoint untouched | card binding in `resolve` | foreign-identity assertions in the new test |

No state schema, migration or checkpoint format changes. The tolerated leftover entry is checked for exact equality,
so a pending decision of another node or action still blocks the resume.

## Resilience

- All inputs are bounded by the existing payload bounds; identities by `valid_identity` (512 bytes, no controls).
- Action and credential collections must agree; disagreement is `InvalidInput`, never silently coerced.
- An unmatched decision is a typed `StaleDecision`, not `CorruptSession`.

## Security

| Category (`rules/security.md`) | Applies | How it was checked |
|---|---|---|
| Trust boundaries and identity | Yes | The decision is joined to the Worker's own persisted pause: card `interrupt_id`/`tool_call_id`, checkpoint id, pending node and the exact server URL of the requirement. A foreign card is refused (test). |
| Authorization (object level) | Yes | Unchanged at Main: Main consumes the card under the caller's authority. The Worker adds the card-to-pause binding, so a decision cannot resume another call. |
| Input, parsing and amplification | Yes | `deny_unknown_fields`, single decision, bounded identities, empty value. |
| Injection and construction | No | No SQL, shell, template or URL construction. |
| Egress and SSRF | No | No new outbound call. The authorized retry uses the existing MCP client path. |
| Secrets | Yes | `mcp_tokens` remain claim-fetched; no new logging, error text or event carries them. Test tokens are synthetic. |
| Supply chain | No | No dependency change. |

**Dependency audits (2026-10-08).** This change adds or updates no dependency. `cargo-deny` 0.20.2
`--all-features check advisories` on the Worker reports only the findings already recorded on `main` (see
[pipeline-size-bounds-http-snapshot-20261008.md](pipeline-size-bounds-http-snapshot-20261008.md)): RUSTSEC-2026-0258
(`h2` 0.4.15), RUSTSEC-2023-0071 (`rsa` 0.9.10) and the yanked `chacha20` 0.10.1. No new findings. Main and Web are
unchanged, so `govulncheck` and `npm audit` are not affected by this change.

## Browser evidence

**Stack.** A private standalone stack, `deploy/scripts/standalone-stack.sh` with `STANDALONE_WORKER=rust`, compose
project `elitea-mcpauth`, NATS standalone, real backend, no response mocks. Every image was built from this branch at
`87b7c1ef` under the private tag `mcpauth-20261008`, so the shared `:standalone` tags and other local stacks were not
touched. `seed`, `seed-runtime` and `seed-llm` ran (offline mock model). Browser host `http://mcpauth.localhost:18130`,
signed in through the local OIDC mock as `e2e-chat@autotest.local` (project 90107). Model `vllm/E2E-MOCK-MODEL`.

| Image | Identity |
|---|---|
| `elitea-worker-rust:mcpauth-20261008` | `sha256:b45f78ca3d10…` |
| `elitea-main:mcpauth-20261008` | `sha256:100805eac6bb…` |
| `elitea-web:mcpauth-20261008` | `sha256:3070a44ee41d…` |

**Binary identity check.** Worker images built before #1160 can share the BuildKit `/cargo-target` cache across
worktrees and ship another branch's binary. The evidence image's `/usr/local/bin/elitea-worker-rust` was extracted
(`docker create` + `docker cp`) and checked with `strings`:
- symbols that exist only on this branch are present: `validate_direct_tool_authorization_frontier`,
  `PipelineMcpAuthorizationCard`, `RawPipelineMcpAuthorizationDecision`, `is_single_mcp_authorization_decision`,
  `pipeline_continuation_start`;
- `sensitive_tool_resume_entry`, added by the later review commit, is absent.

So the binary is this branch at `87b7c1ef`.

**Fixtures (UI unless stated).**
- MCP toolkit 1 `mcpauthpipe`: created in the MCPs form; `url: https://mcp-mock:8443/mcp-auth` and
  `selected_tools: [echo, reverse]` entered in the Raw JSON view (Main's tool discovery is refused by its
  private-destination guard on this stack, so the tool list cannot load).
- Pipeline 1 / version 1 `mcp-auth-direct`: created in the pipeline form; one `type: mcp` node `auth`, tool `echo`,
  `input_mapping: {text: {type: fixed, value: x}}`, `transition: END` (flow-style YAML typed into the editor);
  toolkit attached under Tools → MCP.
- Sensitive-tool policy `sensitive_tools: {mcp: [echo]}` for the sequence case: written as `e2e-admin@autotest.local`
  through the admin guardrails endpoint the Configuration page saves to, called from the admin's browser session.
  The page's own "Add toolkit — Sensitive Action Tools" button could not be used: it adds a blank row that the
  value conversion drops at once (follow-up 2).

| Case | Chat | Executions (worker) | Result |
|---|---|---|---|
| Direct MCP node → **Skip Auth** | 1 | `afb75897…` (pause), `8399c073…` (Skip) | "Pipeline stopped — authorization for mcpauthpipe (tool: echo, node: auth) was skipped." Same after reload. Before the fix this click failed with "The execution input is invalid." (reproduced 2026-10-08 on a rust-worker NATS stack before this work) |
| Direct MCP node → **Authorize** | 1 | `b80b312c…` (pause) | The card shows "Authorization details are unavailable"; the Web sends no continuation, because the mock's resource metadata leads to no OAuth server. The Worker's Authorize path is proven by the unit tests only (follow-up 1). |
| Sensitive approval, then auth card on the same node → **Approve**, then **Skip Auth** | 2 | `e6809058…` (sensitive pause), `cab4bd04…` (Approve → auth pause), `f095686f…` (Skip) | Approve leads to the authorization card; Skip ends with the same "was skipped" message. Same after reload. |

Worker log over the whole session: 0 `native agent assembly failed` lines. The only error codes are the expected
`mcp.authorization_required` (3 pauses) and `session.invalid_scope` on the first session read of a new chat.

## Reviews

- **Code review** (high effort, full diff). Fixed:
  - the pipeline authorization parse no longer gates the nested-Application path (it is non-fatal; a pipeline MCP
    pause without a valid card resolves as `StaleDecision`);
  - the approval entry is built by one shared constructor (`sensitive_tool_resume_entry`), so the tolerated
    predecessor cannot drift from what `PipelineToolDecision` writes;
  - the redundant `authorize` bool was removed.

  Not changed, recorded as follow-ups 3 and 5–7.
- **Security review.** No findings. Checked: a decision cannot resume another call, node, thread or project (card,
  checkpoint and pending-node binding); a sensitive tool cannot run without approval (the tolerated entry is an exact
  match of what the Worker wrote, and only on an `mcp_auth` pause); a token cannot be used for another server
  (`matches_token_key` against the persisted requirement); `mcp_tokens` reach no new log, error or event.
- **Browser revision.** The browser rows ran on `87b7c1ef`. The review commit after it changes no behaviour
  exercised there; its new behaviour is unit-proven. It was not re-run in the browser because the shared Docker host
  was saturated (follow-up 8).

**Platform matrix.** `docs/recovery-guarantees.md` gained a `Worker x P08` row for this decision path, and its
malformed-input PostgreSQL row now cites the foreign-card and disagreement tests.

## Follow-ups

1. **Browser Authorize for a pipeline MCP node** needs a stack that issues a token: the `oauth-emulator` overlay, or
   a mock `/mcp-auth` that accepts a bearer token. With the current mock, an authorized retry would still be
   refused, which ends the node with the "authorized Toolkit was not available" stop (`direct_tool.rs`,
   `mcp_authorization_stopped(refresh_failed)`).
2. **Admin Configuration "Add toolkit" is a no-op** (`apps/elitea-web/src/pages/admin/ConfigurationToolMapEditor.tsx`):
   the appended `{toolkit: '', tools: []}` row is dropped by `fromConfigToolMapRows`, so Sensitive Action Tools and
   Blocked Tools cannot be added from the page. Separate Web defect.
3. **Nested pipelines.** A direct MCP node inside a nested pipeline (`nested_checkpoints`) uses the same code path,
   including the leaf-checkpoint approval allowance, but has no test.
4. The legacy token/decline-only shape (`hitl_resume=false`) remains accepted for compatibility; Main does not send
   it today, and it carries no card identity to bind. Retire it once no client sends it.
5. `resolve_routed_continuation` (tests) mirrors `session.rs::resolve_pipeline_start` rather than calling it.
6. `RawPipelineMcpAuthorizationDecision` duplicates part of `direct_hitl.rs`'s decision parsing; one shared parser
   would keep the two wire validators in sync.
7. `PipelineContinuationDecision::Sensitive` carries three mutually exclusive options (`application`, `tool`,
   `authorization`); a dedicated variant would make the invalid combinations unrepresentable.
8. Re-run the Skip and sequence rows on the final head when the Docker host has capacity.
