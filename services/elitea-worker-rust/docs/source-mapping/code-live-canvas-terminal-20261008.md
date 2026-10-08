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
These results prove local source behavior. Node26 CI, a shipping image, and deployed browser verification remain required.
The tested source map digest is `78d80603468ba0a0066f3f239a904ee4bc1ab5428131944192b03c38a87550f5`.
The result digest is `3a26d752cf553133a85c87a722adc3124fcbb7d3978b75e8ec687947e3c47538`.

The [deployed acceptance record](code-nats-deployed-acceptance-20261007.md) retains the original browser and cancellation evidence.
This correction does not close pending preparation recovery or any remaining service-loss gate.
