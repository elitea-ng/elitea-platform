# Exclusive pipeline branches and ADK fan-in joins

Branch `fix/graph-exclusive-branch-fan-in`, 2026-10-08. A saved child pipeline whose exclusive branches meet at one
node now runs that node. Before this change, the child silently ended without it, and the parent read an empty
child result.

## Business behaviour

The current platform runs stored pipelines on LangGraph. A node reached by several `transition` edges runs whenever
the branch that was actually taken arrives. Router, Decision and HITL nodes pick one branch, so exclusive branches
continue to their shared next node, to `END`, or back to an earlier node in a loop.

The new platform keeps that behaviour for root and saved-child pipelines. It does not port anything else: there is
no defect in the current platform to carry over.

## Defect

ADK-Rust 2.2.0 `StateGraph::compile` runs `defer_unconditional_fan_in`
(`adk-graph-2.2.0/src/graph.rs:216`). Any node reached by two or more direct edges becomes a wait-for-all join. Its
tracker waits for every upstream source, direct and conditional (`get_upstream_nodes`, `graph.rs:759`;
`filter_deferred_nodes`, `executor.rs:727`). A branch that was never taken never arrives. The executor then drains
an empty frontier and completes the run without running the join.

A saved child is compiled through that path (`compile_subgraph_with_runtime`). Every `END` transition is redirected
to the compiler-owned result node `__elitea_subgraph_result_v1`. Three shapes were affected:

| Child shape | Before | Evidence on `main` |
|---|---|---|
| Two branches end the child (`transition: END`) | The result node never runs. `elitea_response` stays `""`, and the parent answers `Pipeline completed.` | `saved_child_with_two_terminal_branches_…`: `elitea_response` `""`. `a_parent_receives_…_both_end`: `{"response":"Pipeline completed."}` |
| Two branches meet at one shared node | The shared node and every later node never run | `saved_child_branches_converging_on_a_shared_node_…`: `final_text` `""` |
| A loop transitions back to the entry node | Nothing runs: the entry node is also reached from the compiler-owned entry node | `saved_child_loop_back_to_its_entry_node_continues`: `final_text` `""` |

**Root graph: not affected (verified, not assumed).** Root pipelines are built through `GraphAgentBuilder::build`,
which replaces the compiled fan-in map with the builder's own, empty, map (`adk-graph-2.2.0/src/agent.rs:750`).
The three root tests pass on `main` and on this branch. They stay as a guard, because an ADK upgrade that changes
that line would expose root pipelines to the same defect.

## Change

| Path | Change |
|---|---|
| `src/agents/graph/compiler.rs:503-547` | `exclusive_transitions` counts direct and entry arrivals per node. Each direct edge into a node with more than one arrival becomes a single-route conditional edge to the same target, which ADK never joins. All other edges are unchanged. |
| `src/agents/graph/compiler.rs:1260` | `compile_subgraph_with_runtime` applies it before `compile()`. |
| `src/agents/graph/fan_in_tests.rs` | Six tests: three saved-child shapes and the same three at the root. They compile real YAML and run with `MemoryCheckpointer`. |
| `src/agents/application_pipeline_tests.rs:1347,1364` | Two parent→child tests through the real `SubgraphNode` and pipeline tool. The parent maps no child variable, so it reads only the child's projected `elitea_response`. |

**Why this is correct.** A stored pipeline advances one node at a time (`max_concurrency(1)`), and each node has at
most one transition. Router, Decision and HITL nodes use `goto`, which replaces their edges (`executor.rs:1172`). So
of several transitions into one node, only one can arrive in a pass. A conditional edge whose router always returns
its single target queues exactly the node the direct edge queued (`get_next_nodes`, `graph.rs:655`).

**Parallel and Map.** Their join semantics are unchanged. Each compiles to one durable node that joins its branches
internally (`parallel_compiler.rs:218`, `map_compiler.rs`). Their own inner graphs (`START → node → END`) do not
pass through this function.

**ADK.** ADK is not patched. `adk-graph` is not vendored (`libs/rust/vendor/` holds only `adk-agent`, `adk-runner`,
`adk-sandbox` and `leiden-rs`).

## Tests

- **Red on `main`.** The three saved-child tests in `fan_in_tests.rs` fail. Both parent tests fail with
  `{"response":"Pipeline completed."}` instead of `LEFT` / `LEFT DONE`. This was checked by running them against
  `origin/main`'s `compiler.rs`.
- **Green on this branch.** All 8 new tests pass. The root tests pass both before and after the change.
- **Full suite.** `cargo test --locked --all-targets --all-features` against a disposable `postgres:18` container,
  with `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`:
  - 2,122 passed, 0 failed, 63 ignored (library: 2,027 passed, 63 ignored);
  - this ran at `9402c9b5`; the later commit adds two parent tests and a clone simplification, and the new tests
    were re-run on it.
  - `ELITEA_REQUIRE_NATS_SECURE_TEST` was not set locally, so the secured-NATS permission test did not use a
    provisioned server. CI provisions it.
- `cargo clippy --locked --all-targets --all-features -- -D warnings` and `cargo fmt --all -- --check` are clean.

## Performance

**Mechanism** (`compiler.rs:512`):
- one pass over the edge list at compile time, with one `HashMap` of arrival counts;
- one in-place rewrite per converted edge;
- no I/O, no allocation on edges that are not converted.

**Runtime cost.** A converted edge evaluates a constant router, which clones one node-name `String` per traversal,
instead of the direct-edge match. There are no new database round trips, transactions or events.

**Result.** The 8 new tests finish in 0.02 s. No benchmark was run, and none is claimed: the change adds no
per-step I/O.

## Durability

**Unchanged.**
- No state channel, migration, checkpoint field or checkpoint format changes.
- `pending_nodes` holds the same node names. ADK keeps fan-in trackers in memory only (`executor.rs:57`), so no
  persisted tracker state is orphaned.

**Effect on existing checkpoints.**
- A saved-child checkpoint written by an older Worker before a converging transition resumes on this Worker and
  runs the shared node. Proven indirectly: the frontier is computed from the same edges after load.
- A child that already completed wrongly on an older Worker keeps its terminal (empty-frontier) checkpoint and is
  not re-run.

## Resilience

**Fixed.** A silent wrong success: a child reported completion without running its remaining nodes, and the parent
continued with `Pipeline completed.` or `""`.

**Bounds are unchanged.** Loops are bounded by `PIPELINE_RECURSION_LIMIT`. The rewrite cannot create fan-out,
because it keeps exactly one target per edge.

## Security

**Security review (2026-10-08).** No finding meets the >80% confidence bar.
- **Reachability.** The rewrite adds no edge, node or target: each conditional edge returns the target its direct
  edge already named, and ADK `validate()` still checks every target.
- **Effect controls run inside the node.** A node that now runs was always declared in the same saved definition.
  Its own checks run inside `execute` and do not depend on scheduling:
  - the node-recovery journal;
  - the direct-tool and application resolvers;
  - the scoped checkpointer;
  - static pauses, keyed by node name.
- **Boundaries unchanged.** Parent/child isolation, scope path, identity and channels are not touched.
- **Less state exposure.** The removed join also stops ADK writing a `<node>_fan_in` key built from upstream state.

**Dependency audits.**
- `cargo deny --all-features check advisories` (cargo-deny 0.20.2) reports only the findings already on `main`:
  - RUSTSEC-2026-0258 (`h2`);
  - RUSTSEC-2023-0071 (`rsa`);
  - one yanked `chacha20`.
- This PR changes no dependency and does not touch `Cargo.lock`.

**Evidence handling.** This record contains no secret. Fixture YAML is synthetic.

## Recovery guarantees

This change fixes a correctness defect inside phase P12 (nested agent or pipeline child). It does not change a
crash-recovery class.

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × P12 | R/F¹, unchanged (`docs/recovery-guarantees.md:279`) | Child threads stay fenced per thread. The converted edges reproduce the same frontier after a checkpoint load (`compiler.rs:512`). | `fan_in_tests.rs:143,164,184`; `application_pipeline_tests.rs:1347,1364` |
| Main × P12 | R/F¹, unchanged | No Main change | — |
| PostgreSQL × P12 | R, unchanged | No checkpoint format change | Full PG18 suite above |

Before the fix, the affected child shapes effectively lost the child's result while reporting success. That was a
correctness gap rather than a crash-recovery class. It is now closed.

## Browser evidence

**Stack.** A dedicated local standalone stack, project `elitea-fanin`, at `http://fanin.localhost:18150`. It uses a
real backend with no response mocks, NATS, PG18, and the offline mock LLM gateway.
- The network uses a pinned subnet, `10.251.7.0/24`, because Docker's default pools were exhausted by other local
  stacks.
- Signed in as the seeded fixture user `e2e-admin@autotest.local` through the stack's local OIDC emulator.

**Images.** All images were built from this branch. Only the Worker differs from `origin/main` `7ac0eaf9`.
- `ghcr.io/elitea-ng/elitea-worker-rust:fanin-20261008`: `sha256:0e24c948c44b…`, rebuilt from a clean tree at
  `ec9214cb`; this is the running container's image.
- **Binary identity.** Worker image builds share the BuildKit `/cargo-target` cache mount across worktrees, so an
  image can ship another worktree's binary. Checked with `docker create` + `docker cp` of
  `/usr/local/bin/elitea-worker-rust` (SHA-256 `e1b3d3a91ea17a3c…`) and `strings`: it contains
  `elitea_worker_rust::agents::graph::compiler::exclusive_transitions` (9 matches; the release profile keeps
  symbols with `strip = "none"`). Only this branch defines that function. The browser results are also behavioural
  proof: on `main` the loop-to-entry child runs nothing, so the parent could not answer `LEFT via ONCE`.
- `ghcr.io/elitea-ng/elitea-main:fanin-20261008`: `sha256:1151aa62b349…`.
- `ghcr.io/elitea-ng/elitea-web:fanin-20261008`: `sha256:8746736ed35f…`.

**Fixtures.** All were created in the UI, in Default Project (project 1).
- Pipeline `fanin-child` (application 1, version 1) is a combined shape:
  - `gate` is a router and the entry point;
  - `mark` loops back to `gate`;
  - `left` and `right` meet at the shared node `finish`;
  - `finish` and `plain` both transition to `END`.
- Pipeline `fanin-parent` (application 2, version 2) has one agent node, `tool: fanin-child`, with
  `output: [reply]`. The child was attached through the editor's Tools → Pipeline picker.
- Both definitions were entered in the editor's YAML tab through its paste handler, using a scripted paste event
  (typing is re-indented by the editor), and saved with the editor's Save button. The stored versions were then
  checked in `p_1.application_versions`.
- An earlier attempt in the personal project (90501) was refused by Main at start, with "the requested model is not
  in the project's catalog". `seed-llm` does not seed personal projects, so the fixtures moved to project 1.

| Message | Path in the child | Chat 1 answer | After reload |
|---|---|---|---|
| `left` | entry → `mark` → loop to `gate` → `left` → shared `finish` → result node | `LEFT via ONCE` | same |
| `plain` | entry → `mark` → loop to `gate` → `plain` → result node (second `END` branch) | `PLAIN via ONCE` | same |
| `right` | entry → `mark` → loop to `gate` → `right` → shared `finish` → result node | `RIGHT via ONCE` | same |

Execution ids are `5c253352…` (`left`), `5e93e770…` (`plain`) and `ae606de1…` (`right`). The answers persist in
`p_1.chat_message_trace_step` (message groups 2, 4 and 6).

**Re-run on the merged tree (2026-10-09).** The branch was merged with `main`, including the shared build-cache fix
from PR #1160, at `18a2b849`. Worker, Main and Web were rebuilt from that head, one or two images at a time, and the
same stack and fixtures were reused.
- Images:
  - Worker `sha256:b0acbdab15ae…`;
  - Main `sha256:5d1fc27d6d6a…`;
  - Web `sha256:d4664e1e6088…`.
- The binary guard was repeated. The Worker binary (SHA-256 `857881bc0ba28eee…`) contains
  `compiler::exclusive_transitions` (5 matches) and the new `elitea_agent_runtime` crate's symbols.
- In chat 1, `plain`, `left` and `right` answered `PLAIN via ONCE`, `LEFT via ONCE` and `RIGHT via ONCE`. The
  answers are message groups 8, 10 and 12, execution ids `88a46ecc…`, `767a46fe…` and `86dab414…`.
- All six turns were identical after reload.
- The full Worker suite on the merged tree (`--offline`, PG18): 1,680 passed, 0 failed, 63 ignored. The count is
  lower because `main` moved tests into `elitea-agent-runtime`.

A negative control on a `main` Worker image was not run in the browser. The red tests above are the before-fix
evidence.

## Follow-ups

1. **Child tool card text.** The `fanin-child` tool card shows a fixed `{"response":"Pipeline completed."}`.
   `PipelineNodeEventSender::send_application_end_scoped` (`src/agents/graph/node_events.rs:339`) always sends that
   text, even though the parent received the real child result. This is pre-existing and not caused by this change.
   The card should carry the child's projected result.
2. **Root depends on an ADK detail.** Root pipelines avoid the join only because `GraphAgentBuilder::build` replaces
   the compiled fan-in map (`adk-graph-2.2.0/src/agent.rs:750`).
   - The root tests in `fan_in_tests.rs` fail if an ADK upgrade changes that.
   - `GraphAgentBuilder::conditional_edge` needs `&'static` targets, so the same rewrite cannot be applied there
     without leaking strings.
3. **Pause test.** No test pauses a saved child at a static `interrupt_after` (for example a Printer) on a branch
   that converges, and then resumes it.
4. **Model resolution for zero-LLM pipelines.** Main resolves a model at start even for a pipeline with no LLM node.
   It refuses the run in a project without a model catalog ("This agent turn requires the current execution
   path."). This is pre-existing.
