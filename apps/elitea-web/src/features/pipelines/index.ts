/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20
 * exported symbols, enforced by `scripts/check-budgets.mjs`).
 *
 * This slice's first landing (sub-unit A2b: fstring-autocomplete +
 * yaml-editor — two small, mostly-standalone widgets with no
 * `pipelines`-internal dependencies of their own). Deliberately using only
 * 8 of the 20 slots: several more `pipelines` sub-units land in this same
 * batch (the flow editor, the create/edit forms), all sharing this one
 * budget.
 *
 * `useFStringAutocomplete` (the lower-level state-machine hook) is exported
 * alongside `useFStringInputAutocomplete` (the DOM-wiring hook that
 * composes it) even though `A2a`/`A2i` (this domain's other sub-units, the
 * declared consumers of this pair) most likely only need the latter — kept
 * public because it is independently useful to any future input surface
 * that wants its own DOM wiring (e.g. a non-`<input>`/`<textarea>` editor)
 * without duplicating the open/query/keyboard-nav state machine. Both
 * remain available via ordinary intra-slice import either way (R-L3: intra-
 * slice imports are unrestricted), so nothing is blocked if this call turns
 * out wrong later.
 */
export { FStringAutocompletePopper } from './ui/FStringAutocompletePopper';
export type { FStringAutocompletePopperProps } from './ui/FStringAutocompletePopper';

export { useFStringAutocomplete } from './model/useFStringAutocomplete';
export { useFStringInputAutocomplete } from './model/useFStringInputAutocomplete';

export type { FStringAutocompleteOption, FStringAutocompleteState } from './lib/fStringAutocomplete';

export { YamlCodeEditor } from './ui/YamlCodeEditor';
export type { YamlCodeEditorProps } from './ui/YamlCodeEditor';

/**
 * Sub-unit A2l's ("PipelineEditor.jsx composition + chat-integration hooks
 * + pipeline slices") contribution — `PipelineEditor`/`useEditPipeline`/
 * `usePipelineCreation` per this batch's own explicit cross-domain
 * requirement ("Wave-2 unit C6 needs all three importable cross-feature —
 * their only real consumer today is `pages/NewChat/NewChat.jsx`").
 *
 * Only the component/hooks and `PipelineEditorProps`/`PipelineEditorHandle`
 * are spent here. `PipelineEditorDeps` (and its constituent slot-prop
 * types: `PipelineConfigurationPanels`, `PipelineCreateFormSlotProps`,
 * `PipelineLlmModelSelectorSlotProps`, `PipelineConfigurationPanelSlotProps`,
 * `PipelineEditorShellProps`) and `useEditPipeline`'s/
 * `usePipelineCreation`'s own params/result types stay off this curated
 * list, same call `features/agents/index.ts` already made for
 * `AgentEditor`'s identically-shaped `AgentEditorDeps`/
 * `AgentEditorShellProps`/`AgentConfigurationFormSlotProps` ("a caller
 * assembling the `deps` object types it structurally against
 * `AgentEditor`'s own prop type without needing a separate import") — every
 * one of them is a plain object literal a caller builds inline, checked
 * structurally against `PipelineEditorProps['deps']`/the hooks' own
 * parameter types with no explicit import required. `PipelineEditorHandle`
 * IS exported because it is used as an explicit type-parameter
 * (`useRef<PipelineEditorHandle>`/`createRef<PipelineEditorHandle>`), not a
 * structurally-checked object literal — `AgentEditor` has no ref/imperative
 * handle at all, so there is no equivalent precedent to follow either way.
 */
export { PipelineEditor } from './ui/PipelineEditor';
export type { PipelineEditorProps, PipelineEditorHandle } from './ui/PipelineEditor';

export { useEditPipeline } from './model/useEditPipeline';
export { usePipelineCreation } from './model/usePipelineCreation';

/**
 * `ConfigurationTab` — the composition root tying `GeneralFormPanel` +
 * `EditorPanel` + `ChatPanel` together for the standalone pipeline-editing
 * page (`pages/pipelines/EditPipeline.tsx`, Wave-2 unit A2m). Landed by a
 * later A2 sub-unit than A2m itself (see `ConfigurationTab.tsx`'s own doc
 * comment) — exported here so that page can reach it at all: `pages/`
 * may only import a slice through its `index.ts` (`no-deep-slice-import`,
 * `.dependency-cruiser.cjs`), and `EditPipeline.tsx` previously had no
 * legal way to import this component even though it already existed on
 * disk. `ConfigurationTabProps` is exported alongside it (not left as a
 * structural-only type, unlike `PipelineEditorDeps`) because
 * `EditPipeline.tsx` needs to build several of its slot/prop values in
 * dedicated `pages/pipelines/lib/` helper files, where an explicit,
 * named prop type is clearer than deriving one via `ComponentProps<...>`
 * at every call site.
 */
export { ConfigurationTab } from './ui/ConfigurationTab';
export type { ConfigurationTabProps } from './ui/ConfigurationTab';

/**
 * The two halves of pipeline-graph PERSISTENCE, spent from this budget for
 * #135 — where a graph edit was accepted with a 200 and lost on reload.
 *
 * `usePipelineVersionSync` is the READ side (parse a loaded version's
 * `instructions` YAML + saved `pipeline_settings` geometry into the two
 * zustand stores the flow editor renders from). It already existed for
 * `ui/PipelineEditor.tsx`'s chat-embedded composition; the STANDALONE editor
 * page (`pages/pipelines/EditPipeline.tsx`) never had a legal way to call it,
 * so its canvas always started from an empty document no matter what was
 * stored.
 *
 * `usePipelineGraphDraft` is the WRITE side — see its own doc comment for the
 * `no-deep-slice-import` fence that made the live stores unreachable from
 * `pages/pipelines`, which is precisely why every save shipped an empty graph.
 */
export { usePipelineVersionSync } from './ui/usePipelineEditorLifecycle';
export { usePipelineGraphDraft } from './model/usePipelineGraphDraft';
export type { PipelineGraphDraft } from './model/usePipelineGraphDraft';

/**
 * The DISCARD side of the same #135 persistence pair: without it,
 * `EditPipeline`'s Cancel→Discard reverted only the RHF fields while
 * `usePipelineGraphDraft` kept reading the edited stores, so a later Save
 * persisted the "discarded" canvas edits. See its own doc comment.
 */
export { resetPipelineDraft } from './model/resetPipelineDraft';

/**
 * The save VETO, for the one caller that has to DISABLE a control on it:
 * `pages/pipelines`' version bar, whose "Save As Version" button was gated
 * only on `!isReadOnly` and so stored precisely the graph the Save veto had
 * refused.
 *
 * The other half of the veto — the page's own save path, where
 * react-hook-form's `handleSubmit` deletes every `root.*` error before
 * deciding whether to submit (7.83, `index.esm.mjs:3002`), leaving that path
 * with no admission check at all — rides on `usePipelineGraphDraft`'s reader
 * instead of a second export here: it needs the judgement at CLICK time, on
 * exactly the `yamlCode` it is about to store, and that reader already reads
 * exactly that string. See `PipelineGraphDraft.admission`. §3.3 keeps this
 * slice's public API at 20 symbols, which is the other reason there is one
 * export here and not two.
 */
export { useLivePipelineGraphAdmission } from './lib/livePipelineGraphAdmission';

/*
 * `PIPELINE_STARTER_TEMPLATE` is deliberately NOT here. It used to live in
 * `./lib/`, and publishing it would have taken this curated API to 21 symbols
 * — one over the §3.5 budget of 20. It has no dependency on this slice, and
 * two layers outside it now need it: `pages/pipelines/CreatePipeline.tsx` (the
 * create flow a person actually takes) and `./lib/usePipelineEditorCreate.ts`
 * (the chat surface's). So it sits in `shared/lib/pipelineStarterTemplate.ts`,
 * which both may import directly — no barrel entry, no budget spent, and no
 * `no-deep-slice-import` breach for the page.
 */
