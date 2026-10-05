# Native dependency delivery: Docker browser acceptance

## Source and deployment

The source owners remain the Code graph journal, Main's existing authenticated
bundle store, the shared Supervisor ledger, and the language-owned preparation
and execution adapters. This acceptance introduces no alternative executor.
The current-platform reference and native contracts are mapped in
[JavaScript delivery](code-javascript-native-delivery-20261002.md) and
[Cargo delivery](code-cargo-retained-profile-20261002.md).

The corrected Deno image is
`sha256:6a54c8b5b9a9d94a008f34f551d718ecde5e44c9033e860dc52d9bfa608b1bdd`.
Only its JavaScript preparation marker writer differs from the previous image.
Python and Cargo image choices remain unchanged. The existing Main stays healthy.
All seven Supervisor listeners and the two Worker delivery consumers start after
the targeted configuration rollout. All 29 helper preservation checks pass.

## Actual product acceptance

Playwright submits `{"rows":20000,"seed":42}` to saved pipeline 143, version 150.
The pipeline runs Python, JavaScript, TypeScript, and Rust sequentially, with
selected state passed between nodes. It acquires dependencies through the real
preparation routes, publishes them through Main, hydrates isolated execution
workloads, and executes offline. There are no LLM nodes in this fixture.

| Browser path | Execution | Result |
| --- | --- | --- |
| Persistent chat 785 | `30d476ba5963fd6c07348dcfb2276473` | Exact result; one output remains after browser reload. |
| Pipeline editor Test chat | `dcf153be12b9da149bd96012e1a15de3` | Exact result; Stop control disappears at completion. |

Both results contain 18,947 accepted and 1,053 rejected records, 824 refunds,
866,440,800 total cents, and weighted total 8,662,764,360,486. The five category
totals and normalized merchant names match the independent fixture.
The exact result digest is
`6a3c4d063a5ba0c21949cf7d67d4bda843cb50a0bc76169c19e2c5f7e272326d`.

The agent-state database contains exactly eight resolved dispatches per request:
four preparation jobs and four execution jobs. Verification derives each ledger
key from the original execution and activation using the existing signed-grant
domain. It checks request digests and Completed receipts rather than joining by
source digest, which could include historical identical-content jobs.
All 16 original runtime containers are absent after settlement.
The earlier failed JavaScript activation and its original container remain
preserved; the successful requests do not rewrite that failure.

The local preparation/execution receipt spans are 63.203 and 62.630 seconds.
The persistent request takes 154.277 seconds from admission to settlement;
approximately 91 seconds precede its first sandbox reservation. The editor
request takes 63.499 seconds from admission to settlement. These are two local
rehearsal samples, not capacity or production latency claims.
JavaScript and TypeScript hydration and transfer dominate their measured spans;
their user-code processing takes approximately 75–154 milliseconds in the
persistent request. Rust user-code processing takes 17 milliseconds, separately
from its compilation and sandbox lifecycle.

## Worker restart acceptance

A separate browser request uses execution `dd60a24c34234f58427bb42544d4bed5`.
Root immediately restarts the Worker while its original native JavaScript runtime is reserved for hydration.
The Supervisor and Main remain running.
Recovery retains the original runtime `9a751b462adbb70593a41f9b38f5f809131528eb2c62ad69ee9049644294de43` and request digest.
The request completes with exactly eight resolved preparation and execution dispatches.
All eight runtimes are removed after settlement.
The browser receives one new exact result without manual resubmission.
Reload retains the three expected successful request outputs and shows no Stop control.

Admission occurs at 15:44:19.141 UTC and settlement occurs at 15:46:21.584865 UTC.
This sample takes 122.444 seconds, including claim recovery.
An earlier timing-control request completes normally; its disruption guard refuses to restart an already completed runtime.
That request adds a distinct expected result, not a duplicate recovery result.
This proves this Worker boundary only.

## Main and Supervisor restart acceptance

Two further requests use the same saved four-language pipeline and input.
Root immediately restarts one service while the original JavaScript runtime is reserved for hydration.
The other services remain running.

| Restarted service | Original execution | Original JavaScript runtime | Admission to settlement |
| --- | --- | --- | --- |
| Main | `8c7a6041dde36bdff0f6b2b96ee95f67` | `17afcc17482873d4e9403ccc62bf6996e39a43592af3b56fb11c32c077204fa1` | 125.325 seconds |
| Supervisor | `b283d60230377092115caac22be1eb65` | `c7c4effdb69c513e808d1f861b91ac9ce27377eda7481b0e1335602fa24c0874` | 216.534 seconds |

Both requests retain their original execution, activation, runtime, and request digest.
Each request has exactly eight resolved dispatches and successful terminal receipts.
All 16 runtimes are absent after settlement.
The historical failed JavaScript container remains preserved.
The open browser receives each new result without resubmission.
Reload retains five successful outputs: the original request, timing control, and three restart requests.
Each output has the exact expected digest and 20,000 input rows.
No Stop control remains after settlement.

These tests prove immediate process restart during one native hydration boundary.
They do not prove simultaneous service loss, dependency outage, or every graph checkpoint boundary.
The measured delays include intake and recovery; they are local samples, not service-level guarantees.

## Required remaining acceptance

The replacement editor Test context adds a separate early-Stop acceptance run.
It restores the original running execution after reload and sends no replacement start request.
Stop cancels the original response; all five dispatched sandbox jobs resolve and their containers are removed.
The [editor lifecycle record](pipeline-editor-test-lifecycle-20261002.md) records the exact execution and remaining History defect.

These Docker runs do not close acquisition-phase loss,
maximum graph capacity, or Kubernetes native delivery.
They do not prove later-request requirements lookup reuses a prior frozen bundle
or compiled executable. The editor lifecycle replacement and static/nested graph
assembly also require their own deployed browser acceptance.
The deployed History replacement shows the successful Test duration and excludes ordinary-chat conversations.
Code trace production remains undeployed.
The Main retention and browser refresh corrections pass early Stop and fresh History reads after reload.
The fresh Test correction passes deployed first use, Clear, reload, and Restore.
Context `789` executes all four languages with eight completed original dispatches and exact result verification.
Clear and Restore submit no replacement execution.
The next focus creates a separate context with no admitted run.
This local sample takes 60.321 seconds; it is not a capacity claim.
Point 5 remains open.
