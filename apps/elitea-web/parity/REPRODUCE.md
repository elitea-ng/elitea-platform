# Reproducing the parity manifest (unit P1, spec §8.3)

> **Historical (2026-10-22).** The `apps/elitea-ui` baseline submodule was
> removed, together with `tools/uictl`, `tools/parity-extract` and the CI
> `parity` job that ran the commands below. The steps here no longer run in
> this repository; they record how the manifest was produced. The manifest and
> its shards stay in place as a record and as input to the gates that still
> read them (endpoint manifest, contract coverage, visual coverage).

The manifest is **generated, not hand-authored** (§8.3 steps 1–5 are fully
mechanical; step 6 is scripted with the dedup mapping reviewed below; step 7
is scripted with a documented batching choice). This file records the exact
commands, the mechanical mappings the scripts use, and every deliberate
choice, so the extraction can be re-run and audited.

**CI does NOT re-run the extraction.** CI re-runs *validation + diff* only:

```sh
go run ./tools/uictl parity-manifest --validate --baseline apps/elitea-ui
go run ./tools/uictl parity-routes    --baseline apps/elitea-ui
go run ./tools/uictl parity-manifest --require-must   # release gate
```

## Pinned baseline

Everything is extracted from the **read-only** pinned checkout:

- repo: `apps/elitea-ui`
- commit: `a55f36cfb5ecb3834bb00bbc8d9cd9a1393168af` (a55f36cf)
- recorded in the manifest root index as `generatedFrom`

Every `source` entry is `apps/elitea-ui/<file>:<line>[ -<line>]` and must
resolve inside that commit; `uictl parity-manifest --validate` rejects any
item whose evidence does not resolve (`--baseline` points at the checkout —
absolute path in the worktree, submodule path in CI).

## Sharded layout (packaging only)

`parity/manifest.json` is a small **root index** (version, `generatedFrom`,
schema pointer, shard list with per-shard item counts).
The items live in **one shard per §8.6 domain**: `parity/manifest/<domain>.json`.
Item format is exactly §8.3 — sharding changes packaging only. Reasons:

1. `scripts/no-binaries-check.sh` fails any tracked file ≥ 1,048,576 bytes;
   the monolithic manifest was ~1.32 MB. Largest shard is ~400 KB.
2. Per-domain shards are the better CI-diff story: a chat PR diffs
   `manifest/chat.json` only, instead of a 35k-line monolith.

`uictl` reads the sharded layout natively: it merges the index + all shards
and enforces every rule (including duplicate-id detection) **globally**
across shards; index/shard disagreements (missing shard, item-count or
domain mismatch) fail the load.

## Regeneration

The extraction pipeline lives **in the repo** at `tools/parity-extract/`
(sidecar tool with its own `package.json` — deliberately OUTSIDE
`apps/elitea-web`, whose `package.json` is owned by unit F1). Parser deps
are exact-pinned (`@babel/parser@7.29.7`, `@babel/traverse@7.29.7`);
`node_modules/` and the `out/` intermediates are gitignored.

```
tools/parity-extract/
├── package.json              # exact pins + the two npm scripts below
├── common.mjs                # walker, parser, screen/domain maps, unit maps
├── extract-routes.mjs        # step 1
├── extract-queryparams.mjs   # step 2
├── extract-api.mjs           # step 3 (RTK endpoints + §5.7 raw transports)
├── extract-sockets.mjs       # step 4
├── extract-permissions.mjs   # step 5
├── extract-actions.mjs       # step 6
├── extract-copy.mjs          # step 7
├── extract-shell.mjs         # §8.6 shell anchors + W-007 evidence
└── build-manifest.mjs        # combiner -> root index + domain shards
```

```sh
cd tools/parity-extract
npm install                       # exact-pinned parser deps
export BASELINE=/abs/path/to/apps/elitea-ui   # pinned a55f36cf; defaults to ../../apps/elitea-ui
npm run extract                   # steps 1-7 + shell anchors, ordered; fails loudly on any census mismatch
npm run build                     # writes ../../apps/elitea-web/parity/manifest.json + manifest/<domain>.json
```

The pipeline is deterministic: the same baseline produces byte-identical
shards (ids are assigned in fixed sort orders per bucket).
`extract-routes` exits non-zero if any §8.1 pattern fails to verify;
`extract-sockets` asserts the 43+34 census; `extract-shell` asserts the
13-entity create menu and the SHELL-012 ordering; `extract-queryparams`
reports any §8.2 key the scan cannot find.

## What each step extracted (counts at a55f36cf)

| step | bucket | id prefix | items | how |
|---|---|---|---|---|
| 1 | routes | `ROUTE-` | 76 | §8.1 table seeded (already verified), every pattern re-verified against `routes.js` / `ProtectedRoutes.jsx` / `router.jsx` with real lines; 73 reachable patterns + 3 declared-but-unmounted anomalies (decision D4) |
| 2 | query params | `PARAM-` | 108 | AST scan of `useSearchParams` bindings, `searchParams.get/set/has/delete`, `new URLSearchParams(location.search…)`, the `SearchParams` constant map, wrapper hooks (`useSearchParamValue`, `use*FromUrl`), `search.includes(SearchParams.X)` sniffing, and `SearchParams.X` interpolated into share-link template literals (write-only URL contracts); one item per distinct key per route scope |
| 3 | API endpoints | `API-` | 212 | AST walk of all 32 `injectEndpoints` modules; URL template literals AND local `const url = …` initializers resolved to `{param}` shapes (no opaque `{url}` items remain); conditional/dynamic paths carry a `[dynamic path template]` marker |
| 3 | non-REST transports | `XPORT-` | 11 | raw `fetch(` / `new XMLHttpRequest` / `axios.*` sites outside the RTK layer (§5.7 superset — see report); call-site-dynamic URLs carry the `[dynamic path template]` marker |
| 4 | socket events | `SOCK-` | 77 | `sioEvents` (43) + `SocketMessageType` (34) from `constants.js:881-936` / `157-193`, plus emit/on call sites |
| 5 | permissions | `PERM-` | 62 | 51 distinct permission strings (PERMISSIONS leaves + inline literals + `useCheckPermission` sites + `<perms>.includes(...)` guard sites, incl. namespaced constants), 6 `PERMISSION_GROUPS`, 3 route guards, 2 platform flags |
| 6 | user-visible actions | `ACT-` | 107 | JSX handler props resolving (≤4 hops, two-pass incl. custom hooks and `useRef` indirections) to RTK mutation triggers or socket emits; socket events are only credited when the file literally emits them (receive-side `.on` handlers are never actions); deduped by (endpoint × screen) — full mapping in the appendix |
| 7 | copy | `COPY-` | 510 | user-visible strings per **source file** (JSX text, visible attributes/props, JSX string-expression children); `should` priority; 13 files whose strings are all assistive-technology labels are `[aria-only]` and verified by the a11y/storybook suite instead of visual regression |
| — | shell anchors | `SHELL-` | 27 | §8.6 shell row, mechanically: 9 sidebar entries, group filtering, deferred cache reset, socket indicator, 13 create-entity types, SimpleCreateRoutes suppression, feedback dialog (W-007) |
| — | §8.5 journeys | `JRNY-` | 30 | spec-authored journey list (like the ROUTE ids); anchors resolved mechanically from the extraction outputs |
| | **total** | | **1,220** | |

## Documented choices (the parts that are policy, not extraction)

1. **`ROUTE-069b`/`ROUTE-069c` renumbered to `ROUTE-072`/`ROUTE-073`.**
   §8.1 uses ids that violate §8.3's own `^[A-Z]{3,8}-\d{3}$` rule. The
   schema rule wins; the original spec ids are preserved in the item titles.
2. **`QP-*` ids become `PARAM-*`.** Same conflict (`QP` has 2 letters); spec
   ids are preserved in titles, e.g. `(spec QP-009)`.
3. **Route anomalies** (D4) are `ROUTE-074..076`, title-prefixed
   `Route anomaly` so `uictl parity-routes` checks them as
   declared-but-unmounted instead of mounted patterns. Declared-only
   patterns *covered* by mounted ones (`/apps/applications`, `/apps/catalog`
   by ROUTE-039; `/settings/edit-configuration/:uid` superseded per
   ROUTE-065) are exempt from needing anomaly items.
4. **Query-param → route attribution** uses the file→screen map in
   `common.mjs` (`SCREEN_MAP`, derived from the ProtectedRoutes lazy-import
   table); keys read from shared components are attributed to `any`. One
   item per **distinct key × route scope**: per-route expansion first, then
   scopes that normalise to the same value (e.g. `any` and `any (shell)`)
   are MERGED into a single item (sites and ops union), so no semantic
   duplicates exist. Keys only ever *written* into generated links (e.g.
   `shared_bucket` in bucket share URLs) are `[write-only URL contract]`
   items — the parameter shape is parity-relevant even without an in-app
   reader, and their acceptance says so.
5. **Actions dedup key is (target endpoint × screen)** — the same endpoint
   dispatched from two screens is two user-visible actions. The scan covers
   every `on[A-Z]*` JSX handler prop (documented superset of
   onClick/onSubmit — confirm dialogs use onApprove/onDelete/etc.), resolves
   through same-file functions (depth ≤ 4), through custom hooks that wrap
   mutation triggers (two-pass, name-based), and through
   `const xRef = useRef(handler)` indirections. Socket-emit actions
   (chat predict, canvas edit, TTS start/stop) are included; a socket event
   is credited **only when the file contains a literal emit of that exact
   event** — `.on(...)` receive handlers are never counted as actions
   (receive-side behaviour lives in the SOCK items).
6. **Copy granularity is one item per source file** containing user-visible
   strings (510 files, 2,264 strings): finer than the sanctioned
   route-level batching, so misses cannot hide inside a big screen, and two
   orders of magnitude coarser than per-string, which would drown the
   manifest. Files whose strings are ALL assistive-technology labels
   (aria-label / alt) are marked `[aria-only]`; their verify method is the
   storybook a11y suite (unit S1), because a visual-regression suite cannot
   see them.
7. **Unit + coverage assignment is mechanical** via `API_MODULE_MAP`,
   `DOMAIN_UNIT` and `UNIT_COVERAGE` in `common.mjs` (spec §9.3 floors).
   Coverage `min: 0` on V1/V2-owned items means "not coverage-gated"
   (e2e/visual suites).
8. **SHELL item order is fixed** so the socket-connectivity indicator is the
   12th shell item — spec §5.5 references it as `SHELL-012` by name.
9. **Waived items.** `W-007` (feedback dialog ships enabled, decision D5) and
   `W-008` (`stopApplicationTask` — dead surface, decision-record contract
   correction from unit W1) are the only waived items; both carry full
   waiver objects. `API-110` (`setApplicationDefaultVersion`) stays `must`
   but its acceptance codifies the **working 3-segment router shape per
   W-009** — the old client's 2-segment call with the version in the body is
   live-broken (405) against the served router and is deliberately not
   reproduced. `API-129`/`API-134` (bucket and artifact listing) pin the
   un-prefixed `/artifacts/s3/` path explicitly in acceptance: it is a real
   routing fact (the deployment routes `/artifacts/` straight to the
   backend), not a bug.
10. **Acceptance immutability**: `uictl parity-manifest --validate` diffs
    ids + acceptance text against the committed (git HEAD) index + shards
    and fails on any deletion or acceptance change without a waiver
    (§9.4 rule 8). Before the first commit the check passes vacuously.
    **Design note:** `--validate` also enforces waiver ⇔ `priority: waived`,
    so a §9.4-rule-8 acceptance edit requires flipping the item to
    `priority: waived` with a populated waiver object — an acceptance change
    is by definition a sanctioned deviation, and this makes it impossible to
    reword acceptance while keeping the item counted in the `must` gate.
    This is intended behaviour, not an accident of implementation.

## Verify commands (run from the repo root)

```sh
go run ./tools/uictl parity-manifest --validate --baseline <abs>/apps/elitea-ui
go run ./tools/uictl parity-routes    --baseline <abs>/apps/elitea-ui
go run ./tools/uictl parity-routes    --baseline <abs>/apps/elitea-ui --new-routes <export.json>
        # second input for the new app once unit R1 exists ([]string JSON)
go test ./tools/uictl/...
bash scripts/no-binaries-check.sh     # all shards < 1 MiB
```

`verify-routes` and `diff-config` are documented not-yet-implemented
subcommands (units R1 / F3); they exit 3 with an explanation so no pipeline
can treat them as passing gates.

## Appendix — step 6 dedup mapping (for review)

One row per (endpoint × screen) action item; `sites` are the JSX handler
locations that resolved to the endpoint. Regenerate with
`node extract-actions.mjs` (emits `out/actions-dedup-mapping.json`, 222 raw
handler sites → 107 items).

| # | screen | target endpoint | handler prop(s) | call sites |
|---|---|---|---|---|
| 1 | agents | applicationCreate | onApprove, onClick | src/[fsd]/features/agent/ui/generate-agent-modal/GenerateAgentModal.jsx:328<br>src/pages/Applications/Components/Applications/CreateApplicationSaveButton.jsx:58<br>src/pages/Applications/Components/Applications/CreateApplicationTabBar.jsx:74 |
| 2 | agents | applicationEdit | onConfirm | src/pages/Applications/Components/Tools/ToolCard.jsx:649 |
| 3 | agents | deleteApplication | onDelete | src/pages/Applications/Components/Applications/DeleteApplicationButton.jsx:89 |
| 4 | agents | generateAgentDraft | onGenerate | src/[fsd]/features/agent/ui/generate-agent-modal/GenerateAgentModal.jsx:323 |
| 5 | agents | saveApplicationNewVersion | onConfirm | src/pages/Applications/Components/Applications/SaveNewVersionButton.jsx:100 |
| 6 | agents | toolkitAssociate | onApprove, onConfirm, onScroll | src/[fsd]/features/agent/ui/generate-agent-modal/GenerateAgentModal.jsx:328<br>src/pages/Applications/Components/Tools/ToolCard.jsx:649<br>src/pages/Applications/Components/Tools/ToolMenu.jsx:668 (+1 more) |
| 7 | agents | updateApplicationRelation | onApprove, onClick, onConfirm | src/[fsd]/features/agent/ui/generate-agent-modal/GenerateAgentModal.jsx:328<br>src/pages/Applications/Components/Tools/AgentPipelineVersionSelector.jsx:373<br>src/pages/Applications/Components/Tools/ToolCard.jsx:649 |
| 8 | agents | updateApplicationVersion | onDiscard | src/pages/Applications/EditApplication.jsx:80 |
| 9 | agents | updateSkillRelation | onApprove | src/[fsd]/features/agent/ui/generate-agent-modal/GenerateAgentModal.jsx:328 |
| 10 | artifacts | deleteArtifact | onConfirm, onDelete | src/[fsd]/features/artifacts/ui/FilePreviewCanvas/index.jsx:508<br>src/pages/Artifacts/component/ArtifactTable.jsx:545 |
| 11 | artifacts | deleteArtifacts | onDelete, onDeleteArtifacts | src/pages/Artifacts/component/ArtifactTable.jsx:491<br>src/pages/Artifacts/component/ArtifactTable.jsx:545 |
| 12 | artifacts | deleteBucket | onDelete | src/pages/Artifacts/Components/Buckets.jsx:221 |
| 13 | artifacts | updateBucketPin | onPin | src/pages/Artifacts/Components/Buckets.jsx:222 |
| 14 | chat | addParticipantIntoConversation | onAdd | src/pages/NewChat/NewChat.jsx:1650 |
| 15 | chat | conversationCreate | onCancelCreateConversation, onCreateConversation | src/pages/NewChat/NewChat.jsx:1388<br>src/pages/NewChat/NewChat.jsx:1389<br>src/pages/NewChat/NewChat.jsx:1455 |
| 16 | chat | conversationEdit | onEditConversation, onMoveToFolderConversation, onMoveToNewFolderConversation, onSend, onSetLLMSettings | src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2198<br>src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2236<br>src/pages/NewChat/NewChat.jsx:1381 (+3 more) |
| 17 | chat | createCanvas | onCloseCanvas, onCloseCanvasEditor | src/pages/NewChat/NewChat.jsx:1404<br>src/pages/NewChat/NewChat.jsx:1616 |
| 18 | chat | deleteConversation | onDeleteConversation | src/pages/NewChat/NewChat.jsx:1382 |
| 19 | chat | deleteFolder | onDeleteFolder | src/pages/NewChat/NewChat.jsx:1395 |
| 20 | chat | editCanvas | onChangeLanguage | src/pages/NewChat/CanvasEditor.jsx:551 |
| 21 | chat | folderCreate | onCancelCreateFolder, onCreateFolder, onMoveToFolderConversation, onMoveToNewFolderConversation | src/pages/NewChat/NewChat.jsx:1393<br>src/pages/NewChat/NewChat.jsx:1394<br>src/pages/NewChat/NewChat.jsx:1399 (+1 more) |
| 22 | chat | folderPinUpdate | onEditFolder, onPinFolder | src/pages/NewChat/NewChat.jsx:1397<br>src/pages/NewChat/NewChat.jsx:1398 |
| 23 | chat | folderUpdate | onEditFolder, onPinFolder, onReorderFolders | src/pages/NewChat/NewChat.jsx:1397<br>src/pages/NewChat/NewChat.jsx:1398<br>src/pages/NewChat/NewChat.jsx:1408 |
| 24 | chat | regenerate | onRegenerateAnswer, onSubmitEditedMessage | src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2110<br>src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2116 |
| 25 | chat | removeAttachments | onRemoveAttachment | src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2123 |
| 26 | chat | selectConversation | onCancelCreateConversation, onClose, onConfirm, onCreateConversation | src/pages/NewChat/NewChat.jsx:1388<br>src/pages/NewChat/NewChat.jsx:1389<br>src/pages/NewChat/NewChat.jsx:1455 (+2 more) |
| 27 | chat | sio:chat_canvas_edit | onChange | src/pages/NewChat/CanvasEditor.jsx:619 |
| 28 | chat | sio:chat_continue_predict | onContinueMcpExecution, onContinueTokenLimitExecution, onHitlResume | src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2111<br>src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2112<br>src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2113 |
| 29 | chat | sio:chat_predict | onSend | src/pages/NewChat/NewConversationView.jsx:912 |
| 30 | chat | sio:tts_start | onClick | src/[fsd]/features/chat/voice-config/ui/VoiceConfigControls.jsx:115 |
| 31 | chat | sio:tts_stop | onClick | src/[fsd]/features/chat/voice-config/ui/VoiceConfigControls.jsx:115 |
| 32 | chat | stopChatTask | onContinueMcpExecution, onContinueTokenLimitExecution, onHitlResume | src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2111<br>src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2112<br>src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2113 |
| 33 | chat | togglePinItem | onPinConversation | src/pages/NewChat/NewChat.jsx:1387 |
| 34 | chat | toolkitCreate | onClick, onKeyDown | src/[fsd]/features/chat/ui/chat-modal/AttachmentSettingsModal.jsx:221<br>src/[fsd]/features/chat/ui/chat-modal/AttachmentSettingsModal.jsx:312<br>src/pages/NewChat/components/CreateToolkitButton.jsx:76 |
| 35 | chat | unselectConversation | onDeleteConversation | src/pages/NewChat/NewChat.jsx:1382 |
| 36 | chat | updateApplicationVersion | onAdd | src/pages/NewChat/NewChat.jsx:1650 |
| 37 | chat | updateParticipantLlmSettings | onSelectModel, onSend, onSetLLMSettings | src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2217<br>src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:2236<br>src/pages/NewChat/NewConversationView.jsx:912 |
| 38 | chat | updateParticipantSettings | onConversationLlmOverride, onPipelineSaved, onToolkitUpdated | src/pages/NewChat/NewChat.jsx:1549<br>src/pages/NewChat/NewChat.jsx:1570<br>src/pages/NewChat/NewChat.jsx:1588 |
| 39 | credentials | batchTestConfigurationConnection | onRevalidate | src/[fsd]/features/credentials/ui/credentials-select/CredentialsSelect.jsx:260 |
| 40 | credentials | createConfiguration | onClick | src/pages/Credentials/CredentialForm.jsx:322<br>src/pages/Credentials/CredentialForm.jsx:345 |
| 41 | credentials | testConfigurationConnection | onClick, onRevalidate | src/[fsd]/features/credentials/ui/credentials-select/CredentialsSelect.jsx:260<br>src/pages/Credentials/CredentialForm.jsx:322<br>src/pages/Credentials/CredentialForm.jsx:345 |
| 42 | import-wizard | forkAgent | onClick | src/[fsd]/entities/import-wizard/ui/ImportWizardModal/IWModalForkButton.jsx:269 |
| 43 | import-wizard | importWizard | onClick | src/[fsd]/entities/import-wizard/ui/ImportWizardModal/IWModalImportButton.jsx:141 |
| 44 | import-wizard | toolkitFork | onClick | src/[fsd]/entities/import-wizard/ui/ImportWizardModal/IWModalForkButton.jsx:269 |
| 45 | indexes | addParticipantIntoConversation | onCancelIndexing, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:330<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:361<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:364 |
| 46 | indexes | conversationCreate | onCancelIndexing, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:330<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:361<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:364 |
| 47 | indexes | deleteIndexItem | onConfirm | src/[fsd]/features/toolkits/indexes/ui/IndexesContainer.jsx:218 |
| 48 | indexes | sio:chat_enter_room | onCancelIndexing, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:330<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:361<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:364 |
| 49 | indexes | sio:chat_leave_rooms | onCancelIndexing, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:330<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:361<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:364 |
| 50 | indexes | sio:chat_predict | onCancelIndexing, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:330<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:361<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:364 |
| 51 | indexes | stopIndexingItem | onCancelIndexing, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:330<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:361<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/index.jsx:364 |
| 52 | indexes | updateIndexSchedule | onChange, onSubmit | src/[fsd]/features/toolkits/indexes/ui/IndexDetails/IndexActions.jsx:205<br>src/[fsd]/features/toolkits/indexes/ui/IndexDetails/IndexActions.jsx:277 |
| 53 | notifications | notificationBulkDelete | onDeleteSelected | src/pages/NotificationCenter/NotificationTable.jsx:230 |
| 54 | notifications | notificationBulkMarkSeen | onMarkToggle | src/pages/NotificationCenter/NotificationTable.jsx:231 |
| 55 | pipelines | generateContentStreaming | onClick, onGenerate, onStop | src/[fsd]/features/pipelines/ai-assistant/ui/AIAssistantModal.jsx:306<br>src/[fsd]/features/pipelines/ai-assistant/ui/AIAssistantModal.jsx:307<br>src/[fsd]/features/pipelines/ai-assistant/ui/AIAssistantModal.jsx:364 (+1 more) |
| 56 | pipelines | stopLlmTask | onClick, onGenerate, onStop | src/[fsd]/features/pipelines/ai-assistant/ui/AIAssistantModal.jsx:306<br>src/[fsd]/features/pipelines/ai-assistant/ui/AIAssistantModal.jsx:307<br>src/[fsd]/features/pipelines/ai-assistant/ui/AIAssistantModal.jsx:364 (+1 more) |
| 57 | pipelines | updatePipelineTrigger | onClick, onSubmit, onValueChange | src/[fsd]/features/pipelines/flow-editor/ui/settings/TriggerTypeSelector.jsx:299<br>src/[fsd]/features/pipelines/flow-editor/ui/settings/TriggerTypeSelector.jsx:332<br>src/[fsd]/features/pipelines/flow-editor/ui/settings/TriggerTypeSelector.jsx:344 (+1 more) |
| 58 | settings | createConfiguration | onBlur, onClick | src/[fsd]/features/settings/ui/environment/EnvironmentSection.jsx:222<br>src/[fsd]/features/settings/ui/system-prompts/ServicePromptsSection.jsx:526 |
| 59 | settings | deleteProjectIcon | onDelete | src/[fsd]/features/settings/ui/project-context/SelectProjectIconDialog.jsx:232 |
| 60 | settings | generateProjectContextDraft | onGenerate | src/[fsd]/features/settings/ui/project-context/GenerateProjectContextModal.jsx:66 |
| 61 | settings | setProjectDefaultModel | onChangeDefaultModel | src/[fsd]/features/settings/ui/ai-configuration/Configuration/ModelConfiguration.jsx:268 |
| 62 | settings | tokenDelete | onDelete | src/[fsd]/features/settings/ui/personal-tokes/TokensTable.jsx:190 |
| 63 | settings | updateConfiguration | onBlur, onClick, onRestore | src/[fsd]/features/settings/ui/environment/EnvironmentSection.jsx:222<br>src/[fsd]/features/settings/ui/environment/EnvironmentSection.jsx:223<br>src/[fsd]/features/settings/ui/system-prompts/ServicePromptsSection.jsx:441 (+2 more) |
| 64 | settings | updateProjectContext | onClick | src/[fsd]/features/settings/ui/project-context/ProjectContextContent.jsx:284 |
| 65 | settings | updateProjectIcon | onChange, onClick, onDelete | src/[fsd]/features/settings/ui/project-context/SelectProjectIconDialog.jsx:191<br>src/[fsd]/features/settings/ui/project-context/SelectProjectIconDialog.jsx:202<br>src/[fsd]/features/settings/ui/project-context/SelectProjectIconDialog.jsx:231 (+2 more) |
| 66 | settings | uploadProjectIcon | onChange | src/[fsd]/features/settings/ui/project-context/SelectProjectIconDialog.jsx:257 |
| 67 | settings | userDelete | onConfirm | src/[fsd]/features/settings/ui/users/DeleteUserButton.jsx:87 |
| 68 | settings-personalization | authorDescription | onSubmit | src/pages/UserSettings/Profile.jsx:56 |
| 69 | settings-users | userCreate | onConfirm | src/[fsd]/pages/settings/Users.jsx:267 |
| 70 | shared | createModerationRequest | onSubmit | src/[fsd]/features/apps/ui/catalog/ApplicationCatalog.jsx:78 |
| 71 | shared | deleteApplicationVersion | onCancel, onClick, onClose, onConfirm, onReplace | src/[fsd]/entities/version/ui/VersionDelete.jsx:125<br>src/[fsd]/entities/version/ui/VersionDelete.jsx:139<br>src/[fsd]/entities/version/ui/VersionDelete.jsx:151 (+4 more) |
| 72 | shared | generateContentBlocking | onQuickFix | src/components/MermaidCodeBlock.jsx:372 |
| 73 | shared | likeApplication | onClick | src/components/Like.jsx:65 |
| 74 | shared | notificationBulkMarkSeen | onClick | src/[fsd]/entities/notifications/ui/NotificationListItem.jsx:119 |
| 75 | shared | publishApplication | onAgreedChange, onCategoryChange, onClose, onContinue, onPublish, onVersionNameChange | src/[fsd]/entities/version/lib/hooks/usePublishVersionMenu.hooks.jsx:41<br>src/[fsd]/entities/version/lib/hooks/usePublishVersionMenu.hooks.jsx:43<br>src/[fsd]/entities/version/lib/hooks/usePublishVersionMenu.hooks.jsx:46 (+3 more) |
| 76 | shared | setApplicationDefaultVersion | onConfirm | src/[fsd]/entities/version/lib/hooks/useSetDefaultVersion.hooks.jsx:101 |
| 77 | shared | sio:chat_canvas_edit | onQuickFix | src/components/MermaidCodeBlock.jsx:372 |
| 78 | shared | testConfigurationConnection | onClick | src/[fsd]/features/openapi/ui/OpenApiDelegatedLoginButton.jsx:53<br>src/[fsd]/features/sharepoint/ui/SharepointDelegatedLoginButton.jsx:53 |
| 79 | shared | toolkitFork | onClick | src/components/Fork/ForkEntityButton.jsx:108 |
| 80 | shared | unlikeApplication | onClick | src/components/Like.jsx:65 |
| 81 | shared | unpublishApplication | onConfirm | src/[fsd]/entities/version/lib/hooks/useUnpublishVersionMenu.hooks.jsx:136 |
| 82 | shared | validateForPublish | onAgreedChange, onCategoryChange, onClose, onContinue, onPublish, onVersionNameChange | src/[fsd]/entities/version/lib/hooks/usePublishVersionMenu.hooks.jsx:41<br>src/[fsd]/entities/version/lib/hooks/usePublishVersionMenu.hooks.jsx:43<br>src/[fsd]/entities/version/lib/hooks/usePublishVersionMenu.hooks.jsx:46 (+3 more) |
| 83 | shared-page-components | deleteApplication | onDelete | src/pages/Common/Components/DeleteButton.jsx:107 |
| 84 | shell-widgets | deleteSummary | onDelete | src/[fsd]/widgets/context-budget/ui/SummaryDetailsModal.jsx:158 |
| 85 | shell-widgets | notificationBulkMarkSeen | onClick | src/[fsd]/widgets/Notifications/ui/NotificationList.jsx:151 |
| 86 | shell-widgets | updateSummary | onEdit | src/[fsd]/widgets/context-budget/ui/SummaryDetailsModal.jsx:159 |
| 87 | sidebar | feedback | onClick | src/[fsd]/widgets/sidebar-root/ui/FeedbackDialog.jsx:145 |
| 88 | skills | generateContentStreaming | onRegenerateAnswer, onSend, onSubmitEditedMessage | src/[fsd]/features/skill/ui/skill-test-panel/SkillTestPanel.jsx:475<br>src/[fsd]/features/skill/ui/skill-test-panel/SkillTestPanel.jsx:476<br>src/[fsd]/features/skill/ui/skill-test-panel/SkillTestPanel.jsx:488 |
| 89 | skills | generateSkillDraft | onGenerate | src/[fsd]/features/skill/ui/generate-skill-modal/GenerateSkillModal.jsx:105 |
| 90 | skills | setSkillDefaultVersion | onConfirm | src/[fsd]/pages/skills/EditSkill.jsx:267 |
| 91 | skills | skillCreate | onApprove, onClick | src/[fsd]/features/skill/ui/CreateSkillTabBar.jsx:130<br>src/[fsd]/features/skill/ui/generate-skill-modal/GenerateSkillModal.jsx:109 |
| 92 | skills | skillCreateVersion | onConfirm | src/[fsd]/features/skill/ui/SaveSkillVersionButton.jsx:98 |
| 93 | skills | skillImport | onClick, onClose, onConfirm | src/[fsd]/features/skill/ui/import/SkillImportButton.jsx:37<br>src/[fsd]/features/skill/ui/import/SkillImportButton.jsx:49<br>src/[fsd]/features/skill/ui/import/SkillImportButton.jsx:50 |
| 94 | skills | skillUpdate | onClick | src/[fsd]/features/skill/ui/SaveSkillButton.jsx:40 |
| 95 | skills | stopLlmTask | onStopGeneration | src/[fsd]/features/skill/ui/skill-test-panel/SkillTestPanel.jsx:490 |
| 96 | skills | updateSkillRelation | onClick, onConfirm | src/[fsd]/features/skill/ui/SkillCard.jsx:140<br>src/[fsd]/features/skill/ui/SkillVersionSelector.jsx:121 |
| 97 | toolkits | addParticipantIntoConversation | onClear, onRunTool, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:195<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:236<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:239 (+1 more) |
| 98 | toolkits | conversationCreate | onClear, onRunTool, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:195<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:236<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:239 (+1 more) |
| 99 | toolkits | createConfiguration | onCreateConfiguration, onTestConnection | src/[fsd]/features/toolkits/ui/form/ToolkitForm/ToolkitForm.jsx:527<br>src/[fsd]/features/toolkits/ui/form/ToolkitForm/ToolkitForm.jsx:528 |
| 100 | toolkits | mcpSyncTools | onClick | src/[fsd]/features/toolkits/ui/form/ToolBase/ToolActionsSelector.jsx:111 |
| 101 | toolkits | sio:chat_enter_room | onClear, onRunTool, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:195<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:236<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:239 (+1 more) |
| 102 | toolkits | sio:chat_leave_rooms | onClear, onRunTool, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:195<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:236<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:239 (+1 more) |
| 103 | toolkits | sio:chat_predict | onClear, onRunTool, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:195<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:236<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:239 (+1 more) |
| 104 | toolkits | stopIndexingItem | onClear, onRunTool, onSelectModel, onSetLLMSettings | src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:195<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:236<br>src/[fsd]/features/toolkits/ui/test-tools/TestTools.jsx:239 (+1 more) |
| 105 | toolkits | testConfigurationConnection | onCreateConfiguration, onTestConnection | src/[fsd]/features/toolkits/ui/form/ToolkitForm/ToolkitForm.jsx:527<br>src/[fsd]/features/toolkits/ui/form/ToolkitForm/ToolkitForm.jsx:528 |
| 106 | toolkits | toolkitDelete | onDelete | src/pages/Toolkits/DeleteToolkitButton.jsx:96 |
| 107 | toolkits | toolkitEdit | onClick | src/pages/Toolkits/SaveToolkitButton.jsx:94 |
