# Instruction authority and recovery

## Ownership

Main freezes authorized instruction content before dispatch. Rust uses this content without a product database lookup.
`AgentExecutionInputV1.project_context` uses field 64. Fields 40 through 63 remain reserved.
The input bundle protects the snapshot with the existing content digest and claim binding.

`instruction_authority.rs` admits the skill catalog and verifies each SHA-256 revision against the exact content bytes.
Each active instruction has an immutable ID and revision. Session state supplies its execution scope and initial run identity.
The same state stores the content snapshot. A summary cannot replace this authority.

The existing PostgreSQL session service persists instruction state through ordinary event state deltas.
The existing writer claim and lease fence protect these writes. This change adds no state table or storage service.

## Source mapping

| Current source | Native owner | Preserved behavior |
| --- | --- | --- |
| SDK `elitea_sdk/runtime/tools/skill_tools.py:31-166` | `src/agents/instruction_authority.rs` | Attached skills form the loadable catalog. `load_skill` uses the `skill` argument. Name matching ignores case and outer spaces. |
| SDK `elitea_sdk/runtime/langchain/constants.py:746-780` | `InstructionState::render` and `InstructionTool` | Activation applies exact skill instructions within their execution scope. Unknown names do not activate a source. |
| SDK `elitea_sdk/runtime/middleware/project_context.py:17-100` | `InstructionPlan::add_project_context` and `InstructionTool` | Project Context supports eager delivery and description-driven delivery through `read_project_context`. |
| Main `internal/application/agentexecution/skills.go` | `InstructionPlan::admit` | Invoked and applied skills activate catalog entries. Selected skills remain available for loading. |
| Main `internal/application/agentexecution/instruction_snapshots.go` | `src/agents/protocol.rs`, `src/agents/request.rs` | Root inputs carry typed Project Context. Nested version details carry the same snapshot fields. |
| `src/agents/context_management.rs` | `InstructionPlan::bind_builder` | Transcript compaction keeps its current policy. Each model call receives fresh instruction text from authoritative state. |
| `src/agents/session.rs` | `InstructionAgent` | Initial activation is persisted before model execution. Continuations retain the previous catalog and active revisions. |
| `src/agents/application_tools.rs` | Child session state and event forwarding | Child instruction scope derives from the parent session, exact tool call, and agent identity. Sibling activations remain separate. |
| `src/agents/graph/llm.rs` | `PipelineLlmReplayEnvelope.instruction_state` | Private graph checkpoints retain exact instruction state across a paused model-tool loop. |
| `src/agents/pipeline.rs` | Native node factory | Root nodes inherit their pipeline authority. Nested pipelines use their own scoped catalog. |
| `src/transport/openai_compatible_facade.rs` | System message projection | Restored instructions use the provider's system or developer role. |
| `src/transport/anthropic_facade.rs` | System block projection | Restored instructions join Anthropic system blocks. They do not become user messages. |
| `src/agents/events.rs` | Applied skill metadata | Browser events expose skill identity and revision. Public application details omit instruction bodies and Project Context snapshots. |

SDK paths refer to the SDK repository. Main paths refer to `services/elitea-main`.
Rust paths refer to `services/elitea-worker-rust`.

## History and decisions

- SDK commit `96b4dd6` introduces progressive skill disclosure through `load_skill`.
- SDK commit `041e504` introduces Project Context delivery on demand.
- Platform commit `edd69d3b` renames the Main skill projection source without changing its ownership.
- Platform commit `92b73bad` contains the prior native context-management baseline.
- Before this change, native assembly rejects invoked, applied, attached, and version skills.
- This change admits immutable snapshots and adds native loading tools.

The SDK reconstructs loaded skill names from tool-message history. This change deliberately replaces that recovery rule.
Native recovery uses structured state even when all old transcript content is absent.
An independent user turn can select new revisions. A continuation keeps the original revisions, including during process replacement.
Missing required continuation state fails admission to model execution. Corrupt state never falls back to the current product revision.

Pipeline nodes share one catalog inside their graph thread. Nested graph threads have separate catalogs.
The private replay envelope includes the same instruction state. No instruction content is placed on shared Redis streams.
The current SDK excludes Project Context from pipeline input. Main preserves that selection rule.

## Bounds

The catalog admits at most 128 sources. Each source admits at most 256 KiB of content.
Total catalog content admits at most 1 MiB. Existing session-event and private checkpoint bounds remain active.
Unknown skill names leave state unchanged. Conflicting IDs, revisions, or normalized names fail admission.
Client transcript input still rejects the system role. Only the native instruction boundary creates restored system content.

## Verification

`instruction_authority_tests.rs` covers activation, duplicate IDs, name collisions, missing state, source scope, and revision mutation.
It also covers eager Project Context, description-driven availability, independent-turn reset, and child catalog isolation.
The state-only service reload test removes every transcript event before recovery.
That test is component evidence. It is not process replacement evidence.

`ordinary_tests.rs` exercises application and ad-hoc loading through native Runner, provider projection, and browser event projection.
Its nested test loads a child skill and verifies that parent and child system instructions remain separate.
`pipeline_tests.rs` mutates an invoked skill after a private pause and verifies the original revision on continuation.
Its saved-pipeline test also checks that parent instructions do not enter the nested pipeline model.
The Anthropic transport test verifies restored system blocks. Assembly tests reject forged system transcript entries.

The PostgreSQL replacement test starts two independent test processes against one isolated database.
The first process loads a skill and persists a confirmation pause. That process exits before the second process starts.
The second process acquires the next writer authority and reads the original instruction revision from PostgreSQL.
The replacement input contains changed content. The model still receives the original content.
The fixture creates and removes its database. It does not use the deployed product database.

This proves session persistence across process replacement. It does not prove browser reconnection or deployment failover.
The private pipeline continuation test separately proves same-call checkpoint resume. It uses an in-process checkpoint service.

Run the focused checks from the repository root:

```sh
cargo test --manifest-path services/elitea-worker-rust/Cargo.toml --locked --offline --lib agents::
cargo test --manifest-path services/elitea-worker-rust/Cargo.toml --locked --offline --lib authoritative_instruction_content
cargo clippy --manifest-path services/elitea-worker-rust/Cargo.toml --locked --offline --lib -- -D warnings
```

Set `ELITEA_TEST_DATABASE_URL` to an approved test database endpoint for the PostgreSQL replacement test.
Without that variable, the test reports its missing prerequisite. A skipped run is not replacement evidence.

## Delivery and checkpoint ordering, 2026-09-14

This delivery follows Main contract commit `c6782160` and protects the previously preserved Rust implementation.
It also includes the already rehearsed persona behavior mapped in [chat personas](chat-personas.md).
It does not include Main's deferred direct pipeline history work or built-in module catalogue widening.

An added regression finds that the ordinary model checkpoint is written before instruction rehydration.
The provider receives the authoritative instructions, but the pending request snapshot omits them.
`session.rs::build_runtime_agent` now installs instruction rehydration before checkpoint persistence.
The checkpoint therefore contains the prepared dynamic instructions before the model call.
Static agent instructions remain separately bound by the provider adapter and the immutable definition.

Older checkpoints have no system content because they precede this callback.
`model_checkpoint.rs::ModelCheckpointWriter::prepare_request` fills that old shape from the restored instruction state.
Client history cannot supply system content. A checkpoint that already contains prepared system content remains exact.
The schema, claim checks, tool declarations, and generation fencing remain unchanged.

The regression first fails at the missing stored instruction assertion.
After the ordering correction, it verifies the stored request and equal initial/recovered provider messages.
A separate compatibility test covers old and prepared checkpoint shapes with exactly one authoritative instruction block.
The final agent suite passes 313 test entries with no ignored tests and the PostgreSQL endpoint supplied.
The process-replacement parent test starts separate write and read fixture processes against one disposable database.
Clippy passes with warnings denied. Rustfmt passes after formatting two existing test blocks.
The focused Anthropic instruction-system-block test also passes with no ignored cases.
Logs: `elitea-point4-prepared-instructions-regression.log`, `elitea-point4-rust-authority-tests-final.log`, and `elitea-point4-rust-authority-clippy-final.log`.

This is component and PostgreSQL process-replacement evidence.
The new checkpoint ordering has no deployed browser acceptance yet.
Full-request budgets, durable summaries, provider measurements, and UI/Main context wiring remain gate 4 requirements.
