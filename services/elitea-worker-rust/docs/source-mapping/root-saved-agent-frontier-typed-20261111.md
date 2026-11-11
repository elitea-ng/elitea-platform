# A root saved-Agent node lost mid-run ends typed, not Internal (FU-291 D8, side fix)

- **Branch:** `fix/root-saved-agent-frontier-typed`, off `main` at `67cbe6c25`.
- **Paths:** `W/` = `services/elitea-worker-rust/src`, `M/` = `services/elitea-main/internal`.
- **The real fix is FU-323** ("a root saved-Agent node as a recovery frontier"): resume from the Agent's model
  checkpoint. This change only makes today's refusal readable and typed.

## Business behaviour

- **Before.** A pipeline whose root Agent node runs a saved Agent, with the Worker lost anywhere inside that node
  (its model call, a tool call, or a saved pipeline it calls as a tool), was reclaimed and refused at inspect with an
  uncaused `invalid_configuration`. The lifecycle maps that to `RuntimeFailureKind::Internal`, so the chat showed the
  generic internal failure. Found by FU-291 D8 (`0b01d3add` on `feat/fu291a-durable-pipeline-tool`).
- **Now.** The same reclaim ends with the registered `RECOVERY_FRONTIER_UNSUPPORTED` wire code and its own text:
  "This run stopped during an Agent step that cannot continue after a restart, so nothing after that step ran and
  nothing was repeated; check what the Agent completed, then start the run again." Nothing calls the model.
- **Not changed.** What recovery admits. An Agent node running a saved Agent is still not a recovery frontier; the
  pinned `an_agent_node_is_a_recovery_frontier_only_for_a_frozen_saved_pipeline` is untouched and passes.
- **Taxonomy choice: `RECOVERY_FRONTIER_UNSUPPORTED`, not `RECOVERY_REFUSED`.** `RECOVERY_REFUSED` means "an approved
  call may have started and was not repeated"; its text asks the user to check a target system for that call's
  effect. Here the refusal is about the frontier kind, whatever the Agent was doing (often only a model call), which
  is what `RECOVERY_FRONTIER_UNSUPPORTED` already means for the PM-2 walk and FU-265. A new kind under that wire code
  gives the precise text; its older-Main fallback is the generic frontier text of the same code.

## Changed paths

- Worker: `W/agents/graph/compiler.rs` (`saved_agent_frontier_node`), `W/agents/session.rs` (refusal and fault line),
  `W/execution/native_agent_lifecycle.rs` (cause → kind; `assembly_failure` now `pub(crate)` for the proof),
  `W/execution/mod.rs` (test-only re-export), `W/execution/toolkit_delivery_processor.rs` (kind → code),
  `W/protocol/output.rs` (kind, text, policy, table, fallback).
- Registry: `libs/go/faults/registry/runtime.json` (`runtime.recovery_frontier_agent_step`),
  `libs/go/faults/registry/worker.json` (`pipeline.recovery.saved_agent_frontier`), generated `codes_gen.go` and
  `docs/errors/README.md`.
- Main: `M/transport/runtimegrpc/output/server.go` (admits the text under its code only).
- Tests: `W/agents/root_saved_agent_frontier_pg_tests.rs` (new), `W/agents/graph/compiler_tests.rs` (one new test),
  `W/protocol/output.rs` (one existing test extended), `M/transport/runtimegrpc/output/recovery_frontier_agent_step_test.go`
  (new).
- Docs: `services/elitea-worker-rust/docs/recovery-guarantees.md` (one row), this mapping, the README entry,
  `docs/upgrading/2026-11-11-0-root-saved-agent-frontier-typed.md`.

## Design as built

- `PipelineDefinition::saved_agent_frontier_node` (`W/agents/graph/compiler.rs:1088`) names the first pending Agent
  node, owned by no fan-out, whose alias Main did not freeze as a saved pipeline. It names; it admits nothing.
- `inspect_checkpoint` (`W/agents/session.rs:2271-2290`): when `recovery_frontier_supported` is false and such a node
  is pending, it logs one WARN fault line and returns `invalid_configuration` caused by
  `pipeline.recovery.saved_agent_frontier`. Any other unsupported frontier keeps today's uncaused refusal.
- `assembly_failure` (`W/execution/native_agent_lifecycle.rs:1931-1937`) maps that cause to
  `RuntimeFailureKind::RecoveryFrontierAgentStep`, sent as `RECOVERY_FRONTIER_UNSUPPORTED` with
  `RECOVERY_FRONTIER_AGENT_STEP_MESSAGE` (`W/protocol/output.rs:1053,1254-1258`), never retryable.
- Version skew: `older_main_fallback` (`W/protocol/output.rs:944`) is `RecoveryFrontierUnsupported`. A Main without
  the new text refuses the frame once; the Worker resends the generic frontier text under the same code (W3-1 C3).
- Main admits the exact text under `RECOVERY_FRONTIER_UNSUPPORTED` only (`M/transport/runtimegrpc/output/server.go:1182,1236-1238`).

## Tests

| Test | Proves | Result |
|---|---|---|
| `postgres_a_root_saved_agent_node_lost_mid_run_is_refused_typed_on_reclaim` (two OS processes, real PostgreSQL, production assembler and the production inspect step) | Crash in the nested LLM node of a pipeline the root saved Agent calls: one `tool_may_have_started` boundary, root frontier `["delegate"]`. Reclaim: `RecoveryFrontierAgentStep` / `RUNTIME_ERROR_CODE_V1_RECOVERY_FRONTIER_UNSUPPORTED`, 0 model calls, exactly one WARN line with `code`, `msg`, `event`, `execution_id`, `generation`, `node_id`, `support_ref`, and the Agent's task text in no log line. | Seen failing first (`Internal RUNTIME_ERROR_CODE_V1_INTERNAL; model calls 0`), then passes |
| `a_saved_agent_frontier_node_is_named_only_for_a_saved_agent` | Named for a saved Agent; not for a frozen saved pipeline; not for an unknown node | pass |
| `an_agent_node_is_a_recovery_frontier_only_for_a_frozen_saved_pipeline` (existing, unchanged) | Admission unchanged | pass |
| `every_registered_runtime_failure_is_in_the_fault_registry_byte_identically`, `post_baseline_kinds_fall_back_to_a_baseline_kind_of_the_same_wire_code`, `the_fallback_is_substituted_at_most_once` (existing, table-driven) | Registry parity, one-hop same-code fallback | pass |
| `a_refused_new_kind_is_replaced_by_the_kind_an_older_main_admits` (existing, one case added) | The new kind is replaced once by `RecoveryFrontierUnsupported` | pass |
| Main `TestRecoveryFrontierAgentStepShowsItsReadableText`, `TestOutputServerAcceptsTheRecoveryFrontierAgentStepFrame` | Text admitted only under its code, untrusted suffix refused, registry byte-identical, frame accepted end to end | pass |
| Faults gate `go test ./libs/go/faults/...` | Registry well formed, generated files current, codes used, legacy list unchanged | pass |

## Performance

One extra pass over the pending nodes, only on a refused reclaim. No new I/O.

## Durability

Unchanged: the refusal happens before the run, writes nothing and repeats nothing.

## Resilience

The refusal is typed and readable, never retryable (the same evidence gives the same answer). An older Main gets the
generic frontier text through the one-hop fallback; the run still ends typed.

## Security

No admission change. The fault line carries ids only (`execution_id`, `generation`, `node_id`, `support_ref`); the
proof asserts the Agent's task text is absent from every log line. The user text is data-free (registry gate C7).

## Recovery rows

| Component × phase | Class | Enforced by | Proven by |
|---|---|---|---|
| Worker × root saved-Agent node, any phase (model call, tool call, nested pipeline tool), reclaimed | F typed `RECOVERY_FRONTIER_UNSUPPORTED` (was `Internal`, an untyped F) | `W/agents/session.rs:2271-2290`, `W/execution/native_agent_lifecycle.rs:1931-1937` | the two-process test above |
| Main × an older Main during rollout | F typed, generic frontier text | `older_main_fallback` (`W/protocol/output.rs:944`) | `a_refused_new_kind_is_replaced_by_the_kind_an_older_main_admits` |

Known gap: R is possible from the Agent's model checkpoint; FU-323 (HIGH, exit work next to FU-310).

## Real-browser evidence

Not run for this side fix (the orchestrating session decides). A Worker kill inside a root saved-Agent node, then a
reload, should show the new text with a support reference.

## Follow-ups

- **FU-323:** a root saved-Agent node as a recovery frontier (frontier gate, skip the child walk for a saved Agent,
  root re-entry through the model checkpoint, security review of the admission change).
