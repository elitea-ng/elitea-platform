# Pending-only root caller id, live graph refusal notices, and typed refusal copy (FU-359, FU-360)

- **Branch:** `fix/fu359-fu360-refusal-notices`, off `origin/main` (`4f9d63873`).
- **Scope:**
  - **FU-359:** Preserve root `application_id` and `grants_enforced` when delegation grant enforcement is on with zero approved grants (every grant still pending), so both pipeline roots and ordinary roots name `caller_id` and show `Review delegation`.
  - **FU-360:** Fold assembly-time `delegation_refused` frames (`pipeline_node`, Agent node and `#2235` saved-pipeline node) live on the Web stream before `agent_start` and before `execution.failed`, matching post-reload state.
  - **Typed refusal vs generic `"Run failed"` copy:** Distinguish typed authorization, policy, and validation refusals from real runtime errors across chat (`ErrorTrace`, `ApplicationAnswer`), workspace transcripts (`TurnTranscript`), pipeline run badges (`useRunEvent`, `RunStateNode`), and `run_finished` notifications (`runFinished`, `pushWorker`).

## Root causes and fixes

### 1. FU-359: Root `application_id` with pending-only grants

- **Main (`services/elitea-main/internal/application/workerdelegation/grants.go`):** Commit `961261746` (`#2242`) already removed the `len(grants) == 0` early return in `withGrants`, so Main sends `RootApplicationId` and `GrantsEnforced = true` whenever the grant switch is on.
- **Worker (`libs/rust/agent-runtime/src/delegation.rs`):** `DelegationPolicy::grants_enforced()` previously returned `!self.grants.is_empty()`, which reported `false` when Main enabled enforcement with zero approved grants (only pending grants). `DelegationPolicy` now stores an explicit `grants_enforced: bool` flag set by `with_grants`, preserving both `grants_enforced() == true` and `root_application_id() == root_id`.
- **Tests:**
  - Main `workerdelegation/grants_test.go`: positive cases for pipeline roots and ordinary roots with zero consumable grants (default settings and saved settings + principal bound); negative cases for switch off, non-positive/missing `application.id`, and invalid `ResourceProjectId`.
  - Rust `elitea-agent-runtime` `delegation_tests.rs`: ordinary root with `with_grants(175, [])` sets `grants_enforced() == true` and names `caller_id == 175` on the refusal notice; unenforced policy omits `caller_id`.
  - Worker `protocol/delegation.rs`: `policy_from_command` with `grants_enforced: true`, `root_application_id: 174`, `grants: []` vs `grants_enforced: false`.
  - Worker `pipeline_root_grant_tests.rs`: pipeline root refusal with zero approved grants names `caller_id == ROOT_ID` for both an Agent node and a saved-pipeline node.

### 2. FU-360: Live `delegation_refused` notice folding

- **Root cause:** During pipeline assembly, the Worker publishes `delegation_refused` (`pipeline_node`) before `agent_start` and before `execution.failed` (`AUTHORIZATION_FAILED`). Main persists the notice onto `meta.delegation_notices_v1` (visible after reload) and streams `execution.node_event` (`delegation_refused`) stamped with `can_review` and `caller_name` before `execution.failed`. On Web, `reduceDelegationFrame` (`apps/elitea-web/src/features/chat-messages/lib/chatStreamDelegationFrames.ts`) returned `history` unchanged when `index === -1` (no assistant message existed yet), dropping the live notice before `execution.failed` created the failed answer row.
- **Fix:** `reduceDelegationFrame` now accepts `context: ChatStreamContext`. When a valid notice arrives with `index === -1`, it creates the in-flight assistant message via `createAssistantMessage(frame, context)` carrying `delegationNotices`. When `execution.failed` arrives, `settleInFlight` settles that message and keeps `delegationNotices`, matching post-reload `convertMessagesToChatHistory` state. Malformed notices still return `history` unchanged without creating a message.
- **Tests:**
  - Main `internal/api/v2/executions/events_delegation_test.go`: verifies `EventHandler` streams stamped `pipeline_node` `delegation_refused` (with `can_review: true` and `caller_name`) before `execution.failed`.
  - Web `chatStreamDelegationFrames.test.ts` and `useChatStreamTransport.test.tsx`: verifies live folding for both Agent node and `#2235` saved-pipeline node refusals followed by `execution.failed`, matching post-reload notices.

### 3. Typed refusal vs generic `"Run failed"` copy

- **Shared classifier (`apps/elitea-web/src/shared/lib/runtimeFailure.ts`):** Classifies typed server failure codes into `refusal` (`authorization`, `policy`, `validation`), `cancelled` (`CANCELLED`), and `error` (real runtime errors such as `INTERNAL`, `MODEL_UNAVAILABLE`, `PIPELINE_CODE_FAILED`, or unknown/absent codes). Provides `delegationRefusalExplainsFailure(failureCode, notices)`.
- **Chat (`ErrorTrace.tsx`, `ApplicationAnswer.tsx`):**
  - Typed refusals render as a warning `Alert` (`data-outcome="refusal"`, `data-refusal-category`) with a sentence-case refusal heading (`Authorization refused`, `Request refused by policy`, or `Request refused`) and the server's reason (or a calm fallback if only a raw code arrived).
  - `CANCELLED` renders as an info `Alert` (`data-outcome="cancelled"`, `Run stopped`).
  - When `AUTHORIZATION_FAILED` already has a `delegation_refused` notice (live or reloaded), `ApplicationAnswer` suppresses the redundant generic `ErrorTrace` while keeping `DelegationNotices` and `FailureReference`.
  - Real runtime errors keep the red `error-trace` banner (`data-outcome="error"`).
- **Workspace (`TurnTranscript.tsx`):** Applies the same refusal status (`data-testid="turn-refusal"`) and delegation-explained suppression for cloud turns while keeping retryable and runtime errors as `severity="error"`.
- **Pipeline run pill (`parseRunsByEvent.support.ts`, `useRunEvent.ts`, `RunStateNode.tsx`):** Preserves `failureCode` on `execution.failed`; `RunStateNode` displays `"Run was refused"` tooltip and warning accent for typed refusals without editing `ConfigurationTab`, `EditorPanel`, `NodeCard`, or `RunStateDialog`.
- **Notifications (`runFinished.ts`, `pushWorker.ts`, `en.json`):** Adds explicit `'refused'` handling (`Run refused` / `A run was refused by policy or permissions.`) so refused `run_finished` notices do not fall through to `"Run finished"`, while keeping `'failed'` copy intact for real runtime errors.

## Security hardening (`SEC-FU359-01`..`SEC-FU359-04`)

- **`SEC-FU359-01`:** Scoped `delegationRefusalExplainsFailure` (`runtimeFailure.ts`, `ApplicationAnswer.tsx`, `TurnTranscript.tsx`) strictly to fatal `pipeline_node` refusal notices with generic `AUTHORIZATION_FAILED` messages, so non-fatal `agent_tool`/`pipeline_child`/`sub_agent` notices and distinct authorization error messages never hide `<ErrorTrace>` or `<TurnError>`.
- **`SEC-FU359-02`:** Cleared prior-generation `delegationNotices`, `failureCode`, and `failureCap` across regeneration in `regeneratingPatch.ts`, `chatStreamTurnFrames.ts`, `useChatStreamTransport.ts`, and Main's `ResetCurrentAgentResponse` (`agent_chat.sql` + regenerated `sqlcgen/agent_chat.sql.go`, stripping `delegation_notices_v1`, `is_error`, `error`, `error_code`, and `cap`).
- **`SEC-FU359-03`:** Enforced frame-type vs `notice.kind` consistency in `chatStreamDelegationFrames.ts`, `repos/delegation_notice.go`, and `delegationnotice.StampFrame`, and restricted pre-`agent_start` assistant message creation to `pipeline_node` `delegation_refused` frames.
- **`SEC-FU359-04`:** Guarded the `run_finished` `TEXT` outcome lookup in `pushWorker.ts` with `Object.hasOwn`.

## Verification

- **Main (Go):** `go vet` and `go test -race` pass on `internal/application/workerdelegation`, `internal/application/delegationnotice`, `internal/api/v2/executions`, `internal/db/sqlcgen`, and `internal/infra/db/repos` (`TestSavedAgentName`).
- **Worker / Runtime (Rust, offline locked, unchanged in security delta):** `elitea-agent-runtime` delegation suite (`42 passed`), `elitea-worker-rust` `protocol::delegation` (`10 passed`), and `agents::pipeline_tests::root_grant_tests` (`28 passed`).
- **Web (Node 24):** `vitest` (`14/14` files, `237/237` tests), `typecheck`, `lint -- --deny-warnings` (0 warnings / 6,583 files), `theme-gate`, `check-budgets`, `check-testid-namespace`, `check-layer-cycle`, `check-dead-code`, `i18n-backfill --check`, `check-handlers` all pass.
- **Browser + PostgreSQL stack proof (`fuwave`, `production-profile` real-model stack):**
  - **Stack & images:** Disposable stack `fuwave` at `http://fuwave.localhost:18460/app/` (`--port-base 18460`, dump `product-real-models-main-ce2756fad.dump`, `verify-profile`: `50 passed, 0 failed`), torn down cleanly (`--volumes`, `0` remaining containers).
  - **Verified images (`binary-check` PASS):** Main `ghcr.io/elitea-ng/elitea-main:fuwave-eeac76f35` (`id=942944df2a6defde2fb9010f5cd6a4b5e82e080e80c5bcd292a327d2c535afd8`, `delegation_notices_v1` count=`4`), Worker `ghcr.io/elitea-ng/elitea-worker-rust:fuwave-0ab69f837` (`id=944ab3ed166905592c9767ed410833be6a55855292401ad1d7eac1607fc24e69`), Web `ghcr.io/elitea-ng/elitea-web:fuwave-0ab69f837` (`id=3a1a0b88c3c63a52c749ff8520f9b6ab8d5134b27fcc2a5a189120259495009b`, `Authorization refused` count=`2`, `Run was refused` count=`2`, `Node needs configuration` count=`1`).
  - **Fixture provenance:** All `p_2` fixtures created via Main's authenticated HTTP API (`POST /applications/prompt_lib/2`, `PATCH .../application_relation/prompt_lib/2/...`, `POST /delegation_grants/prompt_lib/2/...`), zero direct DB fixture writes; live browser checks with zero response mocks and full `page.reload()` on every case.
  - **Case A1 (`fuwave_p0r_refusal` id `176` -> `fuwave_s_refusal_target` id `175`, `pipeline_node` grant `12` pending, `chat/890`):** Live and post-reload showed `[data-testid="delegation-notices"]` (`delegate could not use c1mockmcp20261008 from fuwave_p0r_refusal. This delegation is not approved.`, `Review delegation` href `/app/pipelines/all/176`) and `[data-testid="failure-reference"]` (`AUTHORIZATION_FAILED`) with generic `[data-testid="error-trace"]` suppressed (`count=0`). Approving grant `12` via UI (`Review delegation` -> Approve on `/app/pipelines/all/176?name=fuwave_p0r_refusal`) and clicking **Regenerate** in `chat/890` succeeded (`"Run delegate for FU359_PIPE_TEST"`) with `delegation-notices=0`, `failure-reference=0`, and `error-trace=0` both live and after reload; `p_2.chat_message_group` row `7524` confirmed clean `meta` without `delegation_notices_v1` or `error_code`.
  - **Case A2 (`fuwave_r0_ordinary_refusal` id `177` -> `fuwave_s_refusal_target` id `175`, `agent_tool` grant `13` pending, `chat/891`):** Live and post-reload showed `[data-testid="delegation-notices"]` (`fuwave_s_refusal_target could not use c1mockmcp20261008 from fuwave_r0_ordinary_refusal. This delegation is not approved.`, `Review delegation` href `/app/agents/all/177?review_delegation=175`) with `error-trace=0`; `p_2.chat_message_group` row `7526` confirmed persisted `meta.delegation_notices_v1` (`caller_id=177`, `callee_id=175`, `edge.kind="agent_tool"`).
  - **Case A3 (`fuwave_p_runtime_error` id `178`, `code` node JS throw, `chat/892`):** Live and post-reload showed `[data-testid="error-trace"][data-outcome="error"]` (`data-refusal-category=null`) and `[data-testid="failure-reference"]`; `p_2.chat_message_group` row `7528` confirmed `is_error: true`, `error_code: "PIPELINE_CODE_FAILED"`.
