# Code indexed hydration, 2026-10-05

## Observed failure

The four-language broker test completes Python and commits two platform calls.
JavaScript stops during indexed dependency hydration, before user code dispatch.
The original JavaScript row remains Reserved without whole-Code or broker bindings.

The container helper requires raw request revision 3.
Platform-enabled execution uses revision 5. Workspace execution uses revision 4.
Both extensions can carry native dependencies with underlying revision 3.
The existing extension parser validates these envelopes without removing signed fields.

The Worker finalizes the original signed Execute intent before hydration.
The hydration request previously omitted that intent.
Supervisor therefore provisions the original runtime before registering its immutable platform binding.

## Source ownership

| Behavior | Owning source |
| --- | --- |
| Indexed hydration wire contract | `libs/proto/elitea/runtime/v1/sandbox.proto` |
| Exact original intent and immutable broker binding | `libs/proto/contracts/sandbox-code-platform-owner-v1.md` |
| Fresh intent for each indexed transfer | `src/agents/graph/code_preparation.rs` |
| Signed intent transport | `src/sandbox/client_hydration.rs` |
| Intent verification before runtime provisioning | `src/sandbox/service.rs` |
| Native envelope compatibility | `../elitea-code-runner/src/lifecycle.rs` |
| Validated extension parser | `../elitea-code-runner/src/code_prepared_extensions.rs` |

## Compatibility and authority

Hydration field 16 carries the refreshed original signed intent.
Field numbers 6 through 15 remain reserved.
Supervisor checks the peer, grant, original dispatch, scope, and exact final PreparedJob.
Supervisor registers whole-Code and broker bindings before provisioning the inert runtime.
Platform-enabled hydration requires the signed intent. Legacy plain hydration can omit it.
Hydration does not dispatch user code or create a new execution identity.

Native Execute hydration uses the validated underlying dependency revision.
Preparation revision checks, content bounds, bundle identity, and runtime identity checks remain enforced.

## Current-platform reference

The SDK SandboxClient provides the Code-node input, state, and language behavior reference.
Indexed package hydration and signed runtime admission are new replatform contracts.
The current platform does not provide equivalent checkpoint recovery across Worker and Supervisor restarts.

## Verification limits

Protobuf generation and Buf compatibility checks pass.
Thirty focused Runner lifecycle tests pass. No tests fail or skip.
Seventy-six Worker and Supervisor checks pass, including signed hydration and local transport.
No focused tests fail or skip; none are ignored. Native linking and formatting pass.
Fresh Docker browser acceptance passes for the refreshed broker cohort.
Earlier Python completion proof applies to the preceding deployment.
The remaining Code acceptance gates remain open.

The narrow deployment refreshes Deno execution images and the Rust broker execution image.
Main's pure Rust compiled catalog stays unchanged.
Pure Rust workspace requests still select the previous measured Runner image.
Their cold hydration and cached execution require separate acceptance.
Refreshing that route requires new measured image profiles and Main's startup catalog pin.

## Deployed acceptance

The reviewed deployment changes only Worker, Supervisor, and selected execution image references.
Main and Web retain their existing images and instances.
Shared schema 141 and AgentState schema 13 remain unchanged.
All three earlier terminal fixture records remain identical through service replacement.
The earlier containers and configuration volumes remain retained.

Editor version 163 executes Python, JavaScript, TypeScript, and Rust in 22 seconds.
Execution `4df77ec9a23164759beb86f1fcb65e3b` succeeds at generation 1.
Persistent chat 814 executes the same version in 19 seconds.
Execution `bd23fbb5c1532ad8da56a7f886cba996` succeeds at generation 1.
The chat retains one identical final result after browser reload without regeneration.
Each execution commits exactly eight platform calls: `user_get` and `application_list` once per language.
Four whole-Code bindings, four broker bindings, all resolved dispatches, and original runtime cleanup pass independent receipt checks.
These durations describe this local fixture and its cache state, not a production performance guarantee.

## Remaining limits

These runs do not exercise crash injection, acquisition outage, preparer destruction, or workspace authorization.
Current Kubernetes acceptance and the pure Rust compiled-profile image update remain required.
The UI relabels this pipeline as an Agent after version selection.
Execution still resolves the saved pipeline version correctly; the label discrepancy remains a separate UI issue.
