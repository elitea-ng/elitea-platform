# Effectful tools in pipeline direct tool nodes

Status: proposed. The capability stays disabled. Assembly still refuses effectful direct tools, and the
refusal now names the node kind and the missing guarantees.

## Decision summary

- A direct tool node (`type: toolkit` or `type: mcp`) may run a tool that is not annotated read-only
  only when Gate 6 (`docs/remaining-gates.md:60`) is satisfied for that node. Gate 6 requires durable
  intent, an effect receipt, idempotency or reconciliation, approval, and fencing.
- Sensitive effectful tools use the existing direct-node confirmation (`execute_sensitive`) without
  changes. The receipt is the new part.
- Non-sensitive effectful tools use the same intent and receipt, but without a pause.
- Until the effect owner described below ships, assembly refuses these tools with
  `native_agent.unsupported_capability` and a data-free message (`src/agents/pipeline.rs:2233`).
  The browser keeps the code-mapped text "Configuration type is not supported."
  (`services/elitea-main/internal/transport/runtimegrpc/output/server.go:1112`).

## Current platform behaviour (reference only)

The behaviour we keep from elitea-sdk:

- A sensitive tool in a pipeline toolkit or MCP node pauses before the tool runs, with approve,
  reject or block_with_comment (`elitea_sdk/runtime/middleware/sensitive_tool_guard.py:257-341`).
  There is no edit action.
- On block, the pipeline stops with an assistant message and `_pipeline_blocked`, and declared
  outputs are nulled (`tools/function.py:282-335`).
- On an auth-guarded MCP tool, the node shows the authorize/skip card (`tools/function.py:377-426`).
  Skip ends the pipeline and a failed refresh fails closed (`:337-376`).
- Effectful and read-only tools are treated the same way.

What we deliberately do not port:

- **Fail-open decisions.** A non-dict resume, or a missing or unknown action, counts as approve
  (`sensitive_tool_guard.py:314-333`). Rust rejects malformed decisions (`src/agents/graph/direct_tool.rs:886-935`).
- **Repeated effects.** If the process dies after the tool returns but before the step checkpoint,
  the approved tool runs again. Nothing records that the effect already happened.
- **MCP auth identity without the graph step**, so revisits of a looped node are not told apart.
  Rust uses `pipeline:{node}:{step}` (`direct_tool.rs:1039`).

## Current Rust state

| Concern | Where | Behaviour |
|---|---|---|
| Assembly gate | `src/agents/pipeline.rs:2122` | Refuses `!is_read_only()` with `unsupported_direct_tool_effect()` |
| Runtime gate (defence in depth) | `src/agents/graph/direct_tool.rs:413` | Same rule at node execution |
| Sensitive pause | `direct_tool.rs:472-511`, `:770-870` | Interrupt carries masked args, argument digest and definition digest. The decision must match all three. |
| MCP authorization | `direct_tool.rs:606-704` | The placeholder tool is read-only by construction (`src/toolkits/mcp.rs:843-845`), so it already passes the gate |
| Read-only source | ADK MCP toolset, `readOnlyHint` (default false); toolkits per tool | `idempotentHint` is not used today |
| Receipt types | `src/agents/graph/node_recovery.rs:83-97` (`ReplaySafety`) | Code nodes already append a fenced `Started` record before dispatch and record `CompletedExternalEffect` after it. Direct, LLM and ordinary tool paths do not. |

## Design

1. **Admission.** Remove the read-only refusal only behind a capability that the effect owner turns
   on. An unknown or unclassified tool stays refused. Policy (blocked toolkit or tool) still runs
   first and uses `unsupported_direct_tool_scope()`.
2. **Effect identity.** `effect_id = SHA-256(domain || execution || node || graph step || call_id ||
   definition_digest || argument_digest)`. This reuses the digests the confirmation already binds,
   so approval and effect refer to the same call. Arguments are never stored in clear. Only the
   digest and the existing masked preview are kept.
3. **Confirmation (sensitive only).** Keep `execute_sensitive` unchanged. An approval only allows the
   effect. It is not the effect record.
4. **Durable intent.** Before `tool.execute`, append a fenced `Started{effect_id}` record under the
   claim's `StateWriterLease`. If the append fails, the tool does not run (typed failure, the
   decision is kept).
5. **Receipt.** After the tool returns, write `CompletedExternalEffect{receipt_id}` with the bounded,
   redacted projected output. It is written in the same transaction as the step checkpoint, or
   strictly before it.
6. **Replay.** On re-execution of the node:
   - receipt found → project the stored output and never call the tool (class R);
   - `Started` without a receipt and the tool declares idempotency (MCP `idempotentHint`, or a
     toolkit action with an idempotency key) → retry with the same key (class I);
   - otherwise → `UnknownExternalEffect`: no blind repeat. Use reconciliation from owner proof where
     the toolkit offers it. If it does not, return a typed failure with a support reference and
     route it to the operator, keeping partial results (class C, falling back to F).
7. **Bounds.** The receipt payload is capped by the existing pipeline output limit. There is one
   receipt per (node, step), and the count per execution is capped.

## Recovery guarantees (target)

| Component × phase | Today | Target |
|---|---|---|
| Worker × effectful direct tool call | F: refused at admission | R once a receipt exists. I for idempotent tools. Otherwise C with an F fallback. |
| Worker × sensitive pause/decision | R (graph checkpoint + digest-bound decision) | unchanged |
| Main × admission | F (typed refusal) | unchanged |
| PostgreSQL × intent/receipt write | n/a | Intent before effect. Receipt with checkpoint. A writer-fence loss stops the effect. |

## Known adjacent gap

Pipeline LLM nodes admit effectful sensitive tools today. Admission is a blocklist (`src/toolkits/policy.rs:227-240`),
and approval runs through the ADK confirmation path (`src/agents/graph/llm.rs:1326-1470`) with no
effect receipt. They have the same repeated-effect window as point 2 of "What we deliberately do not
port". The effect owner above should serve both node kinds, so the two get one guarantee.
This is recorded as a gap; this change does not alter it.

## Tests required before enabling

- Each crash window on real PostgreSQL: before intent, after intent and before the effect, after
  the effect and before the receipt, after the receipt and before the checkpoint. Exactly one effect
  for idempotent tools, and no blind repeat for the others.
- A second claim taking over a paused or in-flight node while the old writer is fenced.
- Approve, reject and block on effectful toolkit and MCP tools. Replay of a consumed decision gives
  `StaleDecision`.
- Logs and events contain no argument values, only digests and the masked preview.
- Limit and limit+1 for the receipt payload and the receipt count.
- Browser: sensitive effectful MCP tool (mock `echo`) shows the approve/block card, survives a
  reload, and a Worker restart after approval does not repeat the call.

## Options

| Option | Outcome |
|---|---|
| A. Keep the refusal (this change) | Clear, typed error. No customer-visible effect from direct nodes. |
| B. Implement the effect owner for direct nodes, then LLM nodes | Business parity with stronger durability. A multi-PR slice of Gate 6. |
| C. Admit with confirmation only | Rejected. It ports the repeated-effect defect and fails Gate 6. |
