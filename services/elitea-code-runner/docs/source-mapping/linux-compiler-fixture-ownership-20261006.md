# Linux compiler fixture ownership

The Linux compiler cleanup owns every child of the runtime process. Compiler tests require the same sole-owner boundary.

Parallel fixtures previously shared one Rust test process. Compiler cleanup scanned every task's child list. It could kill another fixture's Cargo or shell child.

## Source mapping

|Source|Behavior|
|---|---|
|`src/compiled_compiler.rs::drain_adopted_children`|Preserve process-wide adopted-child cleanup and its resource bounds.|
|`src/native_finalization.rs::OwnedGroup`|Preserve process-group ownership, subreaper setup, deadline cleanup, and retry lock.|
|`src/compiled_compiler.rs::tests::isolated_test`|Run one exact compiler fixture in a child test process. Bound the parent wait and own child cleanup.|
|`src/compiled_compiler.rs::tests::compiler_fixture_preserves_another_process_owner`|Prove compiler cleanup leaves another parent-owned child alive.|
|`src/rust_native_tests.rs`|Preserve fake-Cargo hydration, profile checks, and original binding assertions.|
|`src/compiled_snapshot.rs::tests`|Preserve the real offline cold-build and inert capture/import proof.|

Both original compiler fixtures keep their setsid, failure, and reaping assertions. Other fixtures retain parallel execution. No production environment, request contract, grant, or cleanup policy changes.

Run locked, offline Linux tests with one Cargo job and two test threads. Record the non-root container, toolchain, target, and direct exit. Local macOS checks do not prove Linux descendant termination.

The deterministic Linux control proves the earlier cleanup can reap another thread's child with `ECHILD`.
The ordinary unchanged baseline passes in the observed schedule; it does not reproduce every CI failure.
The corrected source passes 155 outer tests with zero failures and two existing ignored instances.
The ignored native-Cargo preparation test requires network access and a fresh Cargo home.
No new ignores are added.

Rust 1.97.1 Linux ARM64 strict Clippy passes for every target and feature with warnings denied.
The disposable tooling image adds only the matching Clippy component to the pinned Rust image.
Verification runs offline as UID 10001, with one CPU, 2 GiB memory, and two test threads.
The source and public registry remain read-only; writable caches belong to this fixture.
Linux AMD64 CI remains a separate acceptance gate.
