# Pipeline UI Wave U2: EL-3734 RunStateDialog Timeline Grouping Building Blocks

- **Date:** 2026-11-22
- **Branch:** `feat/fu283-u2-run-state-timeline` (base `4f9d63873`)
- **Reference Source:** `EliteaUI` `ec684f56b` (`feat: [EL-3734] Pipeline Run UI: grouping (#1585)`)
- **Status:** `foundation` (standalone non-intrusive building blocks ready to wire into `RunStateDialog` in Wave U2 integration)

## 1. Behavior Ported from `EliteaUI` (`EL-3734` `#1585`)

| Legacy Source (`EliteaUI` `ec684f56b`) | Replatform Ownership (`apps/elitea-web`) | Notes |
|---|---|---|
| `flow-editor/lib/constants/timeLine.constants.js` | `src/features/pipelines/lib/run-state-timeline/timeline.constants.ts` | Step/connector/name widths, `GROUP_TOGGLE_BUTTON_HEIGHT`, `MAX_BADGE_COUNT = 99`, plus replatform hierarchy depth (`3`) & `node_errors` scan cap (`64`). |
| `flow-editor/lib/helpers/timeLine.helpers.js` | `src/features/pipelines/lib/run-state-timeline/timelineGrouping.helpers.ts` | `groupConsecutiveSteps`, `getVisibleSteps` (XOR expand-all/override semantics), `getActiveRowPosition`, `formatBadgeCount`, `isRowActive`, plus `findPreviousScopedStep` & `findLastOuterStepIndex`. |
| `flow-editor/lib/helpers/parseRunsByEvent.helpers.js` (grouping/nesting delta) | `src/features/pipelines/lib/run-state-timeline/timelineNormalization.helpers.ts` | Pure `normalizeTimelineHierarchy` + `normalizeRunEventsToGroupedTimeline` grouping nested applications (`application:<call_id>`), Map items (`map:<node>:<idx>`), Parallel branches (`parallel:<branch_id>`), loop iterations (`iteration:<node>:<idx>`), and task namespaces (`task:<ns>`), with `scope_id` vs `group_id` separation. |
| `flow-editor/lib/hooks/useTimeLineGroups.hooks.js` | `src/features/pipelines/lib/run-state-timeline/useTimeLineGroups.ts` | Expand-all & per-group toggle state with ref-stabilized callbacks and automatic selection hand-off to group head on collapse. |
| `flow-editor/ui/state/timeline/TimeLine.jsx` | `src/features/pipelines/ui/run-state-timeline/TimeLine.tsx` | Composes header, scrollable `<section>` `Stepper`, grouped `TimeLineStep` rows, and structured `TimeLineContinuedFailureSummary`. |
| `flow-editor/ui/state/timeline/TimeLineGroupToggle.jsx` | `src/features/pipelines/ui/run-state-timeline/TimeLineGroupToggle.tsx` | Uses `@/shared/ui/BaseBtn` (`variant="icon"`), `ExpandIcon`/`CollapseIcon`, `Tooltip`, `aria-label`, and `aria-expanded`. |
| `flow-editor/ui/state/timeline/TimeLineExpandAllSwitch.jsx` | `src/features/pipelines/ui/run-state-timeline/TimeLineExpandAllSwitch.tsx` | Uses `@/shared/ui/BaseSwitch` with `role="switch"`, accessible label, and `data-testid="pipeline-run-details-timeline-expand-all"`. |
| `flow-editor/ui/state/timeline/{TimeLineHeader,TimeLineRunStatus,TimeLineStep,TimeLineStepName,TimeLineStepLabel,ProcessStepIcon}.jsx` | `src/features/pipelines/ui/run-state-timeline/{TimeLineHeader,TimeLineRunStatus,TimeLineStep,TimeLineStepName}.tsx` + `runStateTimeline.styles.ts` | Uses `@/shared/ui/EllipsisTypography`, semantic `<button type="button">` step dots, `theme.vars.*` tokens, and `@/shared/i18n` keys. |

## 2. Replatform Invariants Preserved

- **Non-intrusive delivery:** Zero edits to mounted `EditorPanel`, `RunStateDialog`, `ConfigurationTab`, `knip.json`, or top-level `src/features/pipelines/index.ts`. Exposed via subdirectory barrels `src/features/pipelines/lib/run-state-timeline/index.ts` and `src/features/pipelines/ui/run-state-timeline/index.ts` (`knip.json` L64 `src/features/**/index.ts`).
- **5b-1 D12 `node_errors` (`"Failed · continued"`):** `timelineNodeErrors.helpers.ts` (`extractContinuedFailuresByStep`, `hasContinuedFailureInRange`, `collectContinuedFailuresInRange`) preserves deduplication by `[node, input_step, support_ref, handled]` and nearest-prior-step attribution, surfaces structured `TimelineContinuedFailure` fields (`node`, `handled`, `inputStep`, `supportRef`, `code`, `message`), marks collapsed group dots with `data-status="failed-continued"`, and renders structured details in `TimeLineContinuedFailureSummary` via plain React text nodes.
- **Zero Rust/Go/CI/dependency changes:** Pure `apps/elitea-web` + docs only; `package.json` / `package-lock.json` untouched.

## 3. Verification Evidence

- **Vitest (`node` project, Node 24):**
  - Targeted new suites: `7` test files, `33/33` tests passed (`timelineGrouping.helpers.test.ts`, `timelineNodeErrors.helpers.test.ts`, `timelineNormalization.helpers.test.ts`, `useTimeLineGroups.test.ts`, `TimeLineGroupToggle.test.tsx`, `TimeLineExpandAllSwitch.test.tsx`, `TimeLine.test.tsx`).
  - Regression check with existing `ui/state/*` + `parseRunsByEvent.helpers.test.ts`: `26` test files, `216/216` tests passed.
- **Static & Policy Gates (Node 24):**
  - `npm run typecheck`: PASS (`0` errors).
  - `npm run lint -- --deny-warnings`: PASS (`0` warnings, `0` errors across `6603` files).
  - `npm run theme-gate`: PASS (all checks including `7-brand-pack`).
  - `node scripts/check-budgets.mjs`: PASS (`7380` files within §3.5 budgets).
  - `node scripts/check-testid-namespace.mjs`: PASS (`2851` test IDs in one kebab-case namespace).
  - `node scripts/check-layer-cycle.mjs`: PASS (`0` errors, `0` warnings).
  - `node scripts/check-dead-code.mjs`: PASS (Knip found no issues).
  - `node scripts/i18n-backfill.mjs --check`: PASS (7 new keys synced in `en.json`).
  - `npm run check-handlers`: PASS.
- **Real-browser Chrome verification:** Deferred to Wave U2 integration when `TimeLine` is mounted inside `RunStateDialog`.

## 4. Performance, Durability, Resilience & Security

- **Performance:** Linear `O(N)` grouping (`groupConsecutiveSteps`) and bounded `node_errors` scanning (`MAX_NODE_ERRORS_SCAN = 64` per step); memoized group/row projections and ref-stabilized handlers prevent re-render churn during live runs.
- **Durability / Resilience:** Pure client-side presentation and event-normalization layer (N/A for server persistence); malformed `parent_agent_path` payloads, over-depth hierarchies (`> 3`), oversized identifiers, and invalid timestamps degrade cleanly without throwing.
- **Security (`security.md`):**
  - **XSS / HTML injection:** Zero `dangerouslySetInnerHTML` or raw HTML interpolation; `node_errors` messages, `supportRef`, `group_label`, and step names are rendered exclusively as React text nodes (verified with `<img src=x onerror=alert(1)>` payload in `TimeLine.test.tsx`).
  - **Bounds & Input Validation:** Hierarchy depth capped at `MAX_HIERARCHY_DEPTH = 3`, string fields bounded (`512` / `2048` chars), and `node_errors` scan capped at `64` entries per step.
  - **Secrets / Auth / Network:** N/A (pure client-side UI/helpers; zero network requests, zero secret material; `git diff` secret scan = `0` hits).
