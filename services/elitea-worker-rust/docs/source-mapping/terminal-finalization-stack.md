# Terminal finalization stack bound

## Source mapping

Current SDK `projects/elitea-sdk/elitea_sdk/runtime/tools/application.py` creates the nested `AgentResponse` result.
Current worker `projects/centry/pylon_indexer/plugins/indexer_worker/utils/agent_execution_common.py` emits `agent_response` and `full_message`.
Current worker `methods/agent_common.py` also projects the final message.
The Rust worker owns these transitions in `src/execution/native_agent_lifecycle.rs`.
Main acknowledges the terminal frame before settlement and Redis retirement.
The current platform is a behavior reference for the final chat result.
Its Python stack layout does not define the Rust implementation.

## Earlier implementation

Earlier changes boxed terminal finalization and the post-start stream phase.
Historical macOS verification passed 59 output-delivery tests and 953 library tests.
Those results describe the earlier source and dependency cohort.
They do not establish the current Linux stack bound.

## October 6 correction

The PostgreSQL 18 CI job aborts in the explicit `sensitive-hitl-2mib-stack` thread.
The owning sensitive HITL regression uses fake control, progress, and Redis clients.
Its failing lifecycle path performs no PostgreSQL operation.

Caller boxing still constructs terminal-future temporaries inside the caller's generated poll frame.
The private `finish_after_stream` allocation factory now isolates construction behind `#[inline(never)]`.
Its `finish_after_stream_owned` async body remains byte-identical to the previous terminal body.
All five callers await the existing boxed phase through this factory.

The change preserves cancellation, lease checks, exact replay, settlement, and retirement order.
It retains one allocation per terminal phase.
It adds no task, queue, schema, retry, or thread-stack override.
The explicit 2 MiB test remains unchanged.

## Current evidence

Current-lock macOS ARM64 verification uses Rust 1.97.1, all features, and one Cargo job.
The unchanged 2 MiB regression passes.
All 67 output-delivery tests pass with no ignored tests and default test concurrency.
Current-lock strict Clippy passes for all targets and features with warnings denied.
No database environment or `RUST_MIN_STACK` override is supplied.
The lockfile remains unchanged.

Static ARM64 debug disassembly measures these three nested poll frames:

| Phase | Baseline bytes | Factory bytes |
| --- | ---: | ---: |
| Authorized preparation | 832,592 | 797,808 |
| Started stream | 741,568 | 637,248 |
| Terminal publication | 242,848 | 242,848 |
| Sum | 1,817,008 | 1,677,904 |

The measured reduction is 139,104 bytes, approximately 135.8 KiB.
The baseline artifact uses earlier locked dependencies; the factory artifact uses the current lockfile.
These measurements compare frame reservations, rather than total process stack usage.
The existing macOS debug linker emits its large `__eh_frame` warning.

Current focused checks:

```sh
CARGO_INCREMENTAL=0 cargo test --locked --offline --all-features -j1 --lib \
  execution::output_delivery_tests::sensitive_interrupt_is_the_acked_paused_hitl_terminal_and_skips_completion \
  -- --exact --nocapture
CARGO_INCREMENTAL=0 cargo test --locked --offline --all-features -j1 --lib \
  execution::output_delivery_tests:: -- --nocapture
cargo fmt -- --check
```

The output-delivery group was executed directly from the first command's current-lock test binary.
Linux CI must still verify the unchanged 2 MiB acceptance contract.
No Linux cache was verified, and no cold Linux build or shared database mutation was performed.
A stronger preparation-phase split remains deferred unless a subsequent observed failure requires it.
