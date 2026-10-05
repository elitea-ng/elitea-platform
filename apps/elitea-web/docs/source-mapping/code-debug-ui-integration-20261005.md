# Code debug UI integration, 2026-10-05

The UI adopts the frozen artifact renderer and the Code-only Debug switch.
The current Main contract remains the source authority.
The UI keeps the existing generation-bound final-result observer.

| Current source authority | UI owner | Result |
| --- | --- | --- |
| `services/elitea-main/internal/infra/db/repos/agent_code_debug.go` | `src/shared/lib/codeDebugArtifact.ts` | Validate both metadata copies, the Code node, original visit, execution, generation and attempt. |
| `services/elitea-main/internal/infra/db/repos/agent_code_trace.go` | `src/shared/lib/codeDebugArtifact.ts` | Retain the execution, generation and node grammar. |
| `services/elitea-main/internal/domain/codesandbox/contract.go` | `src/shared/lib/codeDebugArtifact.ts` | Validate the exact original visit reference. |
| `services/elitea-main/internal/infra/storage/runtime_code_debug.go` | `src/shared/lib/codeDebugArtifact.ts` | Accept the versioned artifact reference with the 3 MiB maximum. |
| `services/elitea-main/internal/api/v2/artifacts/objects.go` | `src/shared/api/artifacts.ts` | Read through the current artifact route and project authorization. |
| `libs/proto/elitea/runtime/v1/code_debug_artifact_v1.md` | `src/shared/lib/readCodeDebugArtifact.ts` | Verify the raw byte length and SHA-256 before download. |
| Current Code YAML and `updateYamlNode` | `src/features/pipelines/ui/settings/CodeDebugSettings.tsx` | Write the Debug value only after an explicit edit. |

The Debug switch preserves omitted, false and existing stored values on mount.
The switch preserves source bytes, sibling nodes and other Code controls.
Running or read-only Code cards disable the switch.
The help text explains executable source and selected input state export.

The renderer accepts only matching public metadata copies and Code node identity.
The download project comes from the selected project context.
The reader rejects a foreign project or invalid reference before any request.
The renderer reads only after an explicit click.
The artifact route enforces current access permissions.
The UI treats trace metadata as a content reference, without new access rights.

The bounded reader uses one buffer capped at the reference byte length.
The reader cancels a larger stream, including streams with a missing or false Content-Length.
Denied responses are cancelled without reading their private error bodies.
The UI verifies exact raw bytes before it creates a download.
Changing project, scope or visit aborts a pending read.
Closing the modal or removing the renderer also aborts the read.

Live tool modals, restored tool modals, run history and persisted trace details use the same renderer.
Run history and persisted details require the selected trace row and message group to match.
Retry warnings cannot fall back to an earlier artifact.
No source, selected input, credential, grant or private error is rendered from trace metadata.
Denied, missing or changed artifacts show safe user guidance.

The frozen renderer had four unrun tests.
Integration corrects its adapter fixture type and lint defects.
Integration also adds bounded response reads and English locale strings.
The Code switch adoption omits unrelated recovery controls from the combined packet.

Focused tests cover parser bounds, artifact access failures, raw byte verification and cancellation.
Focused tests also cover all trace adapters, ordinary tools, the Debug switch and Code YAML roundtrips.
Full TypeScript and focused lint checks run after integration.
These checks do not prove deployment or browser acceptance.
The parent task owns the build, deployment and browser acceptance.

## Grouped trace correction and deployed acceptance

Worker `src/agents/graph/code_debug.rs` emits the attempt-bound export receipt.
Main validates and projects that receipt through its existing Code trace contract.
`chatStreamToolFrames.ts` now keeps the browser request selector separate from the numeric runtime generation.
Stale selectors and conflicting runtime proofs remain rejected.

`ApplicationAnswerThinking` groups Code rows by the original node name.
`SubAgentAccordion.tsx` previously renders those rows as text with an optional callback.
The caller supplies no callback, so clicking the export cannot open its modal.
The correction uses the existing `ActionView` and `ToolModal` for Code debug receipts inside the group.
Ordinary grouped rows retain their existing renderer.
Project authorization, bounded reads and byte verification stay in their existing owners.

The exact 18-frame public replay reproduces the missing modal before the correction.
Four UI tests cover editor chat, main chat, restored grouped traces and persisted detail fallback.
They check explicit artifact reads and verified Blob downloads with the recorded bytes.
Ten regression suites pass 119 tests; the final focused suite passes four tests.
TypeScript, focused lint and whitespace checks pass.

The corrected Web image is built and deployed in Docker rehearsal.
Main, Worker, Supervisor and their contracts remain unchanged.
Persistent chat 819 opens the stored attempt's snapshot modal after reload.
A fresh run returns one exact typed result in seven seconds and opens its new snapshot modal.
Its receipt records 205 bytes and SHA-256 `e547f31aaf9959cec7a63cfda4ce8f9afb4296102c8079ee00cd4f0cff00489b`.
The download action shows no artifact error; the browser reports no console errors.
The browser automation download event does not expose a saved file path.
This live check proves the card and action, not a separately inspected downloaded file.
The earlier debug-disabled run retains zero exports and zero debug trace proofs.
Unpushed source checks and rehearsal proof do not establish CI for the PR head.
