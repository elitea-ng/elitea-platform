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

## Opt-in runner failure capture

`src/diagnostics/failure.rs` captures diagnostics when `ELITEA_RUST_FAILURE_DIAGNOSTICS=on`.
The default is `off`; other values fail startup validation.
Capture admits at most one failure per monotonic second across the process.
The diagnostic text contains at most 8,192 UTF-8 bytes and 32 async span names.

The implementation uses the existing tracing registry for active async ancestry.
It reads static worker span names only, never recorded field values.
`std::backtrace::Backtrace::force_capture` supplies the synchronous stack.
No new tracing framework, dependency, database table, or wire field is introduced.

`src/agents/runtime.rs::NativeAgentRuntimeError` captures at runner start and event failure conversion.
The owning lifecycle emits this detail with its existing execution span and upstream error code.
`Debug`, `Display`, and the public error projection do not expose the captured diagnostic.
The implementation never formats the upstream error message, details, or source chain.
Existing public errors and browser behavior remain unchanged.

The current-platform traceback references above provide the behavioral mapping for this slice.
The Rust implementation uses separate operator diagnostics instead of forwarding traceback text to users.

### Focused verification

Four capture tests pass: configuration, frequency limiting, UTF-8 bounds, and async ancestry after a task yield.
The async test includes secret sentinel values in span fields and confirms their exclusion.
Strict all-target Clippy passes.
The complete diagnostics tests and runner ownership tests also pass.

### Limits and next verification

Capture occurs at the runner boundary, not every original dependency failure location.
Async ancestry includes active, instrumented worker spans only.
The process frequency limit can suppress additional failures during a burst.
The retained error code and execution correlation still accompany suppressed captures.
The byte limit bounds rendered text; it does not bound native stack capture or symbolization time.
Capture remains disabled by default until release overhead and deployed behavior are measured.

Release capture, nested correlation, provider-specific causes, and browser error acceptance remain open.
This slice does not complete OBS-RUST-01.

## Deployed capture verification

The rehearsal worker runs source `49e8ab2e` with capture enabled.
Image ID: `sha256:8850e0c34e5b4a97b8421b2b839afb36b4ab6bf8cecbd3d6f86e042fc07502e5`.
The deployment retains all five mounts, networks, credentials, and resource limits.
No active execution claim exists before replacement.

Fresh headed Playwright chat 654 exercises a 256-token pipeline response with four continuation calls.
Execution: `b1b5fd112e4417db9d840332174ad601`.
The browser displays `OUTPUT_CONTINUATION_EXHAUSTED`, preserves the partial response after reload, and reports no page errors.
The screenshot confirms the visible error below the partial answer.

The worker emits one diagnostic with exactly 8,192 bytes.
It includes the lifecycle span, execution identity, and `model.output_continuation_failed` code.
The optimized stack resolves `capture_detail`, `event_failed`, and the runner boundary to Rust source lines.
The diagnostic does not contain the fixture's model output text.
This is a controlled payload check, not exhaustive redaction acceptance.

Local evidence:

- `/private/tmp/elitea-diagnostic-failure-result.json`
- `/private/tmp/elitea-diagnostic-failure-complete.png`
- `/private/tmp/elitea-capture-diagnostics-proof.json`

The first log scan misses the diagnostic because ANSI sequences interrupt the field name.
Normalized inspection confirms successful capture; there is no missing event capture in this test.
The follow-up removes ANSI formatting and renders diagnostic stack text with actual line breaks.
That formatting change requires the next image deployment.

## User and operator explanation requirements

The user requires understandable failure reasons, corrective actions, and operator context without source-code investigation.
`native_agent_lifecycle.rs::model_failure` still maps many distinct model errors to `Internal`.
Main's `runtimeFailurePolicy` enforces canonical messages for the current protocol categories.
A UI-only wording change cannot restore distinctions already lost upstream.
The next contract slice must preserve safe reasons across worker, Main, persistence, replay, and UI.
Raw provider text remains excluded from public messages.
