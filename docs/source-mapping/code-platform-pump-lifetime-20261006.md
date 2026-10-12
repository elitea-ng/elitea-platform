# Code platform pump lifetime

## Source boundary

`code_attempt_remote.rs` owns one exact Code observation attempt.
`code_platform_drive.rs` owns its submission and broker futures.

Previously, each Submit RPC owned a separate broker future.
A Pending or retryable transport result ended that future.
The following one-second backoff had no broker pump.
A fast Busy response could repeatedly cancel a broker step before admission.

The corrected submission future contains the existing reconciliation loop.
One broker future runs during grant refresh, Submit, and backoff.
Both futures use the same current claim and absolute observation deadline.
The ordinary and compiled Submit branches retain their original selectors.

The earlier awaited finalization remains before hydration and observation.
It registers the exact immutable original intent and broker binding required by MainStep.
Later Submit refreshes return authority for that same immutable identity.
MainStep derives its own fresh broker grant from the current claim.
It does not depend on the concurrent refresh's returned execution grant.

A terminal RPC outcome stops the broker future before dispatch journal persistence.
The biased selection retains terminal receipt precedence when both futures are ready.
Cancellation, deadline, or broker refusal drops both futures.
No task runs after the owning observation ends.

Main authorization, Supervisor capacity, effect identities, and retry classes remain unchanged.
The change neither submits a new activation nor repeats a committed platform operation.

## Runtime evidence

The v5 Worker restart fixture uses execution `f7200bb11a4e9ff2578fa69b44377298`, generation 1.
Its original JS runtime and dispatch remain the same after Worker replacement.
The platform journal contains JS sequence 1, committed, and no sequence 2.
The restored Worker stream starts at `2026-10-06T15:17:16Z`.
The JS node journal records `authorization_denied` at `2026-10-06T15:17:45.067Z`.
The Supervisor execution records `sandbox.deadline_exceeded`.

The saved public logs contain no broker HTTP status or per-step transport trace.
They do not prove which particular broker request lost its observation lifetime.
The source establishes that nonterminal Submit results can cancel that lifetime.
The focused tests exercise this scheduling boundary with controlled futures.

Evidence resides under `/private/tmp/elitea-code-recovery-readiness-20261006/worker-only-operator-v5/failed-terminal-1/`.
The failed JS container remains stopped and present. Generic cleanup remains unproved.
The no-effect tombstone columns do not measure generic container cleanup.
This increment does not change or prove runtime cleanup.

## Verification boundary

Run the focused driver tests with the repository lockfile and existing test profile:

```sh
cd services/elitea-worker-rust
cargo test --locked --offline --all-features -j1 --lib agents::graph::code_remote::platform_drive::tests -- --nocapture
```

New cases cover fast Pending and Busy outcomes with one multistage broker step across backoff.
Other cases cover broker refusal during backoff and one absolute deadline across all retry waits.
A terminal receipt case verifies pump shutdown before journal finalization waits.
Existing cases cover parent cancellation and simultaneous terminal receipt precedence.

The final native macOS run passes 9 driver cases, 6 typed-attempt cases, and 1 broker wire case.
All 16 cases pass, with no failures or ignored cases.
The final focused Cargo command exits 0 in 45.327 seconds.
Strict offline Clippy for the library and tests exits 0 with warnings denied.
The Cargo lockfile remains unchanged.

The native linker reports the existing `__eh_frame` compact-unwind size warning.
The scheduling tests use controlled futures; they do not invoke the full remote execution method.
Source review verifies its wiring into the tested driver.
No Linux, database, container, or deployed transport test runs in this increment.

Native checks do not prove service replacement, deployed transport, or final runtime output.
Repeat the exact Worker-only acceptance after the corrected Worker image is deployed.
Require original runtime retention, sequence 2 commitment, one final output, and confirmed cleanup.
