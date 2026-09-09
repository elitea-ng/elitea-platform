# Terminal finalization stack bound

## Source mapping

Current SDK `projects/elitea-sdk/elitea_sdk/runtime/tools/application.py` creates the nested `AgentResponse` result.
Current worker `projects/centry/pylon_indexer/plugins/indexer_worker/utils/agent_execution_common.py` emits `agent_response` and `full_message`.
Current worker `methods/agent_common.py` also projects the final message.
The Rust worker owns these transitions in `src/execution/native_agent_lifecycle.rs`.
Main acknowledges the terminal frame before settlement and Redis retirement.
The current platform is a behavior reference for the final chat result.
Its Python stack layout does not define the Rust implementation.

## Implementation

The shared `finalize` function allocates its `finish_after_stream` future on the heap.
Previously, each caller embeds that large future in its own generated async state.
Several failure branches repeat this allocation footprint in the debug poll frame.
The successful path also retains that poll frame while terminal publication runs.
This causes a stack overflow on the normal test thread.

The change uses the existing `Box::pin` pattern at one shared phase boundary.
It keeps cancellation, lease checks, exact replay, settlement, and retirement in the same order.
It adds no task, queue, schema, or thread-stack override.

## Evidence

The application and ad-hoc lifecycle regression fails before this change with stack overflow.
The macOS crash report identifies terminal publication beneath the active lifecycle poll frames.
Debug disassembly shows approximately 1 MiB in `execute_owned` and 588 KiB in `execute_started` before correction.
The test passes with the boxed finalization phase and the normal stack configuration.
The final minimal patch passes all 59 output-delivery tests with no ignored tests.
