# Web HITL approvals and sub-agent hierarchy parity (Track W) — 2026-10-16

Branch `feat/web-hitl-approvals-parity` from `main` `dd0bbc170` (after #1084 merged). Web only
(`apps/elitea-web`); no Worker, Main, proto, CI, Helm or compose change. Brief:
`handoffs/point5-wave1-20261008/T-W-web-hitl-parity.md`. This is the D4 prerequisite of Wave 2a-2: the same buckets
and labels render Parallel branches and Map items once the Worker stamps their hierarchy (`T-2A-fanout-runtime.md`
§7 D4, 2a-2 task 11).

## 1. Business behaviour taken from the current EliteaUI, and what was not ported

Reference: `EliteaUI/src/[fsd]` (read-only).

| Behaviour | Reference | New Web |
|---|---|---|
| Cards below the thinking view, inside the answer block: coordinator auth cards, one approval accordion per delegated-auth instance, the token-limit `ChatContinue`, coordinator HITL cards, one `SubAgentAccordion` (`paused`, `defaultExpanded`, `transparent`) per spawning instance | `chat-box/application-answer/AnswerApprovals.jsx`, `ApplicationAnswer.jsx:224` | `ui/chat-box/AnswerApprovals.tsx`, `ApplicationAnswerApprovals.tsx`; mounted in `ApplicationAnswer.tsx` after the content/error block. The flat bottom list and the warning-boxed auth card above the thinking view are gone. |
| Instance key `parent_agent_call_id` → `child_thread_id` → `thread_id` → `JSON(parent_agent_path)` → name | `useApplicationAnswerState.hooks.js:292-349` | `lib/approvalBuckets.ts` `buildHitlBuckets`; auth: `groupToolkitAuthorizationActions` (`mcpAuthorization.helpers.js:275-321`) |
| Labels: ` ▸ `, root tier numbered only when the root name repeats, backend `sibling_ordinal` authoritative, lexical fallback, contiguous rank, trace context keeps numbering | `subAgentGrouping.helpers.js:393-461` | `lib/subAgentBreadcrumbs.ts` — reference tests ported verbatim (`subAgentBreadcrumbs.test.ts`, 12) |
| Owner path / instance key of an action | `executionHierarchy.helpers.js:5-117` | `lib/executionHierarchy.ts` `getActionOwnerPath`, `getSubAgentName`, `getSubAgentInstanceKey` — reference tests ported verbatim (`executionHierarchy.owner.test.ts`, 7) |
| `SubAgentAccordion`: pill summary, collapsed default, shimmer + spinner while running, `paused` stops it, reader owns expansion after mount | `sub-agent-section/SubAgentAccordion.jsx` | `ui/sub-agent-section/SubAgentAccordion.tsx` (reference prop set). Removed: raw `<pre>` JSON per action, auto-expand of paused blocks, italic "Paused — awaiting resume". |
| Thinking view: coordinator runs and instances interleaved in timeline order; `${name} (${n})` / breadcrumb labels; `expanded = isStreaming \|\| reader`, no `onChange` while streaming, stays expanded once a live turn settles | `components/Chat/ApplicationThinkView.jsx` | `ui/chat-box/ApplicationAnswerThinking.tsx` |
| `ChatHitlActions` (neutral panel, Markdown, default message, `BaseBtn` positive/alarm + icons), sensitive-action card, clarifying container, `BlockWithCommentControl`, `EditControl` | `chat-hitl-actions/*.jsx` | `ui/chat-hitl-actions/*` |
| `AskUserAnswerSummary` above the answer text | `AskUserAnswerSummary.jsx`, `AnswerMessageItems.jsx:41`, `hitl.helpers.js` | `ui/chat-hitl-actions/AskUserAnswerSummary.tsx`, `lib/askUserSummary.ts` |
| Toolkit authorization card = bare `ChatContinue` (`⚠️ Toolkit Authorization Required`, message, details, Authorize / Skip) | `ChatContinue.jsx`, `ApplicationAnswer.jsx:140-152` | `ui/chat-continue/ChatContinue.tsx`, `ApplicationAnswerAuthorization.tsx` `AuthorizationCard`, `lib/toolkitAuthorizationContext.ts` |
| A clicked card in an in-process root set is retired (`queued`, `hidden`); siblings stay answerable; restore on refusal | `ChatBox.jsx:1859-1905`, `hitl.helpers.js` `settleHitlResumeAttempt` | `widgets/chat-box/ui/hooks/useChatBoxHandlers.hitl.ts` `decideHitlCard` |

**Deliberately not ported / changed:**
- **Name folding** (`partitionActionsIntoBlocks` rule 2: fold a fresh call id into the last paused same-name block). It
  merged two parallel instances of one agent. This runtime resumes with stable ids, so instances are identified only by
  call id / thread / path (`lib/subAgentGrouping.ts`, W5).
- **Reference partial-set coalescing** (150 ms batch of whatever is decided). Today's Main requires the complete pending
  set (`continue.go` `decisionsMatchCurrentHITLInterrupts`), so the Web queues until the last card is decided, then
  submits once. Wave 2 replaces only `decideHitlCard` with a per-card POST.
- **Socket fallback after a REST answer** (any refusal or 409). Never taken. A 409
  `agent_hitl_already_resolved` / `agent_authorization_already_resolved` quietly marks the set answered.
- **Raw HTML in interrupt messages.** Messages render as Markdown with `renderHtml={false}` (no model-supplied
  `<img>` beacons). The reference renders HTML.
- **Family-token auto reuse** in `ChatContinue` (`McpAuthHelpers.reuseAuthFamilyToken`) — needs the mcps feature.
- **Static pause** (`interrupt_before/after`) has no reference screen; it is rendered as the generic card style: one
  card per pause, the node name, a `Continue` button (sends the fixed text "Continue", the previous control's default),
  no text box, no proof kind, no thread id, no multi-select. A saved-pipeline leaf sits in its spawning call's
  accordion (`lib/staticPauseCards.ts`, `ui/chat-box/StaticPauseCard.tsx`).
- **Mixed HITL + authorization set (FU-228).** Main resumes one kind per continuation; the Web queues each kind and
  submits each set when complete. The reference submits the root batch including auth decisions together.

## 2. Changed paths

All under `apps/elitea-web/`. New libs: `src/features/chat-messages/lib/{approvalBuckets,subAgentBreadcrumbs,
staticPauseCards,askUserSummary,toolkitAuthorizationContext}.ts`; `src/features/chat-messages/model/usePauseSettle.ts`;
`src/widgets/chat-box/model/hitlDecisionQueue.ts`; `src/widgets/chat-box/ui/hooks/useChatBoxHandlers.hitlCards.ts`;
`src/entities/message/lib/normaliseHitlInterrupts.ts`. New UI: `ui/chat-box/{AnswerApprovals,ApplicationAnswerApprovals,
StaticPauseCard}.tsx`, `ui/chat-hitl-actions/AskUserAnswerSummary.tsx`. Changed: `lib/{executionHierarchy,
subAgentGrouping,hitlInterrupts,chatStreamInterruptFrames}.ts`, `model/{useChatStreamTransport,
useChatStreamRunStarters}.ts`, `ui/chat-box/{ApplicationAnswer,ApplicationAnswer.types,ApplicationAnswerAuthorization,
ApplicationAnswerThinking,ChatMessageList,ChatMessageList.types}.tsx`, `ui/chat-continue/ChatContinue.tsx`,
`ui/chat-hitl-actions/{ChatHitlActions,BlockWithCommentControl,EditControl,AnswerQuestionsControl}.tsx`,
`ui/sub-agent-section/SubAgentAccordion.tsx`, `entities/message/lib/{normalise,toolActions,wire}.ts`,
`widgets/chat-box/ui/{ChatBox,ChatBox.helpers,StaticPipelineContinueControls}.tsx`,
`widgets/chat-box/ui/hooks/{useChatBoxActions,useChatBoxData,useChatBoxHandlers,useChatBoxHandlers.hitl,
useChatBoxHandlers.helpers,useChatBoxHandlers.authorization,useChatBoxHandlers.authorization.types,
useSessionMcpAuthorizationRefs}.ts`, `src/shared/i18n/en.json` (35 keys, written by `scripts/i18n-backfill.mjs`),
4 e2e specs (selectors moved to the reference test ids: `chat.toolkit-hitl`, `chat.tail-sensitive-tools`,
`chat.tail-trace-steps`, `chat.mcp-auth-prompt`).

## 3. Tests

Full Web vitest (node project) on the merged head `13fcc9719`: **1748 files, 17585 passed, 6 expected-fail, 1 failed** — `src/shared/api/themePreference.test.ts` "keeps the mark in memory when storage refuses the write", which fails identically on clean `origin/main` `f4e6157aa` locally (Node 24 `localStorage`; not touched here). Earlier scoped run over `src/features/chat-messages src/widgets/chat-box src/entities/message src/pages/chat`:
**116 files, 1282 passed, 2 expected-fail (pre-existing `it.fails` in `canvasFileSource.test.ts`, #5846/#5845), 0 skipped.**

New or rewritten (counts): breadcrumbs golden 12, owner/instance golden 7, approval buckets 8 (nested `Pipeline ▸
Agent`, `Sub (1)`/`Sub (2)`, `Parent (1) ▸ Sub`, trace context, never-fold, mixed), static pause cards 3, placement
7 (`ApplicationAnswer.approvals.test.tsx`: root card plain below the thinking view, Authorize/Block both answer,
nested accordion, two same-name instances with one answered, queued card disabled, ask_user summary, root static
pause), thinking panel 5, `SubAgentAccordion` 4, recorded-stream interrupt frames 12 (nested pause, two same-name
instances across progress + terminal frames, mixed guard + ask_user + auth, parallel aggregate → 0 cards, terminal →
not streaming, live = reload hierarchy incl. mixed coordinator + sub-agent), decision queue 14 (bounds 16/17
decisions, 8192/8193-byte value, corrupt storage, empty `tool_call_id`), handlers queue 17 (1 of 2 posts nothing and
keeps streaming; 2nd posts one continuation with both decisions in message order; 409 → 0 socket emits, no error,
cards stay answered; other refusal → restored; reload restore for HITL and auth; child scope; unsendable set),
pause settle 4 + transport 3 (batch kept, late card after window, stamped `pause_progress`), cards parity 8,
`ChatContinue` 6, ask_user summary 3 + lib 7, toolkit auth context 4.

Other gates (local): `tsc --noEmit` 0 errors; `oxlint --deny-warnings` clean; `i18n-backfill --check` OK; `knip
--max-issues 0` OK; `check-budgets`, `check-layer-cycle`, `check-dead-code`, `check-partition`,
`check-testid-namespace`, `check-stale-disclosure`, `check-visual-coverage`, `check-contract-coverage`, `theme-gate` OK;
`check-bundle-secrets` OK. Not run: the Playwright streaming e2e lane (needs the full standalone stack; machine load
around 40-50); the changed specs only swap selectors.

`PipelineTestChatCodeDebug.test.tsx` (3 cases) clicked a `debug_probe` accordion: the old grouping treated a pipeline
Code node (`original_name` only) as a sub-agent. The current UI renders plain pipeline nodes as flat, node-labelled
chips (`getSubAgentName`, EliteaUI #5389), so the test now asserts no accordion and opens the receipt from the flat
row; the download path is unchanged.

## 4. Performance

- Budgets: bucketing and labels are O(cards + trace actions) per render, memoised per message
  (`ApplicationAnswerApprovals.tsx` `useAnswerApprovalModel`); breadcrumbs O(n log n) over ≤16 cards. No new network
  round trip: a decision set is still one REST continuation (`decideHitlCard`), now exactly one for N cards instead of
  N refused ones.
- Pause batch: a terminal pause keeps observation open at most `PAUSE_SETTLE_MAX_MS` = 3 s, quiet window
  `PAUSE_SETTLE_QUIET_MS` = 750 ms (`model/usePauseSettle.ts`), proven by `usePauseSettle.test.ts`.

## 5. Durability

| Crash / restart window | Rule | Mechanism | Proof |
|---|---|---|---|
| Browser reload with some cards decided, set not submitted | Decisions survive in `sessionStorage` (`el.chat.hitlDecisions`), re-applied on hydrate; pruned when the card is no longer pending | `hitlDecisionQueue.ts`, `useChatBoxHandlers.hitlCards.ts` `restoreQueuedHitlDecisions`, `useChatBoxData.ts` | `hitlQueue` tests "reload restore" |
| Another tab consumed the set | 409 → cards answered quietly, composer released | `useChatStreamRunStarters.ts` `already-resolved`, `hitl.ts` `submitHitlSet` | `hitlQueue` 409 test |
| Card arriving after the pause window | Not shown live; Main persists the full pending set, reload shows it; nothing is submitted until the set is complete, so no decision is lost | `usePauseSettle.ts` | transport test "late card" |

Recovery classes (Web × phase):

| Component × phase | Class | Notes |
|---|---|---|
| Web × HITL pause and decision (reload) | R | queued decisions restored; pending cards answerable |
| Web × HITL decision (two tabs) | I | Main consume-once (409) is the dedupe; Web treats it as answered |
| Web × output delivery of a pause batch | I (interim) | bounded window; FU-227 makes it exact |

## 6. Resilience

- Bounded: 16 decisions per response (Main's max), 8 KiB value, 8 responses, 512-char ids (`HITL_QUEUE_LIMITS`,
  `hitlDecisionQueue.ts:60`); agent path ≤3 tiers, names ≤2048, call ids ≤512 on live (`hitlInterrupts.ts`) and reload
  (`normaliseHitlInterrupts.ts`, `toolActions.ts` `persistedPath`); pause window capped at 3 s.
- Typed, readable failures: not-queued, unsendable set, route refusal restore the cards with a message; no silent
  socket fallback for a multi-card set.
- Strict validation of stored state (all-or-nothing on read; corrupt → absent).

## 7. Security (`rules/security.md`)

| Category | Applies | Evidence |
|---|---|---|
| Trust boundaries / identity | Minimal | No identity from headers; ids sent are re-authorized by Main in the admission transaction. |
| Object authorization | Server-side, unchanged | Client only scopes credentials: tokens only for cards decided `authorize`, declines only for `skip` (`useChatBoxHandlers.authorization.ts`). |
| Input / amplification | Yes | Bounds above; stored blob validated strictly; reload type guards for `message`/`available_actions`. |
| Injection / XSS | Yes | No `dangerouslySetInnerHTML`; interrupt messages Markdown with `renderHtml={false}` (test `ChatHitlActions.parity.test.tsx` "without raw HTML"); tool args as React text; masked `***` kept. |
| Egress / SSRF | No | Browser only. |
| Secrets | Yes | Tokens never persisted (queue refuses values for auth decisions); only user-typed decision values are stored, in the `el.` namespace swept by logout `clearNamespace`; in-memory maps cleared on conversation change. No logging of values. |
| Supply chain | No new dependency | `npm audit`: 7 pre-existing (2 low, 1 moderate, 4 high: braces, katex, mermaid, micromatch, smol-toml, source-map-js, vite-plugin-singlefile); 0 new. |

Secret scan of the diff before each commit: 0 matches (token/key patterns).

## 8. Real-browser evidence

All UI proof in the user's Chrome through Claude in Chrome (own tabs only, user's existing sign-in; no settings changed).

### 8.1 Reference (current EliteaUI, `http://localhost/`, project "Private" id 2)

Fixtures already existed (agents created earlier through the UI): "Full Name Resolver" (agent 9) with sub-agents
"Name Resolver" (toolkits `test`, `sharepoint`) and "Surname Resolver"; "R9 Parallel Parent" with "R9 Auto Child".
Turns were sent from the agent page test chat (UI); nothing was created through the database.

| # | Scenario | Observation | File (session scratchpad `evidence/`) |
|---|---|---|---|
| R1 | Same sub-agent called twice in parallel (conversation 783, persisted) | thinking view rows `R9 Auto Child (1)`, `R9 Auto Child (2)`; coordinator chip after them | `ref-01-parallel-same-child-labels.jpg` |
| R2 | One instance expanded | instance chips inside, collapsed siblings unchanged | `ref-02-child-accordion-expanded.jpg` |
| R3 | "Resolve the full name Ivan Petrov" (live) | the delegated-auth card for `sharepoint` appears first, inside a `Surname Resolver` approval accordion, while siblings still run | `ref-03-auth-card-first-live.jpg` |
| R4 | Mixed pause (live) | below the thinking view: `Surname Resolver` accordion with "Toolkit Authorization Required" (Authorize / Skip), then `Name Resolver` accordion with "Sensitive Action Authorization Required" `test.delete_file` (Authorize / Block / Block with Comment). Order: auth buckets, then HITL buckets | `ref-04-mixed-auth-and-sensitive.jpg` |
| R5 | Block on the sensitive card | that card retires at once; `Name Resolver` resumes (spinner); the auth card stays answerable | `ref-05-after-block-auth-remains.jpg` |
| R6 | Skip on the auth card | card retires; `Surname Resolver` resumes, then raises its own sensitive card in its accordion | `ref-06-after-skip-resumed.jpg`, `ref-07-next-card-surname.jpg` |

Authorize was not clicked on either card (a delete action and an OAuth flow on the user's environment). The
agent-page test chat blocks navigation ("Leave site?"), so the reference reload check was not repeated there.

**Mixed-card submission (FU-228).** The reference submits each card on its own: Block resumed `Name Resolver`
while the auth card stayed pending. The new Web keeps the same placement and order and queues each kind separately
(HITL set, authorization set), submitting each set when it is complete. A one-card set therefore submits at once,
as in the reference. Whether Main accepts a HITL continuation while authorization requests are still pending on the
same response is Main's contract (FU-228); the result on pm1084 is recorded in §8.2.

### 8.2 New Web (production-profile stack `pm1084`)

Image for the pass: `elitea-web:trackw-13fcc9719`, Id `sha256:0d017774eb09…`, built from `13fcc9719` (this branch
merged with `main` `f4e6157aa`); extracted SPA assets contain the branch-only strings `chat-static-pause-panel` and
`pause_progress`. It supersedes `elitea-web:trackw-00cddc4e2`. The image stays valid for every later head whose
`git diff --name-only 13fcc9719 <head>` touches no Web build input (`apps/elitea-web/**`). Results: pending (stack not
yet free).

## 9. Visual gaps (deferred)

Per the user (2026-10-16), this PR matches structure and behaviour (placement, order, hierarchy and labels, actions per
card, independent resolution, reload, two tabs, mixed cards). Pixel styling is one later follow-up:
- neutral panels use `background.chatContinueBackground` (same values as the reference `surface.interactive.active`);
  10/12 px radii → `radiusMd` (8 px), 6 px → `radiusSm`; 13 px text → 12 px `bodySmall`; Parameters header uses
  `background.aiProviderAccordion.*`; sub-agent label `bodySmall` (no `bodySmall2` in the new type scale);
- actions inside the thinking view are the new-Web `ActionView` preview rows, not the reference toolkit/model chips
  (`renderGroupChips`), for coordinator and sub-agent actions alike;
- the reference answer block wraps the approval accordions on a slightly lighter surface; the new Web uses the plain
  answer-block surface.

## 10. Follow-ups

- **FU-227** (Worker, 2a-2): stamp `pause_progress: true` on per-card progress pause frames; then delete the
  `usePauseSettle` window.
- **FU-228** (Main): one continuation for a mixed HITL + authorization set.
- After a 409 the answer stays blank until the next reload (no handler-level refetch/reattach exists); wire
  `reattachStreamedExecution` once the attach target is known after a refusal.
- Visual gaps of §9 as one styling follow-up.
- Wave 2: replace `decideHitlCard` queue-then-submit with the per-interrupt decision API.
