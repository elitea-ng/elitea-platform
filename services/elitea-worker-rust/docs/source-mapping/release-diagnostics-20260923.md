# Release diagnostic information

## Scope

This change supplies release symbols and source lines for OBS-RUST-01.
It does not implement diagnostic capture or complete Gate 4.

## Behavioral reference

Current SDK revision: `966526e8334354366dd161b606d73fe8e204b850`.

`elitea_sdk/runtime/middleware/strategies.py::ExceptionContext.error_traceback` formats exception stack information.
`elitea_sdk/runtime/utils/EliteACallback.py::on_tool_error` displays exception information through its callback state.
These paths establish the need to identify a failure location.
The Rust implementation must not copy unrestricted exception text into browser responses.

## Rust implementation

`Cargo.toml` retains optimized release code with `debug = "line-tables-only"` and `strip = "none"`.
Function symbols and source lines remain in the shipped binary.
Line tables do not add local-variable debug information.
Thin LTO, optimization level, and panic policy remain unchanged.
No application schema or wire contract changes.

`Containerfile` checks the actual release binary for `.debug_line` and `.symtab` sections.
The runtime image receives that same binary without a later stripping step.
This keeps diagnostic information with its matching executable.

`src/diagnostics.rs` still controls crate-scoped logging and the redacted panic hook.
`src/agents/runtime.rs::NativeAgentRuntimeError` retains the upstream ADK error privately.
`src/execution/native_agent_lifecycle.rs` reports static upstream codes under the execution span.
These existing paths do not yet capture full stack or async span diagnostics.

## Remaining acceptance

- Verify symbolized stack capture in the optimized Linux worker.
- Preserve async span ancestry separately from synchronous stack frames.
- Bound capture size, frequency, and overhead.
- Keep payloads and credentials out of diagnostic fields.
- Verify useful public errors and operator correlation through the browser.

Release section checks prove packaging only.
They do not prove capture, redaction, or browser acceptance.

## Packaging verification

The Linux release image builds successfully with `cargo auditable build --locked --release`.
Both ELF section checks pass.
Image: `elitea-worker-rust:release-diagnostics-20260923`.
Image digest: `sha256:7ade52477d97f8bc117474ae51adf31dab362583973caa47943b8508740ab68c`.

`nm` finds worker-owned function symbols in the copied runtime binary.
`addr2line` resolves `postgres_session::record_session_result` to `src/state/postgres_session.rs:1142`.
The binary grows from 24,017,688 bytes to 139,242,512 bytes.
These are executable file sizes, not resident-memory measurements.

The reference image is `elitea-worker-rust:pipeline-recovery-20260923`.
The build context uses its source plus the two release-packaging changes.
Local evidence: `/private/tmp/elitea-release-symbol-proof/proof.json`.
Build log: `/private/tmp/elitea-release-diagnostics-build.log`.

The rehearsal deployment remains unchanged.
No browser test runs for this packaging-only slice.
Stack capture and browser error acceptance remain required before OBS-RUST-01 closes.
