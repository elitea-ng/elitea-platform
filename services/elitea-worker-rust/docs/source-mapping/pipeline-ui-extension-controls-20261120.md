# Pipeline UI: Graph Extensions & Parallel Design-System Controls + FU-286 (2026-11-20)

- **Track:** FU-283 Wave U1 (`ui-u1-ext`)
- **Branch:** `feat/fu283-u1-extension-design-system` (based on `origin/main` `5f21740a8`)
- **Reference UI:** `EliteaUI` `origin/main` (`46d85cda7`) — `Map`, `Parallel`, `SplitOut`, and `Aggregate` are replatform-only nodes whose settings controls must use the `apps/elitea-web` `@/shared/ui` design-system vocabulary (`InputBase`, `SingleSelect`, `BaseBtn`, `BasicAccordion`, `BaseCheckbox`).

---

## 1. Business Behavior & Replatform Invariants

### Delivered
- **Gap §3.6 (Design-system controls):** Replaced all raw MUI `TextField`, native `<select>` / `<option>`, raw `Button`, and raw `Accordion` / `AccordionSummary` / `AccordionDetails` in `graph-extensions/*` and `parallel/*` with existing `@/shared/ui` wrappers (`InputBase`, `SingleSelect`, `BaseBtn`, `BasicAccordion`), preserving `className="nodrag nopan nowheel"` drag guards, accessible labels, helper text, and disabled playback states.
- **FU-286 (Empty choice i18n key leak):** Fixed `ExtensionChoice` calling `t('pipelines.graphExtensions.values.', '')` when `choices` includes `''` (which caused i18next to render the raw key `pipelines.graphExtensions.values.`). Empty choices now map explicitly to `{ value: '', label: emptyLabel ?? t('shared.ui.singleSelect.placeholder', 'None') }` without calling the dynamic value key.

### Preserved Replatform Behavior
- Zero changes to YAML serialization, schema admission, opaque foreign field retention, state key ordering, v1→v2 `Map` reduction migration, `Parallel` `on_error`/`join`/`error_policy` policy semantics, or `Code` child family rules.
- Unknown/invalid authored select values (e.g. `authored_unknown`, `unsupported`, `fail_fast`) remain visibly rendered in `SingleSelect` without silently mutating YAML.
- Zero edits to `*Node.tsx` components, `NodeSettings.tsx`, `RunStateDialog.tsx` (`EL-3734` moved post-PR B), `TriggerTypeSelector.tsx`, `PipelineWebhookModal.tsx`, `en.json`, Rust, Go, or Docker files.

---

## 2. Changed Paths (`path:line`)

| Path | Lines | Change |
| --- | --- | --- |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/ExtensionFields.tsx` | `L1-L40` | Migrated `ExtensionField` to `InputBase` (with `expand` for multiline); migrated `ExtensionChoice` to `SingleSelect` + `FormHelperText` inside `Box.nodrag.nopan.nowheel`; fixed FU-286 in `choiceOption` (`L24-L27`). |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/StateChannelSelect.tsx` | `L1-L40` | Migrated native `<select>` to `SingleSelect` + `FormHelperText` inside `Box.nodrag.nopan.nowheel`, preserving `Select a state variable` empty option and unknown authored values. |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/ExtensionLimits.tsx` | `L1-L31` | Replaced raw MUI `Accordion`/`AccordionSummary`/`AccordionDetails` with `BasicAccordion` (`defaultExpanded={false}`, `uppercase={false}`) inside `Box.nodrag.nopan.nowheel`. |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/MapSettings.tsx` | `L1-L124` | Replaced 4 raw `Button` controls with `BaseBtn` (`secondary`/`text`, `size="small"`, `nodrag nopan nowheel`) and wrapped `On Error` / `Join` `SingleSelect`s in `Box.nodrag.nopan.nowheel`. |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/MapWorkerInputs.tsx` | `L1-L72` | Replaced raw `Button` with `BaseBtn` (`variant="text"`, `size="small"`, `nodrag nopan nowheel`); added `nowheel` to `BaseCheckbox`. |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/AggregateGrouping.tsx` | `L1-L36` | Replaced 2 raw `Button` controls with `BaseBtn` (`text`/`secondary`, `size="small"`, `nodrag nopan nowheel`). |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/AggregateOperations.tsx` | `L1-L72` | Replaced 2 raw `Button` controls with `BaseBtn` (`text`/`secondary`, `size="small"`, `nodrag nopan nowheel`); added `nowheel` to `BaseCheckbox`. |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/SplitRetentionFields.tsx` | `L1-L53` | Replaced 2 raw `Button` controls with `BaseBtn` (`text`/`secondary`, `size="small"`, `nodrag nopan nowheel`). |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/SplitOutSettings.tsx` | `L48-L50` | Added `nowheel` to `BaseCheckbox` drag guard (`nodrag nopan nowheel`). |
| `apps/elitea-web/src/features/pipelines/ui/settings/parallel/ParallelBranches.tsx` | `L1-L94` | Replaced 5 raw `Button` controls with `BaseBtn` (`text`/`secondary`, `size="small"`, `nodrag nopan nowheel`). |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/ExtensionFields.test.tsx` | `L1-L162` | New co-located TDD unit tests for `ExtensionChoice` (including FU-286 regression), `ExtensionField`, `StateChannelSelect`, and `ExtensionLimits`. |
| `apps/elitea-web/src/features/pipelines/ui/settings/graph-extensions/MapSettings.policy.test.tsx` | `L55-L106` | Migrated `Reduction` / `Owned worker` queries to `SingleSelect` and added FU-286 regression assertion (`['None', 'worker', 'drafter']`). |
| `apps/elitea-web/src/features/pipelines/ui/settings/parallel/ParallelSettings.test.tsx` | `L1-L185` | Migrated native `<select>` queries/actions to `SingleSelect` (`role="combobox"` / `listbox` / `option`, `aria-disabled="true"`). |
| `apps/elitea-web/src/features/pipelines/ui/nodes/{MapNode,SplitOutNode,AggregateNode,ParallelNode}.test.tsx` | — | Coordinator-approved test-only migration from native `<select>` DOM queries to `SingleSelect` interactions; all policy/admission assertions preserved verbatim. |

---

## 3. Verification, Tests & Static Gates

### Unit Tests (`vitest --project node`)
- **Targeted suite (8 files):** `ExtensionFields.test.tsx` (6), `MapSettings.policy.test.tsx` (4), `ParallelSettings.test.tsx` (13), `MapNode.test.tsx` (7), `SplitOutNode.test.tsx` (13), `AggregateNode.test.tsx` (14), `ParallelNode.test.tsx` (2), `graphExtensions.families.test.ts` (6) → **8/8 files, 65/65 passed, 0 skipped**.
- **Full pipelines suite (`src/features/pipelines/`):** **245/245 files, 2,777 passed, 1 pre-existing expected fail, 0 failed, 0 skipped**.
- **Fixtures:** Synthetic YAML fixtures constructed in-memory via `graphExtensionTestUtils.tsx` and `parallelTestUtils.tsx` (no database fixtures).

### Static Gates (`apps/elitea-web`, Node 24 / npm 11)
- `npm run typecheck`: **PASS** (0 errors).
- `npm run lint -- --deny-warnings`: **PASS** (0 warnings, 0 errors across 6,497 files).
- `npm run theme-gate`: **PASS** (all 9 checks + brand-pack contract tests).
- `node scripts/check-budgets.mjs`: **PASS** (7,276 files within §3.5 budgets).
- `node scripts/check-testid-namespace.mjs`: **PASS** (2,798 test IDs in one namespace).
- `node scripts/check-layer-cycle.mjs`: **PASS** (5,821 modules, 0 errors, 0 warnings).
- `node scripts/check-dead-code.mjs`: **PASS** (0 issues).
- `node scripts/i18n-backfill.mjs --check`: **PASS** (0 new keys added; `en.json` untouched).
- Raw MUI control scan (`TextField`/`Button`/`Accordion`/`<option`/`native: true`) across `graph-extensions/*` and `parallel/*`: **0 matches**.
- Secret scan on `git diff`: **clean**.
- `npm audit`: offline in sandbox (`ENOTFOUND registry.npmjs.org`); **0 dependencies added or modified** (`package.json` and `package-lock.json` untouched).

---

## 4. Performance, Durability, Resilience & Security

- **Performance:** Purely presentational control swap; no new network calls, queries, effects, or allocations on hot paths. `ExtensionFields.tsx:L29-L36` and `StateChannelSelect.tsx:L30-L34` build bounded option arrays over existing in-memory node/state lists (capped at 64 broadcasts/outputs/groups/operations and 16 parallel branches by existing UI button bounds).
- **Durability:** N/A — purely client-side editor form controls; authored YAML persistence and backend checkpointing are unchanged.
- **Resilience:** Bounded list additions (64 items for Map/Aggregate/SplitOut rows, 16 branches for Parallel) and invalid/unknown authored YAML values remain visibly preserved without silent coercion (`ExtensionFields.tsx:L32`, `StateChannelSelect.tsx:L32`; proven by `ExtensionFields.test.tsx`, `MapNode.test.tsx`, `ParallelSettings.test.tsx`). Disabled playback state disables every input, combobox (`aria-disabled="true"`), checkbox, and button (`ParallelSettings.test.tsx`, `MapNode.test.tsx`, `SplitOutNode.test.tsx`, `AggregateNode.test.tsx`).
- **Security (`rules/security.md` category status):**
  - Trust boundaries & identity: N/A (no auth/identity/header handling).
  - Authorization: N/A (no routes/RPCs).
  - Input, parsing & amplification: Existing YAML parser/admission validators unchanged; all select values remain plain React text children (no `dangerouslySetInnerHTML`, no `style` or event attributes).
  - Injection & construction / XSS: Safe React text rendering only; FU-286 eliminates raw i18n key interpolation on empty choice (`ExtensionFields.tsx:L24-L27`, proven by `ExtensionFields.test.tsx` and `MapSettings.policy.test.tsx`).
  - Egress & SSRF: N/A (no outbound requests or external URLs).
  - Secrets: No secrets in code, tests, or docs; diff secret scan clean.
  - Supply chain: 0 dependencies added or changed; `npm audit` offline in sandbox.

---

## 5. Recovery Guarantees (§2)

| Component × Phase | Guarantee | Enforcing code & proof |
| --- | --- | --- |
| Web/browser × authoring (non-execution) | **N/A** (unchanged) | Editor form controls write to the same `usePipelineYamlStore` / `patchGraphExtensionNode` pipeline without altering execution or checkpoint recovery (`ParallelSettings.test.tsx`, `MapNode.test.tsx`, `SplitOutNode.test.tsx`, `AggregateNode.test.tsx`). |

---

## 6. Real-Browser Evidence & Follow-Ups

- **Real-browser Chrome proof:** **Deferred** to Wave U1 / PR B integration per `handoffs/point5-wave1-20261008/fu283/WAVE-U1-PLAN.md` §3.5 (pure-Web static/unit-verified Wave U1 track; zero Rust/Docker/stack spin-ups).
- **Follow-ups:** None blocking. `RunStateDialog.tsx` (`EL-3734`) is scheduled post-5b-1 PR B (`FU-351`).
