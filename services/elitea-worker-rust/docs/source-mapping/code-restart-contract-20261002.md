# Code execution restart contract

## Ownership

Restart recovery is a product requirement for Main, Worker, and Supervisor.
Main does not own graph checkpoints.

| Component | Durable responsibility | Recovery requirement |
| --- | --- | --- |
| Main | Original execution admission, authority, commands, results, and browser projection | Restore the original execution and reconnect its observer. Do not create a replacement request. |
| Worker | Graph checkpoints, original node activations, selected state, and Code dispatch journal | Resume the saved frontier under the current fenced claim. Reconcile existing dispatches before creating effects. |
| Supervisor | Original sandbox identity, dispatch receipts, resource enforcement, and cleanup | Observe the original runtime. Reuse its terminal receipt and complete cleanup after recovery. |
| Browser | Observer and controls for the original response | Reconnect or restore the existing run. Clear and Restore do not submit execution requests. |

PostgreSQL retains durable state.
A transport interruption does not establish execution failure or permission to repeat work.
An uncertain sandbox completion requires reconciliation against its original receipt.
The failure message states that the job was not restarted.
This contract does not promise exactly-once external effects without an effect-specific durable receipt.

## Verified Docker boundaries

The [native browser acceptance record](code-native-browser-acceptance-20261002.md) records three separate immediate restart tests.
Each test interrupts one service during original JavaScript hydration.
The other services remain running.

| Restarted service | Original execution | Result |
| --- | --- | --- |
| Worker | `dd60a24c34234f58427bb42544d4bed5` | Eight original dispatches complete. One exact result arrives without resubmission. |
| Main | `8c7a6041dde36bdff0f6b2b96ee95f67` | The original runtime survives. Eight dispatches complete and one exact result arrives. |
| Supervisor | `b283d60230377092115caac22be1eb65` | The original runtime survives. Its receipt remains recoverable and cleanup completes. |

Each request retains its original execution, activation, runtime, and request digest.
All original runtime containers are absent after settlement.
Browser reload retains the expected results and removes Stop controls.
The fixture processes 20,000 records through Python, JavaScript, TypeScript, and Rust.

The [editor lifecycle record](pipeline-editor-test-lifecycle-20261002.md) verifies separate Stop, Clear, History, reload, and Restore behavior.
Empty cancelled Test runs retain their original history.
Fresh Test execution completes without an existing conversation pointer.

## Remaining proof

These tests cover one hydration boundary per restarted service.
They do not cover simultaneous service loss, acquisition-phase loss, dependency outage, or every graph frontier.
Native Kubernetes recovery and compiled cold/warm restart acceptance remain required.
Capacity testing remains separate from these local functional samples.
Point 5 remains open.
