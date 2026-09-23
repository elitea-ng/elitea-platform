# Full context capacity

## Evidence

Full-mode chat 596 contains 1,407,395 bytes of fictional history.
Main rejects this request before worker admission with `Invalid agent execution request`.
The previous agent input bound is 1 MiB. Model token capacity does not determine this bound.

## Source mapping

This change implements the new context policy. It does not copy legacy compaction.
Main `internal/application/agentexecution/input_bundle.go` stores the authoritative input.
Main `internal/domain/execution/model.go` defines its byte ceiling.
Rust `agents/protocol.rs` and `protocol/control.rs` enforce the corresponding input contract.
Rust `transport/input_content.rs` fetches this content after claim authorization.
Rust `transport/openai_compatible_facade.rs` bounds the prepared provider request.
Rust `state/postgres_session.rs` stores native ADK session and recovery state.

## Implementation

Agent input and provider request ceilings increase to 8 MiB.
This permits a 4 MiB text fixture plus serialization overhead. Model token checks still apply.
Session state permits 8 MiB. Events permit 16 MiB for combined state and content.
Application and user shared state retain their 1 MiB bounds.
Configuration and direct toolkit input bounds remain unchanged.
Shared migration 0132 and agentstate migration 0003 change runtime constraints only.
No application table changes are required.
Deploy database constraints and Main consumers before the worker producer.

## Outstanding verification

The isolated rehearsal stack now passes the 400k Full-window browser case.
Live process recovery, repeated compaction, and one-million-token capacity remain open.
The 64 MiB retained event limit still needs a separate retention design.
Do not treat larger per-record capacity as unlimited long-run durability.
No live one-million-token model is available in the current test catalogue.

## Initial checks

Main input-bundle tests pass, including an exact 4 MiB history round trip.
PostgreSQL session tests pass with migration 0003 installed in isolated databases.
A 4 MiB checkpoint also survives session-writer replacement. Exact replay stores one event.
Oversized shared app/user updates fail without partial state.
This is a component test, not a live process-crash test.
Protocol generation and whitespace checks pass.

## Rehearsal deployment

Only shared migration 0132 and agentstate migration 0003 were pending. Both applied successfully.
Main image: `sha256:3941189c7bc5f3a74749f8e54263b9aec073ced081774a30f3df95956b38dcdc`.
Worker image: `sha256:a139a8c36538d3b9b5330fa9b654027ae39a25de47f2447c195a42b1ff787529`.
A fresh headed browser submits chat 596 successfully after deployment.
Execution `f5e331881b558a736156d766dcc6bf75` starts compaction at 353,853 estimated input tokens.
Its Full window is 400,000 tokens. Usable input is 387,808 tokens.
Compaction completes at 40,356 estimated input tokens, approximately 10% of usable input.
The delivery code, corrected color, completed check, and next step survive.
The answer remains unchanged after reload. No browser page errors occur.
The browser uses real provider calls without mocked responses.
This proves one Full-mode compaction, not repeated compaction or process recovery.

## Repeated and child component checks

A deterministic test completes three compactions with a one-million-token Full policy.
Each cycle adds sixteen model records, approximately 3.36 MB of new work.
The output reservation is 128,000 tokens. The bounded safety margin is 8,192 tokens.
Usable input is 863,808 tokens. Every prepared request falls below the 15% target.
Exact pinned authority, the current user correction, and the final tool response remain intact.
The test uses a summary fixture. It does not prove live provider quality or native token counting.

The PostgreSQL compaction suite passes after the capacity migrations.
Its recovery test starts separate write, resume, and read processes.
The child-scope suite also passes against PostgreSQL.
It checks sibling history isolation, independent summary reuse, and root-takeover fencing.
These checks do not replace live nested-agent and repeated-compaction acceptance.

## Live restart case: open failure

Fresh browser chat 597 uses the same fictional Full-window history.
Execution `dbc0a1770933bdbfa96b6f4062310201` enters compaction at 353,853 estimated input tokens.
The fixture is the sole active claim when the worker receives an immediate restart.
Claim 1 expires. Claim 2 starts at 18:24:45 UTC and resumes compaction.
The recovered summary fails `context_summary_evidence`, including its bounded correction attempt.
The browser receives `The runtime operation failed.` No successful answer is recorded.
The test fails. Do not count it as accepted live compaction recovery.

The claim history proves takeover. It does not prove successful summary continuation.
The rejected summary text is not persisted, so the exact invalid evidence link remains unknown.
Next, inspect evidence-link correction and improve bounded diagnostics without logging conversation content.

## Constrained evidence repair candidate

The existing correction prompt requests valid evidence links, but its schema accepts arbitrary strings.
The failed live case shows that one correction can still leave invalid links.
The candidate constrains correction evidence strings to the previously validated reference values through the provider schema.
An empty reference set permits only empty evidence arrays.
The summary model accepts only this exact schema refinement. Other model configuration overrides remain prohibited.
Final source and contract validation still runs for compatible providers that ignore schema constraints.
The original summary supplies all facts. Correction copies only evidence arrays and requires unchanged completed-work identities.
This prevents reordered or rewritten results from receiving another result's evidence links.
Normal summary generation continues through the native ADK event summarizer.
Evidence repair uses the same authorized summary model, output reservation, and call budget.

OpenAI-compatible and native Anthropic wire tests verify the exact evidence enum.
The summary boundary rejects unrelated configuration overrides.
Eighteen compaction checks pass, including PostgreSQL process replacement.
Live acceptance of this repair remains outstanding. Do not treat the earlier restart case as passing.

## Repair restart retest: still open

Worker image `sha256:de9fdae53e3055868ca7d5f7f5fec20cfffced5e18ef031946b37f5f989ff8fb` runs the constrained repair candidate.
Chat 598 restarts during compaction. Execution `cf861b957e09dc34c38c528d6f4fbbd9` receives a second claim.
The recovered summary again fails `context_summary_evidence` after correction.
Provider wire tests pass, but this live result does not establish that the gateway enforces the schema.
The candidate remains unaccepted and uncommitted.
Add content-free invalid-link and label-match counts before choosing another repair strategy.

## Diagnostic retest and source evidence normalization

Chat 599 uses worker image `sha256:cbaca985d4700d76c4b3e0271e1b84af4e95ccc1459588f6487c524981304f58`.
Execution `8d764c3178ee499f49749029de08cfd5` takes over after restart, but fails source-reference validation after correction.
This remains a failed recovery acceptance case.
A local conversion probe preserves the strict evidence enum through pinned Bifrost OpenAI and Azure request conversion.
A direct synthetic provider probe reproduces exact source citations missing from the generated reference catalogue.
A second provider call with the constrained evidence schema returns valid links.
These probes do not establish the exact response from the failed worker calls.

The candidate registers missing evidence citations only when they occur verbatim in the original source.
It preserves completed-work text and links. It neither invents evidence nor accepts the candidate as its own source.
Reference-count and serialized-summary bounds apply after normalization. Repeated validation produces the same record.
Unknown links and invented references still fail validation. Native schema correction remains available for invalid links.
The prompt now distinguishes short source citations from paraphrased conclusions and invented message labels.

The direct all-history probe also receives a downstream 272,000-input-token rejection for the selected model route.
This differs from the catalogue's 400,000-token context window and needs a separate capacity-contract check.
Do not claim that the catalogue value proves the downstream route admits every request within that window.

The protocol and Rust `agents/context_budget.rs` already support optional `max_input_tokens`.
Main `application/configurations/models.go` does not expose this field in `CurrentModelCatalogItem`.
Main `application/agentexecution/model_context_limits.go` therefore does not freeze an input-only ceiling from the catalogue.
Wire explicit provider input limits through the authorized catalogue before claiming full capacity-contract coverage.
Do not infer an input-only ceiling by subtracting the maximum output from every model's context window.

## Source-evidence recovery acceptance

Fresh headed-browser chat 600 passes on 2026-09-21 with real provider calls and no mocked responses.
Worker image: `sha256:2ae83437f2610414229967ea8018b82610dad0d03170451ee2b23972c22b181f`.
Execution `5ba2d4cc1f66cd0ca8ae97798219e0a3` restarts during compaction and resumes under a replacement claim.
Estimated input falls from 353,853 to 40,661 tokens, approximately 10% of the displayed usable input.
The final answer preserves CEDAR-731, teal, completed archive verification, and the pending handoff note.
The answer remains identical after browser reload. No browser page errors occur.
Nine summary validation tests, ten evidence-focused checks, and eighteen PostgreSQL compaction checks pass.
Clippy passes for all targets after extracting the source-evidence registration helper without changing its behavior.

This proves successful recovery for this fixture. It does not close the input-only catalogue limit gap.
The test selects `gpt-5.4-mini` and explicitly limits its response to 8,192 tokens.
The displayed output reservation is that configured response limit, not the model's maximum output capability.
Live repeated compaction and nested-agent acceptance remain open.
