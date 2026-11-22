# Pipeline UI Grouped Tool Picker Module (FU-283 Wave U1)

- **Date:** 2026-11-20
- **Track:** `ui-u1-picker` (`feat/fu283-u1-tool-picker`, base `origin/main` `5f21740a8`)
- **Reference UI:** `EliteaUI` `origin/main` (`46d85cda7`), `src/[fsd]/features/pipelines/tool-picker/*`, `src/[fsd]/shared/lib/helpers/agentNesting.helpers.js` (`EL-7085` / `#1559`), and `src/[fsd]/features/toolkits/{lib/hooks/useToolValidation.hooks.js,ui/tool-validation-banner/ToolValidationBanner.jsx}` (`EL-7516` / `#1593`).

---

## 1. Business Behavior Ported vs. Intentionally Not Ported

### Ported from `EliteaUI` (`46d85cda7`)
- **Pure helpers (`apps/elitea-web/src/features/pipelines/lib/tool-picker/`):**
  - `toolPicker.constants.ts`: `APPLICATION_TOOL_TYPE`, `TOOL_REF_KINDS`, `APPLICATION_REF_NODE_TYPES`, `PICKER_KINDS`, `PICKER_PAGE_SIZE`, `PICKER_SEARCH_DEBOUNCE`, `IDEMPOTENT_RELATION_ERRORS`, `PIPELINE_TOOL_KIND_ORDER`, `PIPELINE_TOOL_CARD_ACTION_CLASS`, `LOCKED_VERSION_STATUSES`.
  - `agentNesting.helpers.ts` (**`EL-7085` / `#1559`**): `SWARM_REASON`, `NESTING_REASON`, `AGENT_HOST_TIER`, `PIPELINE_HOST_TIER`, `wouldExceedAgentNestingDepth`, `getNestingDisabledReason`, `getAgentDisabledReason`.
  - `pipelineToolUsage.helpers.ts`: `isApplicationTool`, `isMcpAttachment`, `getToolRefKey`, `getReferencedNames`, `getAttachmentRefName`, `getAttachmentRefKey`, `getToolkitDisplayName`, `findAttachedToolkit`, `collectToolReferences`, `getAttachmentReference`, `getAttachmentKind`, `groupAttachmentsByKind`.
  - `pickerItems.helpers.ts`: `buildToolkitPickerItems` (pinned in-use first, duplicate-name disabling) and `buildApplicationPickerItems` (self-exclusion, `has_interrupt`, `EL-7085` Swarm + nesting-depth reasons, duplicate-name disabling).
  - `attachmentTool.helpers.ts`: `buildToolkitAttachment`, `buildApplicationAttachment`, `withApplicationVersion`, `getToolkitAvailableTools`, `getAttachmentPagePath`.
  - `attachmentDiff.helpers.ts`: `getAttachmentKey`, `isSameToolSet`, `diffAttachments`.
  - `attachmentReconcile.helpers.ts`: `reconcileAttachments` (pure, idempotent; drops unreferenced session/previously-used attachments, preserves legacy unused, narrows `selected_tools` only when usage changes).
- **Hooks (`apps/elitea-web/src/features/pipelines/lib/tool-picker/`):**
  - `usePickerMenu.ts`, `usePagedPickerList.ts`, `useProjectApplicationsList.ts`, `useProjectToolkitsList.ts`, `useCreateInNewTab.ts`, `useIsVersionLocked.ts`, `usePipelineToolAttachments.ts`, `useApplyPipelineAttachments.ts`, `useReconcilePipelineAttachments.ts`.
  - Wired to `apps/elitea-web`'s TanStack Query generated hooks (`useListApplications`, `useGetApplication`, `useListToolkitInstances`, `getToolkit`, `getApplication`, `updateApplicationRelation`), `eliteaFetch` (`PATCH /elitea_core/tool/prompt_lib/{projectId}/{toolkitId}`), `unwrapListPage`/`unwrapBody`, `useDebounce`, `useSelectedProjectId`, `useToolkitTypeSchemas`, `useGetToolkitNameFromSchema`, and `useIsMcpVisible`.
- **UI components (`apps/elitea-web/src/features/pipelines/ui/tool-picker/`):**
  - `PickerEntityIcon.tsx` (using `ToolkitTypeIcon`, `McpIcon`, `ApplicationsIcon`, `FlowIcon`), `PickerMcpStatus.tsx`, `PickerItemRow.tsx` (including `EL-7085` disabled tooltip explanation), `PickerTrigger.tsx`, `PickerMenu.tsx`, `AgentVersionSelect.tsx`, `ProjectAgentPicker.tsx`, `ProjectToolkitPicker.tsx`, `ProjectToolkitsMultiPicker.tsx`, `PipelineToolCardActions.tsx`, `PipelineToolVersionLabel.tsx`, `PipelineToolValidationBanner.tsx` (**`EL-7516` / `#1593`**), `PipelineToolCard.tsx`, `PipelineToolsSection.tsx`.

### Deliberately Not Ported / Replatform Adaptations
- **No Formik / Redux ambient state:** `apps/elitea-web` has no Formik or Redux; form state (`tools`, `onToolsChange`, `versionStatus`, `yamlJsonObject`) is passed via explicit props/context objects (`ProjectAgentPickerContext`, `ProjectToolkitPickerContext`, `ProjectToolkitsMultiPickerContext`), matching existing `features/pipelines` conventions.
- **No sideways feature imports (`no-sideways-features`):** `PipelineToolValidationBanner` reads from `@/entities/toolkit` (`toolkitValidation.useToolkitValidationInfo`) and `@/entities/credential` (`CredentialWarningBanner`) plus an optional `validationOverride` prop instead of importing `features/toolkits` or `features/agents`. `PickerMcpStatus` accepts `onLoginClick` / `renderLoginAction` instead of importing `features/mcps`.
- **No legacy `window.open` without `noreferrer` and no query-string name/viewMode leakage:** `useCreateInNewTab` and `PipelineToolCardActions` open same-origin validated paths with `'noopener,noreferrer'` and omit query parameters from entity links.
- **Standalone Wave U1 module:** Existing `ui/select/*`, `ui/nodes/*`, `ui/settings/*`, `NodeSettings.tsx`, `FlowEditor.tsx`, and `EditorPanel.tsx` are untouched; Wave U2 plugs this module into the redesigned Node Panel.

---

## 2. Changed Paths

- `apps/elitea-web/src/features/pipelines/lib/tool-picker/index.ts` (L1-L108)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/toolPicker.types.ts` (L1-L157)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/toolPicker.constants.ts` (L1-L45)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/agentNesting.helpers.ts` (L1-L65)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/pipelineToolUsage.helpers.ts` (L1-L185)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/pickerItems.helpers.ts` (L1-L157)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/attachmentTool.helpers.ts` (L1-L115)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/attachmentDiff.helpers.ts` (L1-L61)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/attachmentReconcile.helpers.ts` (L1-L97)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/usePickerMenu.ts` (L1-L41)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/usePagedPickerList.ts` (L1-L109)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/useProjectApplicationsList.ts` (L1-L69)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/useProjectToolkitsList.ts` (L1-L67)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/useCreateInNewTab.ts` (L1-L32)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/useIsVersionLocked.ts` (L1-L13)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/usePipelineToolAttachments.ts` (L1-L195)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/useApplyPipelineAttachments.ts` (L1-L221)
- `apps/elitea-web/src/features/pipelines/lib/tool-picker/useReconcilePipelineAttachments.ts` (L1-L64)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/index.ts` (L1-L47)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PickerEntityIcon.tsx` (L1-L86)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PickerMcpStatus.tsx` (L1-L86)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PickerItemRow.tsx` (L1-L139)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PickerTrigger.tsx` (L1-L199)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PickerMenu.tsx` (L1-L267)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/AgentVersionSelect.tsx` (L1-L169)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/ProjectAgentPicker.tsx` (L1-L216)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/ProjectToolkitPicker.tsx` (L1-L254)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/ProjectToolkitsMultiPicker.tsx` (L1-L299)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PipelineToolCardActions.tsx` (L1-L91)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PipelineToolVersionLabel.tsx` (L1-L45)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PipelineToolValidationBanner.tsx` (L1-L115)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PipelineToolCard.tsx` (L1-L296)
- `apps/elitea-web/src/features/pipelines/ui/tool-picker/PipelineToolsSection.tsx` (L1-L147)
- `apps/elitea-web/src/shared/i18n/en.json` (backfilled `pipelines.toolPicker.*` keys via `scripts/i18n-backfill.mjs`)
- 10 unit/component test files under `lib/tool-picker/*.test.ts(x)` and `ui/tool-picker/*.test.tsx`

---

## 3. Tests & Static Gates

- **Targeted `vitest` (`lib/tool-picker` + `ui/tool-picker`, 10 files):** **42 passed / 0 failed / 0 skipped** (all MSW-backed, zero `vi.mock`, zero nested-`<button>` or MUI out-of-range warnings).
- **`npm run typecheck`:** PASS (0 errors).
- **`npm run lint -- --deny-warnings`:** PASS (0 warnings, 0 errors across 6,539 files).
- **`npm run theme-gate`:** PASS (all 9 checks + 5/5 brand-pack contract tests).
- **`node scripts/check-budgets.mjs`:** PASS.
- **`node scripts/check-testid-namespace.mjs`:** PASS.
- **`node scripts/check-layer-cycle.mjs`:** PASS.
- **`node scripts/check-dead-code.mjs` (Knip):** PASS.
- **`node scripts/i18n-backfill.mjs --check`:** PASS.
- **`npm audit`:** Offline in sandbox (`ENOTFOUND registry.npmjs.org`); **0 dependencies added or changed**.
- **Secret scan (`/usr/bin/grep` on diff):** Clean.

---

## 4. Performance, Durability, Resilience & Security

- **Performance:**
  - *Mechanism:* Lazy list loading (`skip: !hasOpened` in `ProjectAgentPicker.tsx:86-97`, `ProjectToolkitPicker.tsx:120`, `ProjectToolkitsMultiPicker.tsx:167-168`), bounded page size (`PICKER_PAGE_SIZE = 20` in `toolPicker.constants.ts:27`), 300ms debounced search (`usePickerMenu.ts:21`), and synchronous page-0 reset on query/project change (`usePagedPickerList.ts:36-39`).
  - *Proving test:* `lib/tool-picker/hooks.test.tsx` (`usePickerMenu` `hasOpened` gating), `ui/tool-picker/ProjectPickers.test.tsx` (queries fire only on trigger click).
- **Durability:**
  - *N/A (pure Web UI authoring library):* No server-side state or checkpoints are written; `useApplyPipelineAttachments.ts:144-215` applies backend relation mutations idempotently so retries on Save are safe (`hooks.test.tsx` proves 404 `'No such toolkit'` removal tolerance).
- **Resilience:**
  - *Mechanism:* Bounded nesting-depth checks (`agentNesting.helpers.ts:29-39`), explicit error + Retry UI in `PickerMenu.tsx:120-137`, idempotent relation reconciliation (`attachmentReconcile.helpers.ts:52-97` returning same reference when unchanged), and fail-safe YAML parse (`useReconcilePipelineAttachments.ts:9-16`).
  - *Proving tests:* `agentNesting.helpers.test.ts` (limit & limit+1), `attachmentReconcile.helpers.test.ts` (idempotency), `PickerPrimitives.test.tsx` (error/retry UI), `hooks.test.tsx` (idempotent error handling).
- **Security (`rules/security.md` category status):**
  - *Trust boundaries / identity & Authorization:* N/A (Web client only; backend enforces project membership and published-version locks; client `useIsVersionLocked` is UX-only).
  - *Input, parsing & amplification:* Bounded `PICKER_PAGE_SIZE = 20`; no `dangerouslySetInnerHTML` or raw HTML rendering.
  - *Injection & URL/path encoding:* Enforced in `attachmentTool.helpers.ts:4-11,95-110` (`SAFE_SEGMENT_RE = /^[A-Za-z0-9_-]+$/` + `encodeURIComponent`) and `useCreateInNewTab.ts:3,22-27` (refuses protocols, `//`, `..`, `%2f`, CRLF; opens with `'noopener,noreferrer'`). Proven by `attachmentTool.helpers.test.ts` and `hooks.test.tsx`.
  - *Secrets & privacy:* No tokens, credentials, or query-string secrets in URLs (`PipelineToolCardActions.tsx:39-42` opens clean path with `noopener,noreferrer`).
  - *Egress & SSRF:* N/A (browser-only same-origin API calls via `eliteaFetch`).
  - *Supply chain:* Zero dependencies added or modified.

### Recovery-Guarantee Rows (§2)
| Component × Phase | Class | Enforcing Code | Proving Test |
|---|---|---|---|
| Web/browser × authoring (attachment Save retry) | **I — Idempotent retry** | `apps/elitea-web/src/features/pipelines/lib/tool-picker/useApplyPipelineAttachments.ts:28-36,144-215` | `apps/elitea-web/src/features/pipelines/lib/tool-picker/hooks.test.tsx` |
| Web/browser × authoring (YAML reconcile) | **I — Idempotent retry** | `apps/elitea-web/src/features/pipelines/lib/tool-picker/attachmentReconcile.helpers.ts:52-97` | `apps/elitea-web/src/features/pipelines/lib/tool-picker/attachmentReconcile.helpers.test.ts` |

---

## 5. Real-Browser Evidence & Follow-Ups

- **Fixtures:** All unit/component tests use in-process MSW handlers (`src/test/setup.ts`) and React Query/Router test harnesses (`features/pipelines/__tests__/testUtils.tsx`).
- **Real-browser Chrome proof:** Deferred to Wave U1 / Wave U2 integration per `WAVE-U1-PLAN.md` §3.5 (this PR delivers the standalone `tool-picker` library not yet mounted on a route until Wave U2's Node Panel wires it in).
- **Follow-ups:**
  1. Wave U2: mount `ProjectAgentPicker`, `ProjectToolkitPicker`, `ProjectToolkitsMultiPicker`, `AgentVersionSelect`, and `PipelineToolsSection` in the redesigned `NodeConfigPanel` and `EditorPanel`, and inject host `McpLogInButton` / version-level validation errors.
  2. Run `npm audit` in an online CI environment (0 dependencies added by this PR).
