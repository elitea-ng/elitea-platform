# Saved child pipeline variables

## Current-platform evidence

`projects/elitea-sdk/elitea_sdk/runtime/tools/application.py::_run` merges saved defaults with invocation variables.
It keeps each reconstructed child runnable local to its invocation.
This prevents one sibling invocation from replacing another sibling's variables.
The current implementation also returns child state for further result projection.

## Rust implementation

| Contract | Source |
| --- | --- |
| Parse task and additional fixed, variable, or fstring mappings | `src/agents/graph/application.rs::ApplicationNodeDefinition` |
| Expose declared child variable types | `src/agents/graph/compiler.rs::PipelineDefinition::declared_variable_types` |
| Bind the frozen child graph and its schema | `src/agents/pipeline.rs` |
| Validate targets and values before child execution | `src/agents/graph/application.rs::ApplicationNode` |
| Keep explicit input channels isolated | Native ADK `SubgraphNode` input mappings |
| Persist child values and defaults | Existing ADK child graph checkpoints |

Additional mappings preserve JSON types instead of converting every value to text.
Targets must exist in the saved child pipeline schema.
Invalid values fail before child execution and before downstream state publication.
Omitted mappings retain child defaults.
Explicit null fails validation for the currently supported non-nullable types.
This differs from the legacy rule that ignores null when a default exists.

Only declared parent user state and the task input can supply extra mappings.
Runtime control fields and message history cannot become child variable targets.
Internal mapping channels include the parent node identity.
They do not overwrite parent user variables.
The mapped payload retains the existing 240 KiB bound.
Task-only configuration digests remain unchanged.

This change does not support extra variables for ordinary Agent children yet.
Such configurations fail explicitly during binding.
Typed child output projection remains separate work.
No application database schema changes are required.

## Numeric template correction

The integration test exposes an existing serialization mismatch in `state_modifier.rs`.
With arbitrary-precision JSON enabled, numeric serialization exposes a private map to MiniJinja.
The template adapter now converts numeric primitives explicitly, including nested values and the `from_json` filter.
Numbers outside finite primitive range remain text rather than exposing the private serialization map.
MiniJinja retains its standard boolean rendering, including `True`.

## Verification

All 117 focused graph tests pass.
They check mapped values, retained defaults, type rejection, unknown targets, and reserved-field rejection.
It also checks nested HITL checkpoint identity and resume behavior.
The numeric regression checks integer arithmetic, nested decimal arithmetic, and parsed JSON arithmetic.

Formatting, diff checks, and Clippy with tests and warnings denied pass.
Deployment and successful browser acceptance for child variable mappings are recorded below.
Existing sandbox browser acceptance does not prove this new contract.

## Browser integration correction

The first browser attempt finds an obsolete UI admission rule that permits only the `task` mapping.
`graphAdmission.nodes.ts` now permits up to 64 mappings, requires `task`, and rejects reserved child targets.
`graphAdmission.nodeReads.ts` now validates every variable mapping against the parent state.
`runtimeContract.constants.ts` reserves the worker's internal child-variable channel prefix.
The runtime still validates the selected child's schema and types during binding and execution.
The UI does not claim schema validation for an unfetched child definition.

All 64 focused admission and node-default tests pass. Typechecking, focused lint, and the application build pass.
The worker image is deployed at `sha256:b66ac7dd17fdbc74c12dc312147c2650342a02c560a3bca497b1cf30bd8954f8`.
The matching rehearsal UI is `elitea-web:child-inputs-20260930`.

The rehearsal restart also exposes an incorrect Minikube example in the deployment guide.
Use the supported `kubelet.pod-max-pids` flag, not the configuration key as a flag.
The corrected node returns Ready and reports an effective `podPidsLimit` of 128.

## Deployed browser acceptance

Persistent chat 761 runs saved parent 136/version 143 with child 135/version 142.
It returns `3|for orders|2|True`: fixed integer, rendered label, mapped two-item list,
and the untouched child boolean default. Reload retains exactly one result.
The graph has no LLM node, so the selected model is not invoked by this check.

The ephemeral pipeline test rejects `count: wrong-type` and produces no child result.
Focused worker tests additionally prove rejection before a child checkpoint is created.
The deployed error message is still generic (`The runtime operation failed.`).
`application.rs` maps the typed validation failure into `node_failure`, losing its cause;
this diagnostic correction remains open and this negative browser check is not complete
acceptance of error quality. The valid parent fixture is restored and saved afterwards.

The association picker leaves the selected child visible after successful attachment.
Closing it reveals the attached child card; this is a separate UI polish issue, not a
failed relation write. The flow editor currently exposes only task mapping controls;
additional child mappings are authored through YAML.

## Child-input diagnostic correction

The new worker now preserves `pipeline.child_input_type_invalid` through the existing
`PipelineNodeEventSender` failure channel instead of reducing it to `agent.legacy`.
The public policy reuses the existing `PIPELINE_INPUT_INVALID` wire code with a
specific, registered message explaining the wrong type, the mapping to inspect,
and the fact that the child and later nodes did not run. Error restoration recognizes
this exact policy, including after persistence. No protocol or database migration is needed.
An ERROR log records the node, input key, and expected type, without recording the value.

This follows the existing direct-tool failure-channel pattern in `graph/direct_tool.rs`.
The current-platform behavioral source remains the application input mapping described above;
the typed public failure and its durability are replatform implementation details.
Tests cover graph rejection before child execution, event-stream reason propagation, and
canonical persisted error restoration. Deployment and browser verification of the improved
message remain pending; the earlier browser failure proves only the stopping behavior.

Verification: 59 focused child tests pass. After extracting the validation helper, both
new error-channel/restoration tests pass again. Clippy with tests and warnings denied,
formatting, and diff checks pass.

## Typed child output projection

Current SDK `elitea_sdk/runtime/tools/function.py`, in its FunctionTool output projection,
preserves a child pipeline's computed declared output fields and falls back to final text
when a field is absent or null. An ordinary Agent uses its final response instead of
propagating stale parent fields. This behavior is the reference, not its broad state copying.

The Rust `ApplicationNode::pipeline_subgraph` now uses ADK's isolated, explicit output
mapping for parent-declared fields present in the child schema. `project_response` validates
every resulting value against the parent's declared types before returning any updates.
Undeclared child fields do not escape. Missing/null fields retain the text fallback, and
ordinary Agent behavior remains unchanged. Output-size bounds still apply to the complete
projected update. A rejected projection sends the existing `pipeline.result_invalid` event
and applies no partial parent update; the child itself may already have completed.

All 15 focused application graph tests pass, including typed integer/list outputs, stale
parent-value replacement, undeclared-field isolation, text fallback, and incompatible parent
output types. The negative checkpoint test confirms the child completed while the failed
first parent node wrote no partial parent checkpoint. Clippy with tests and warnings denied
passes. Typed output deployment/browser acceptance remains open.

## Main terminal-admission compatibility

The combined worker image deploys at
`sha256:8ae04bb922fd7954e505108178ba0e7b93b55ddf72c305b4377f5299ce1f2e18`.
The first negative persistent-chat check exposes a missing cross-service registration:
Main's `runtimegrpc/output/server.go::runtimeFailurePolicyForError` rejects the new safe
message despite its existing wire error code. Worker logs retain the exact cause but terminal
publication is rejected, leaving chat 763 running. This is not UI acceptance.

Main now admits only the exact child-input message under `PIPELINE_INPUT_INVALID`, retaining
the old direct-tool message. Arbitrary details and the same text under another code remain
rejected. The output transport package tests pass. Coordinated deployment and recovery of the
rejected terminal remain pending. This registration requires no schema migration.

Main image `elitea-main:child-input-20260930` deploys with the allowlist correction.
The retained execution `64332add9ef92b3dbc9e6a3e03dd0838` settles from its existing terminal
checkpoint (`agent_delivery.checkpoint_terminal_retired`) after the update. It does not
rerun the child. Chat 763 displays the actionable message after reload. Operator ERROR logs
identify `delegate`, input `count`, and expected `int` without publishing its value.
The Main rehearsal binary preserves the pre-existing working-tree fixes already used by the
rehearsal; those unrelated edits are not included in this commit.

## Typed output browser acceptance

Persistent chat 764 runs saved parent 136/version 143 with child 135. The parent
explicitly projects `answer`, integer `count`, and list `items`, then a downstream
State Modifier evaluates integer addition and list length. The visible result is
`4|2|3|for orders|2|True`, proving the child fields remain typed rather than becoming
the child's final response string. Reload retains exactly one result. This fixture
has no LLM node; the selected model is not used and this is not model-generation proof.

The check uses the deployed worker digest above and Main's registered-message fix.
The earlier invalid-input fixture is restored to valid inputs for this test. Fresh
streaming of the invalid-input error and typed-output ephemeral chat acceptance are
not claimed by this check. Ordinary Agent per-call variable binding and richer mapping
controls remain open; Gate 5 is not complete.

Deployment simplification for Docker Compose and Kubernetes follows functional
development. Docker remains the primary development target; hybrid Kubernetes evidence
does not replace later full Kubernetes deployment acceptance.

## Ordinary Agent call variables

Current SDK `elitea_sdk/runtime/tools/application.py:354-412` keeps the rebuilt
runnable local to one invocation, merges stored defaults with non-null keyword
arguments, and passes the resulting variables to `client.application`. The new
worker follows this ownership contract without rebuilding toolsets or refetching
saved versions on every call.

`variables.rs::AgentCallVariables` freezes declared names (including empty default
placeholders) and stored defaults. The saved-agent tool advertises optional string
or null properties for those names. Unknown names and non-string values are
rejected, null keeps the default, and an explicit empty string replaces it. The
string contract matches the platform's authored VersionVariable fields; pipeline
state mappings themselves continue to support typed JSON for pipeline children.
Names reserved for `task` and `chat_history` cannot replace invocation controls.

`ApplicationAgentTool::invoke_child` renders the original template on a local
`LazyNestedAgent`/profile clone. The shared saved agent, model settings, toolsets,
skill/project-context authority, and context-management policy are unchanged.
Both fresh execution and `prepare_resume` use that local agent. Existing durable
call arguments retain overrides and are compared exactly during resume; no new
checkpoint owner, database field, or schema migration is introduced. The complete
call arguments are bounded at 240 KiB before rendering.

`graph/application.rs` admits additional Agent-node mappings only when the selected
saved tool declares those properties and sends the evaluated values with `task`.
This exposes the same contract to graph calls and model tool loops. Additional
mapping authoring still uses YAML; richer controls remain a UI gap.

Focused variable tests cover empty declarations, override precedence, null/default
behavior, explicit empty strings, invalid values and independent sibling bindings.
Live model/UI acceptance and a disruption test with overridden variables remain
required before claiming this Agent extension accepted.

The application-focused suite passes 63 tests and variable-rendering suite passes
16 tests. A gateway-fixture integration test also observes two actual outgoing
provider requests from consecutive calls to the same saved Agent: the first uses
`Audience=operators; tone=brief`, while the second uses the stored
`Audience=users; tone=formal`. It proves prompt binding and no cross-call leakage
at the provider boundary, not a live external-model or crash-recovery result.
Task-only invocations retain the existing shared immutable base-agent fast path.

The broader agent unit/component suite passes all 541 tests after integration.
These tests use their configured local doubles and do not establish live deployment
acceptance. The existing `task` fixed/f-string/variable semantics are unchanged;
this extension addresses additional declared child-Agent instruction variables.

## Ordinary Agent deployed browser evidence

Commit `d9245333a` deploys as worker manifest
`sha256:467e12bee3a0612a751aa5e89881352933a276de5f3f4cab836a8b67a98d9918`;
Ready pod image identity is read back. Saved Agent 137/version 144 uses the real
`eu.anthropic.claude-haiku` model, instruction placeholders `audience` and `tone`,
and defaults `users`/`formal`. Parent pipeline 138/version 145 invokes it twice:
first with a variable mapping for `audience=operators` and fixed `tone=brief`,
then with no overrides and an f-string task. A State Modifier combines the outputs.

Persistent chat 765 displays two generated jokes with the exact headers
`Audience=operators; tone=brief` and `Audience=users; tone=formal`. Reload preserves
one combined answer. Execution `a946824a3bbfeab69c27bf8469774f88` completes both
Agent nodes and retires normally. The parent UI still shows its unused fixture
model; the child owns the explicitly configured real Haiku model. This is live
provider/UI proof of the variable override/default contract, not disruption proof.

The pipeline editor's ephemeral Test chat also completes both real Haiku calls and
shows the same override/default headers. No ephemeral reload persistence is claimed.
Clippy with tests and warnings denied passes. A worker-disruption/resume check with
variable overrides remains open; these successful runs do not establish recovery.

## Parallel clarification resume regression

`ordinary_tests.rs::parallel_child_variables_survive_clarification_resume_independently`
checks two calls to the same saved Agent with different `audience` overrides.
Both children pause for clarification. A partial decision set cannot resume either child.
The complete decision set resumes each exact child without replanning its call.
Captured provider requests retain each child's override before and after resume.
Neither child uses the saved default or the other child's override.

All 21 tests selected by `cargo test --lib parallel_ --jobs 2` pass.
These tests use local provider fixtures and the same process.
They do not prove worker replacement, live clarification controls, or database recovery.
The existing task-only clarification test also passes.

## Child clarification routing correction

Persistent chat 765 exposes a failed answer after worker replacement.
The saved graph checkpoint and nested confirmation resolve correctly in a recorded-event probe.
The persisted continuation request contains the correct interrupt identity but omits `tool_call_id`.
The previous UI path preserves that field only when `childThreadId` exists.
A saved Agent node can pause within the parent pipeline thread.
Without the tool identity, Rust selects the static HITL-node resolver and rejects the decision.

`apps/elitea-web/src/widgets/chat-box/ui/hooks/useChatBoxHandlers.hitl.ts` now retains explicit tool decisions on the root thread.
Static HITL nodes retain their existing root-action contract.
No checkpoint ownership, database schema, or authorization rule changes.

The current SDK reference remains `runtime/tools/application.py` for invocation-local child state.
The new Rust regression is `agents/pipeline_tests.rs::pipeline_agent_variable_call_resumes_clarification`.
It verifies resumed child instructions retain the invocation override instead of the saved default.
All 44 UI handler tests, UI typechecking, and the application build pass.
The focused Rust regression, formatting, and Clippy with warnings denied pass.

The rebuilt UI `elitea-web:ask-resume-20260930` passes persistent-chat acceptance in chat 765.
The first child accepts the selected debugging topic and returns its joke.
The second child pauses independently and accepts the free-text topic `Database indexes` after worker replacement.
Worker UID changes from `84907700-439f-407d-8631-ca90c9a2beb9` to `fbf6e83f-093b-4292-aa5d-cef633153f4e`.
The final report retains both results and the separate `operators/brief` and `users/formal` instruction values.
Browser reload retains the same report without duplicate results.
This verifies sequential child clarification recovery. It does not close broader Gate 5 composition or dependency installation.
