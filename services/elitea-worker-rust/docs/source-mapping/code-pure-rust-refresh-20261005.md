# Code pure Rust refresh, 2026-10-05

## Behavior and owners

The SDK SandboxClient supplies the selected state and dependency declaration to the Code node.
The Rust replatform prepares dependencies in a separate runtime and executes their immutable offline bundle.
This record distinguishes fresh execution from compiled-cache and recovery acceptance.

| Contract | Owner |
| --- | --- |
| Original preparation and execution intent | Worker `src/agents/graph/code_preparation.rs` |
| Native request envelope and package hydration | Runner `src/code_prepared_extensions.rs` and `src/lifecycle.rs` |
| Typed graph state and checkpoint lineage | Worker `src/agents/graph/code_state.rs` and `src/state/postgres_checkpointer.rs` |
| Measured compiler profile | `scripts/runtime/rust_compiled_release_manifest.py` |
| Profile admission and immutable startup pin | Main compiled snapshot authority and Worker runtime configuration |

## Fresh Docker execution

The execution image is `sha256:8c035f5e089ad11854fda506e4af975e3b5d79d7e1458bc2072957fef5711b4c`.
The preparation image remains `sha256:8c403798c060330cf9836a583ea5d803890b91387b0088f3e64f005c22b00024`.
Both images contain the same independently checked Rust wrapper.
Bootstrap disables only the pure Rust compiled profile while retaining the passing broker routes.
Shared schema 141 and AgentState schema 13 remain unchanged.

Saved pipeline 145/version 158 executes in persistent chat 816.
Execution `6f5763380e1c56c5ee93afeaf8c7a764` completes at generation 1 in about ten seconds.
The fixture uses CSV and futures dependencies, structs, a trait, asynchronous processing, and typed state.
Its output contains three accepted records, total 30, and groups alpha 19 and beta 11.
The browser retains one identical answer after reload.
Both original sandbox dispatches resolve and their jobs complete with exit code zero.
The native bundle binds the retained preparation image to the new execution image.

Pipeline state and node-attempt journals have different checkpoint threads.
Their save ordinals are local to the full graph key and can tie across threads.
Acceptance reads must select the pipeline definition, thread, capability, family, execution, and generation.
An unrelated empty node journal does not indicate missing pipeline state.

## Final catalog and compiled reuse

The owning manifest tool measures the new execution image against the genuine native bundle.
The measured profile is appended to the retained catalog without editing its measured fields.
Main, Worker, and Supervisor adopt catalog digest `c48a17d4d79087387f2a4e4092b7218c4aa9bd8fe0df6a77885f450d1f655bc8`.
Their service images remain unchanged; only the coordinated runtime material and catalog pin change.
Both database schema heads remain unchanged.

The same saved version runs twice more from persistent chat 816.
Cold execution `1942c40eff90dbf3bca141f739a84809` completes in about 73 seconds.
Warm execution `f67ec12c50f1f6a0f8db3b194d53d0b7` completes in about six seconds.
Both settle successfully at generation 1 and persist one exact typed result each.
The cache contains one ready descriptor bound to the new execution image.
Its key is `85501eacdfe80ddc5326c023212686bddff5ab7be2cd1187d738e3a9fbc04d27`.
The ledger records one verified compilation and two compiled executions.
All three descriptor-bearing dispatches agree with the published cache descriptor.
The cold run has a separate compile dispatch; the warm run does not create one.
All dispatches resolve, all compiled jobs confirm cleanup, and independent Docker inspection confirms that all three runtimes are removed.

The public acceptance receipt digest is `dcc011c7c217bd81d57431d2a16bb7c60cf16e62876c7e695221e4be646d078e`.
These local timings describe this fixture and cache state, not production throughput.

## Remaining acceptance

Fresh final-cohort restart recovery, acquisition outages, and current Kubernetes checks remain open.
The user moves all workspace integration to the [post-worker backlog](../wanted_feature.md#wf-01--code-workspaces) on 2026-10-05.
Start that work after the entire worker is complete and released; workspace acceptance does not block this sandbox PR.
Debug artifacts are committed, but deployed UI acceptance still misses their download cards.
The browser request selector and numeric runtime generation belong to different identity domains.
The follow-up source fix binds the frame and active response to the same browser selector.
Main retains validation of the separate numeric runtime generation in the artifact proof.
Six previously failing cases reproduce the mismatch before the fix.
The corrected five-suite filter passes 153 tests; full TypeScript and focused lint also pass.
Stale or missing browser selectors and conflicting runtime proofs remain rejected.
The follow-up Web image is deployed successfully.
Fresh editor execution `cf9ad06c51f3951e37a0ec38eeaa1ea2` completes in seven seconds and commits a 206-byte debug snapshot.
Persistent chat 819 completes in six seconds and retains one exact result after reload.
Its grouped Code rows still use `SubAgentAccordion` without the `ToolModal` path.
The next narrow UI correction connects that group to the existing artifact renderer.
Debug-disabled version 165 completes execution `e636e2c1772ffe9ee610c710ba9871a4` with zero debug artifacts and zero debug trace proofs.
The positive artifact preview matches the committed byte length and SHA-256. A downloaded file remains unconfirmed.
Source tests and previous-image proofs do not close these live acceptance gates or establish CI for unpushed changes.

## Grouped debug follow-up

The grouped renderer correction passes 119 tests and four focused download cases using the recorded public stream.
The corrected Web image deploys without changing Main, Worker or Supervisor.
Persistent chat 819 opens its stored snapshot modal after reload.
A fresh run completes in seven seconds, displays one exact result and opens its own snapshot card.
The browser reports no console errors or artifact access error after the explicit download action.
The automation download event does not return a saved file path; separate file inspection remains unconfirmed.
See the [UI source mapping](../../../../apps/elitea-web/docs/source-mapping/code-debug-ui-integration-20261005.md) for the identity and renderer ownership changes.
Working shared foundations remain in this delivery even when future graph features depend on them.
The workspace deferral changes acceptance scope; it does not remove working Code dependencies.
