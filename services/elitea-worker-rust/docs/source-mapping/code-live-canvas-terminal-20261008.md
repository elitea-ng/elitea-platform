# Code live canvas terminal events, 2026-10-08

## Behavior and source ownership

Chat848 settles as CANCELLED before user Code starts.
Live chat shows cancellation, but the canvas continues to show an active run.
Reloaded History retains the correct terminal state.
The durable failure event does not reach the canvas callback.

Current EliteaUI revision `ce05c72ccf489caf57513a34f3725eed0865e1eb` defines the expected canvas behavior.
Its `src/[fsd]/features/pipelines/flow-editor/lib/hooks/useRunEvent.hooks.js` clears active nodes and marks a stopped run.
The new implementation keeps that behavior and requires a durable terminal event with matching run identity.
Stop admission alone does not prove cancellation.

| Owner | Source or symbol | Change |
| --- | --- | --- |
| Main | `internal/application/output/runtime_failure.go` | Preserve the canonical code, safe message, and retry flag. |
| Web transport | `src/features/chat-messages/model/useChatStreamTransport.ts`, `onFailed` | Forward the failure through the existing callback before detachment. Bind the admitted response and generation. |
| Web bridge | `PipelineTestChat` → `ChatBox` → `useChatBoxSend` → `EditorPanel` → `FlowEditor` | Reuse the existing callback path. |
| Web event contract | `parseRunsByEvent.support.ts`, `RunSocketEvent` | Retain the optional canonical failure code. |
| Web canvas | `useRunEvent.ts`, `applyExecutionFailure` | Require matching response and generation. Set Stopped or Error and clear active state. |
| Regression | `useChatBoxSend.canvasTerminal.test.tsx` | Exercise real SSE transport, delayed Stop completion, stale identities, and both terminal codes. |

Web paths are relative to `apps/elitea-web/`. Main paths are relative to `services/elitea-main/`.
Saved cancelled-history rendering and author selection remain unchanged.

## Verification and limits

The four-file Vitest selection passes 75 unique tests, with zero failures and skips.
TypeScript, targeted lint with warnings denied, and whitespace checks return direct exit0.
Local Node is24.19.0. The package requires Node26.
These results prove local source behavior. Node26 CI and deployed browser verification remain required.
The tested source map digest is `78d80603468ba0a0066f3f239a904ee4bc1ab5428131944192b03c38a87550f5`.
The result digest is `3a26d752cf553133a85c87a722adc3124fcbb7d3978b75e8ec687947e3c47538`.

The later production image builds from exact source `ac6e0adead340f06a65398b1aa2991e3db8d6f41` with direct exit0.
Its image ID is `sha256:69bf9188bd69b46e3237945bd1cbbbb09f61fa084e90612fb4e7add784978402`.
Build provenance binds the shipping Node26 Alpine builder. Local regression tests remain on Node24.19.
The build receipt digest is `c85c6b4e72cfcb37385bcc7a78a28aa6aeacaf91aa2b47fbffcadeafbdbe6673`.
The source manifest digest is `113322ca3f848b2145d27b02d79f3d461e2ec0d9a7943fad8d90771bf768e960`.

The strict local Alpine3.24.2 scan reports zero HIGH and zero CRITICAL findings.
Image export, scan, and gate each return direct exit0.
The scan receipt digest is `36325d224e2aaf5439fb7549d07224be3a47a16b64b996e17cbb040bb56f789d`.
The report digest is `41378c787046eb5c6ea726cf0a29021683307b31469db14e0992bb124a7921b6`.
The verified archive configuration digest is `sha256:6f7e92f576bdcc1140e32ec3890e9efa2963df1e3b45bc12b12f70a6fb196975`.
The manifest, configuration, report, source labels, and exact built image match.

The serial build retains the authorized16GiB Docker budget and unchanged8GiB, one-CPU builder.
Source context checks pass before and after the build. The compiler returns idle.
The local scanner uses Trivy0.72. CI uses0.74 and remains a separate check.
The new image is not deployed. The running Web remains at its accepted42a0 source boundary.
Verify matching-run Stop and failure canvas states after deployment. Verify normal terminal History and reload separately.

The [deployed acceptance record](code-nats-deployed-acceptance-20261007.md) retains the original browser and cancellation evidence.
This correction does not close pending preparation recovery or any remaining service-loss gate.
