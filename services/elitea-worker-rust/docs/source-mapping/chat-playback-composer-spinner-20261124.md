# Chat composer spinner fix on read-only playback and withdrawn agents (2026-11-24)

| Item | Value |
| --- | --- |
| Date | 2026-11-24 |
| Branch | `fix/chat-playback-composer-spinner` (based on `origin/main` `ca4a50234`) |
| Commit (code & tests) | `2612f5e60` |
| Components | Web (`apps/elitea-web`) only. No Rust or Go changes. |

## Business behaviour and comparison to current EliteaUI

In current EliteaUI (`ChatBox.jsx:2855-2874, 3084`), `NewChatInput`'s `isLoading` spinner flag tracks only active asynchronous loading states (`isLoadingConversation`, `isFetchingParticipant`, `modelSettingsAreSaving`, `isUploadingAttachments`, `isUpdatingInternalToolsConfig`, `activeConversation?.isSending`, and non-injectable streaming), never static read-only states (`isPlayback`).

In `elitea-web`, `deriveChatBoxInputState` folded both static read-only states (`isTestPlayback` in pipeline editor `Restore Test` playback mode and `#972` `isActiveParticipantWithdrawn` on unpublished agents) into `isComposerBusy`, and `ChatBoxComposer` passed `isLoading: inputState.isComposerBusy` to `NewChatInput`. That caused `UserInputFooter` (`showLoading && <UploadProgressIndicator />`) to spin an indeterminate MUI `<CircularProgress />` around the composer Send/Voice button permanently while viewing a restored `TERMINAL` test run or a withdrawn-agent conversation.

- **Fixed behaviour:** `deriveChatBoxInputState` (`apps/elitea-web/src/widgets/chat-box/ui/ChatBox.helpers.ts:163-193`) splits `isComposerLoading` (active async operations: `isLoadingConversation`, `isFetchingParticipantDetails`, `isUploadingAttachments`, `isUpdatingInternalToolsConfig`, `isConversationSending`) from `isComposerBusy` (which continues to block `Send`, drafts via `isDraftInputBusy`, and participant switching via `isInputLoading` for `isTestPlayback` and `isActiveParticipantWithdrawn`). `ChatBoxComposer` (`apps/elitea-web/src/widgets/chat-box/ui/ChatBoxComposer.tsx:136`) passes `isLoading: inputState.isComposerLoading` so read-only states disable the composer and Send without showing an infinite loading spinner.
- **Deliberately unchanged:** Ephemeral Test Chat continues to reset on hard page reload and opens `isPlayback: true` only when viewing restored `TERMINAL` runs via `View run history -> Restore Test`. Streaming (`isStreaming`) continues to leave `isComposerLoading` and `isComposerBusy` `false` so A17 message queuing remains active.

## Changed paths

- [`apps/elitea-web/src/widgets/chat-box/ui/ChatBox.helpers.ts`](../../../../apps/elitea-web/src/widgets/chat-box/ui/ChatBox.helpers.ts): `deriveChatBoxInputState` (`163-193`) adds `isComposerLoading` (`177-178, 192`).
- [`apps/elitea-web/src/widgets/chat-box/ui/ChatBoxComposer.tsx`](../../../../apps/elitea-web/src/widgets/chat-box/ui/ChatBoxComposer.tsx): passes `isLoading: inputState.isComposerLoading` (`136`).
- [`apps/elitea-web/src/widgets/chat-box/ui/ChatBox.helpers.test.ts`](../../../../apps/elitea-web/src/widgets/chat-box/ui/ChatBox.helpers.test.ts): asserts `isComposerLoading === false` for `isTestPlayback`, `isActiveParticipantWithdrawn`, and `isStreaming`, and `true` for active loading flags.
- [`apps/elitea-web/src/pages/pipelines/ui/PipelineTestChat.test.tsx`](../../../../apps/elitea-web/src/pages/pipelines/ui/PipelineTestChat.test.tsx): adds integration component test rendering `<PipelineTestChat restore=... />` with a `TERMINAL` run and asserting disabled `chat-message-input` with zero `role="progressbar"` elements after participant fetches settle.
- `services/elitea-worker-rust/docs/source-mapping/{chat-playback-composer-spinner-20261124.md,README.md}`: this mapping.

## Delivery gate properties

### Performance

- `deriveChatBoxInputState` (`ChatBox.helpers.ts:163-193`) replaces two linear `OR`/`Array.some` expressions in the same O(1) memoized render derivation with zero extra renders, subscriptions, or network calls.
- Budget gate (`node scripts/check-budgets.mjs`): PASS across `7438` files (`ChatBox.helpers.ts` = `392` lines <= 400-line ceiling).

### Durability

- Pure UI read-only rendering fix (`isPlayback` / `isActiveParticipantWithdrawn`): persisted conversation state, `editor_test_runs`, checkpoints, and replay selection are unchanged (`usePipelineTestConversation.ts` untouched).

### Resilience

- Read-only states (`isTestPlayback`, `isActiveParticipantWithdrawn`) still enforce `disabledInput: isDraftInputBusy === true` and `disabledSend === true`, while active async loading (`isFetchingParticipantDetails`, `isLoadingConversation`, attachment uploads, internal-tool updates, sending) continues to show `<UploadProgressIndicator />` until settled and clears cleanly afterward.

### Security

- **UI-only scope:** No auth, route, API schema, SQL, egress, HTML/Markdown sanitizer, CSP, or secret-handling path is added or modified. Read-only fail-closed gates (`disabledSend`, `isDraftInputBusy`, `isInputLoading`) remain intact so playback and withdrawn conversations still refuse message submission, attachments, and participant switching.

| Category (`rules/security.md`) | Status | Mechanism & Proving Test / Justification |
| --- | --- | --- |
| Trust boundaries and identity | N/A | Client presentation derivation only; server-side auth/conversation ownership unchanged. |
| Authorization (object level) | N/A | No API routes or server queries added/modified; UI still blocks sends on `isTestPlayback` & `isActiveParticipantWithdrawn` (`ChatBox.helpers.ts:179-192`). |
| Input, parsing and amplification | N/A | No parser or untrusted payload decoder changed. |
| Injection and construction | N/A | No HTML, Markdown, SVG, SQL, URL, or DOM-innerHTML sink added; only boolean flags derived (`ChatBox.helpers.ts:177-179`). |
| Egress and SSRF | N/A | No outbound server or client fetch logic changed. |
| Secrets | Applicable | `git diff` secret scan across branch diff = `0` hits; zero credentials/tokens added. |
| Supply chain and deployment | Applicable | Zero dependencies added (`package.json` and `package-lock.json` untouched); `npm audit` shows only `21` pre-existing moderate dev-dep advisories (`glob`/`minimatch` via `@storybook/react-vite` & `orval`, `0` new). |
| Cross-layer differentials and version skew | N/A | Wire contracts unchanged. |
| Agentic delegation and untrusted content isolation | N/A | No delegation, HITL, or tool rendering changed. |
| TOCTOU, concurrency races, and effect replay | Applicable | Active `isConversationSending` and `isFetchingParticipantDetails` still block Send during load/send transitions while clearing the spinner cleanly once settled (`ChatBox.helpers.ts:177-189`). |
| Sandbox escape and resource isolation | N/A | No sandbox or runner code touched. |
| AuthN, OAuth, PAT, and cryptographic hygiene | N/A | No auth or crypto touched. |

## Recovery guarantees

N/A — pure React UI loading-state derivation (`isComposerLoading`); no Main, Worker, Sandbox, NATS, PostgreSQL, or LLM execution/checkpoint/recovery path is altered.

## Verification, test counts, and skips

Executed in `apps/elitea-web` (Node `v24.19.0`, npm `11.17.0`):

- `npm run typecheck` (`tsc --noEmit`): **PASS** (`0` errors).
- `npx oxlint src/widgets/chat-box/ui/ChatBox.helpers.ts src/widgets/chat-box/ui/ChatBox.helpers.test.ts src/widgets/chat-box/ui/ChatBoxComposer.tsx src/pages/pipelines/ui/PipelineTestChat.test.tsx`: **PASS** (`0` warnings, `0` errors across `4` files).
- `npx vitest run --maxWorkers=2 src/widgets/chat-box/ui/ChatBox.helpers.test.ts src/widgets/chat-box/ui/ChatBoxWithdrawnNotice.test.tsx src/pages/pipelines/ui/PipelineTestChat.test.tsx src/features/chat-input/ui/NewChatInput.test.tsx`: **4/4 files, 74/74 tests PASS** (`ChatBox.helpers.test.ts` `50`, `PipelineTestChat.test.tsx` `11`, `NewChatInput.test.tsx` `9`, `ChatBoxWithdrawnNotice.test.tsx` `4`; `0` skipped).
- `node scripts/check-budgets.mjs`: **PASS** (`7438` files checked, within §3.5 budgets; `ChatBox.helpers.ts` = `392` lines).
- `node scripts/theme-gate.mjs`: **PASS** (`9/9` checks + `brandPack.contract.test.ts` `5/5`).
- `npm audit --audit-level=critical`: `21` pre-existing moderate dev-dep advisories (`0` new).
- `bash scripts/no-binaries-check.sh`: **PASS**.
- Full Vitest suite / Playwright Linux visual suite: deferred to GitHub Actions on PR.

## Real-browser evidence (`fixspin`, `2026-11-24`, torn down)

- **Built image & `stack.sh binary-check`:**
  - `ghcr.io/elitea-ng/elitea-web:fixspin-2612f5e60` (`id=ba7312f9987bb9c3ac38478cf1e2554a107dfb7a5d4295c9102b7d4695ea8d26`, `/usr/share/nginx/html/spa`, `marker=isComposerLoading`, `count=2` OK).
  - Companion verified images: `elitea-main:p5b2-c4c04d321`, `elitea-worker-rust:p5b2-c4c04d321`, `elitea-sandbox-supervisor:pm-778e5beb8`, `elitea-code-runner:pm-2d7f7d3e8-{deno,rust}`.
- **Disposable production-profile stack (`fixspin` at `http://fixspin.localhost:18460/app/`):**
  - `stack.sh verify-profile --name fixspin`: **`50 passed, 0 failed`**.
- **Real-browser verification (`admin@centry.user`, Project `Private` ID `2`, zero mocks):**
  1. Created and saved Pipeline `fixspin-verify` (Pipeline ID `170`, Version ID `196`, `START -> LLM_1 -> END`, model `gpt-5.4-mini`, system prompt `"Reply with exactly SPIN_OK"`).
  2. Sent `"hello"` in Test Chat and received live streamed reply **`SPIN_OK`** (`chat-message-input` enabled, `[role="progressbar"]` = `0`, `.MuiCircularProgress-root` = `0`).
  3. Hard-reloaded editor URL (`200 OK`): ephemeral Test Chat reset to clean welcome screen (`chat-message-input` enabled, `[role="progressbar"]` = `0`, `.MuiCircularProgress-root` = `0`).
  4. Opened `View run history` -> `Restore Test` on completed run (`2026-11-24T12:31:57.081Z · TERMINAL`, run ID `4ef6244f-ae5c-5112-bccf-3af49353f3f8`): confirmed `Existing run` selector renders `TERMINAL`, `hello` -> `SPIN_OK` transcript is visible, `[data-testid="chat-message-input"].disabled === true`, and both `[role="progressbar"]` and `.MuiCircularProgress-root` counts are **`0`** (no blue spinning ring around Send/Voice).
  5. Clicked `Clear Test`: confirmed clean welcome state, `Clear Test` disabled, `[data-testid="chat-message-input"].disabled === false`, `[role="progressbar"]` = `0`, `.MuiCircularProgress-root` = `0`, **`0` browser console errors**, and **`0` HTTP `4xx`/`5xx` responses**.
- **Teardown:** `stack.sh down --name fixspin --volumes` removed all `fixspin` containers, volumes, networks, and temporary stack material (`docker ps -a --filter "name=elitea-fixspin"` empty).

## Fixtures

Unit and component tests use MSW handlers and Testing Library (`ChatBox.helpers.test.ts`, `PipelineTestChat.test.tsx`). Real-browser verification on `fixspin` created `fixspin-verify` through the UI (`0` database seeding, `0` response mocks).

## Follow-ups

None.
