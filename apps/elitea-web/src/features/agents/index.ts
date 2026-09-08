/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 * This budget is shared across every A1x sub-unit that owns files under
 * `features/agents` (some landed concurrently in this same worktree, some
 * not yet) — NOT a per-sub-unit allowance. This is sub-unit A1c's
 * ("Agent-details configuration form") contribution: only the two symbols
 * that genuinely need a cross-slice/page-level entry point.
 *
 * The other 12 components A1c ported (`ApplicationTools`/
 * `ApplicationAdvanceSettings`/`ApplicationEditorNotes`/
 * `ApplicationVariables`/`InstructionsInput`/
 * `InstructionsSlashSuggestionList`/`WelcomeMessageInput`/`ModalMessage`/
 * `StyledShowContextModal`/`AgentInternalToolSwitch`/`AgentMetaSwitch`/
 * `AttachmentSwitch`) are consumed INTRA-slice only today — by
 * `CreateAgentForm` itself, and by whichever sibling A1 sub-unit builds the
 * `PipelineConfigurationForm`-baseline-equivalent hub that plugs these same
 * panels into `entities/application-form`'s `ApplicationConfigurationLayout`
 * slots. Per R-L3, a sibling file inside this SAME `features/agents/` slice
 * may import them directly (`../ui/ApplicationTools`, etc) without going
 * through this barrel — only `pages/`/`widgets/`/a genuinely different
 * slice needs the entry point spent here. Promote any of the 12 to this
 * list the moment a real cross-slice/page consumer needs one directly.
 *
 * `VersionReplacementModal` is exported per the batch brief's explicit
 * "MUST EXPORT VIA PUBLIC API" list (a confirmed future cross-feature
 * consumer target for Wave-2 unit C6). `CreateAgentForm` is exported
 * because it is this domain's top-level create/edit form assembly — the
 * baseline's own `CreateApplication.jsx`/`AgentEditor.jsx` page-level
 * callers render it directly, a relationship this app's own page/widget
 * layer will need the same entry point for.
 */
export { CreateAgentForm } from './ui/CreateAgentForm';

/**
 * The "Edit with AI" trigger for an agent's Instructions field. Exported as
 * ONE symbol (the button; its modal, gate hook and diff renderer stay
 * intra-slice) because the button gates itself — a page mounts it
 * unconditionally and it renders nothing where the backend cannot serve it.
 * See its own doc comment for the three gate conditions.
 */
export { EditInstructionsWithAiButton } from './ui/ai-edit/EditInstructionsWithAiButton';

export { VersionReplacementModal } from './ui/VersionReplacementModal';

/**
 * `pages/agents-hub`'s `AgentModal` shows the agent instructions in this
 * dialog. Baseline: `AgentModal.jsx:263-269`. A page must enter the slice
 * through this file (`.dependency-cruiser.cjs` `no-deep-slice-import`).
 *
 * `CreateAgentFormProps` gave up the slot. The §3.5 budget is 20 symbols,
 * and the list was full. No file outside this slice imported that type; one
 * doc comment in `pages/agents/CreateApplication.tsx` names it, and a doc
 * comment is not an import. Restore it here if a real consumer appears, and
 * retire another symbol for it.
 */
export { StyledShowContextModal } from './ui/StyledShowContextModal';

/**
 * #134 — `AgentVersionControls` (the agent editor's version dropdown +
 * "Save As Version" pair) is the composition root `pages/agents/
 * EditApplication.tsx` needs; without it both halves were already-ported
 * dead code (the selector reachable only from a TOOL card, the button from
 * nowhere at all). One symbol, not two — `AgentPipelineVersionSelector` and
 * `SaveNewVersionButton` stay intra-slice and are composed behind it.
 *
 * The slot it occupies came from dropping `VersionReplacementModalProps`
 * (the TYPE, not the component — the batch brief's "MUST EXPORT VIA PUBLIC
 * API" list names `VersionReplacementModal` itself, which is untouched
 * above). Verified before removal: a whole-tree grep for
 * `VersionReplacementModalProps` across `src/` and `e2e/` finds only its own
 * declaration and this line — zero consumers, in production or in tests. A
 * future cross-slice caller can type its props structurally off the
 * component, exactly as this barrel's `AgentEditor` doc comment already
 * argues for `AgentEditorDeps`.
 */
export { AgentVersionControls } from './ui/AgentVersionControls';

/**
 * #307 — the agent editor's own delete and export affordances. Both were
 * fully ported, fully tested and imported by NOTHING: the issue lists them
 * among the "correctly-wired components with no mount point", and the page
 * that is supposed to host them (`pages/agents/EditApplication.tsx`) could
 * not reach them from outside this slice. The three slots they and their
 * host needed came from repacking the four validation hooks into
 * `applicationValidationHooks` (see that file) — no export was dropped.
 */
export { DeleteApplicationButton } from './ui/DeleteApplicationButton';
export { ExportApplicationButton } from './ui/ExportApplicationButton';

/**
 * #307 — the agent editor's Tools panel, and the LAST free slot on this
 * curated API (20/20). One symbol, deliberately: a page-level composition
 * would have had to import FOUR (`ApplicationTools`, `ToolCard`,
 * `useDisassociateToolkit`, and the `AgentToolAssociation` type its
 * `renderToolCard`/hook contracts are written in) against a budget with one
 * slot free. `AgentToolsPanel` composes all four INSIDE the slice — the
 * same "compose behind one export" answer `AgentVersionControls` already
 * gave for the version dropdown + Save-As-Version pair — and takes only
 * page-owned values across the boundary (ids, the server's raw
 * `version_details.tools[]`, the read-only flag, a refetch callback), none
 * of which needs a type import.
 */
export { AgentToolsPanel } from './ui/AgentToolsPanel';

/**
 * #345 — the agent editor's tag control. It was written, tested through
 * `ApplicationEditForm`, and mountable by nobody: an unexported local of a
 * file no page renders, against a backend that had no `tags` field on
 * `VersionWriteRequest` at all. Both halves are closed now — the wire
 * contract carries the field and `UpdateVersion` writes the association
 * rows — so the page mounts this through `CreateAgentForm`'s `tagsSlot`.
 *
 * The slot came from repacking the five write hooks into
 * `applicationWriteHooks` (see that file); it freed four, and three are
 * deliberately left free.
 */
export { AgentTagEditor } from './ui/AgentTagEditor';

/** Explicit UI for the application-version `mcp` tag, shared by agent and pipeline edit pages. */
export { ApplicationMcpAccessToggle } from './ui/ApplicationMcpAccessToggle';

/**
 * The "Information" accordion — entity id, version id, the pipeline trigger
 * rows (type/schedule/timezone/last run/webhook type), "Forked from", and the
 * "Show" link onto the stored pipeline document.
 *
 * Another component that was written, unit-tested against a mocked
 * `pipeline_trigger` endpoint, and mounted by NOBODY — the same class of
 * defect `AgentTagEditor` above records, and the reason the pipeline editor
 * had no way to show a person the ids they need to wire an external caller.
 * The pipeline configuration form (`pages/pipelines/ui/
 * EditPipelineConfigurationPanel.tsx`) is its first real call site.
 *
 * Exported as the component alone: every prop is a primitive the caller
 * already holds, so a caller needs no type import. That keeps this curated
 * API at 19 of its 20 §3.3 slots.
 *
 * `ApplicationEditorNotes` — its sibling in the same unmounted set — is
 * deliberately NOT exported with it. `version_details.notes` has no column on
 * `application_versions`, no property on `VersionWriteRequest`, and no branch
 * in `UpdateVersion`: mounting it would give a person a text box whose
 * content the very next save discards.
 */
export { ApplicationInformation } from './ui/ApplicationInformation';

/**
 * Sub-unit A1a's ("Application data layer + version-lifecycle hooks")
 * contribution — the application/version data layer + tool change-diffing +
 * validation + chat-version-switch hooks (see each file's own doc comment
 * for the baseline file it ports and any disclosed backend gap/redesign).
 * `useDeleteVersion` is exported per the Wave-2 batch brief's explicit
 * requirement ("a known future consumer of not-yet-built
 * entities/version-adjacent UI — a version-delete dialog"). #345 repacked
 * all five write hooks into `applicationWriteHooks` — see that file for what
 * the four freed slots bought and for the grep that proved the repack has
 * one call site, not five. `getChangedTools`,
 * `useAutoSwitchApplicationChatVersion`, `useApplicationsStore`,
 * `entities/application-form`'s own `useCreateApplicationDraft`/
 * `useSaveApplicationVersion`, and every hook's plain input/result types
 * stay intra-slice-only (sibling `A1*` sub-units under this SAME
 * `features/agents/` slice may still import them directly — R-L3 only
 * restricts crossing INTO a different slice).
 */
export { applicationWriteHooks } from './model/applicationWriteHooks';
export { applicationValidationHooks } from './model/applicationValidationHooks';
export { useApplicationChatSwitchVersion } from './model/useApplicationChatSwitchVersion';
export { useCreateConfiguration } from './model/useCreateConfiguration';

/**
 * Sub-unit A1d's ("Generate-agent-modal (AI-assisted creation) +
 * AgentEditor composition") contribution — `AgentEditor` per this batch's
 * own explicit cross-domain requirement ("Wave-2 unit C6 needs it
 * importable cross-feature"). Only the component and its top-level props
 * type are spent here; `AgentEditorDeps`/`AgentEditorShellProps`/
 * `AgentConfigurationFormSlotProps` (the injected-slot contracts — see
 * `ui/AgentEditor.tsx`'s own doc comment) and `GenerateAgentButton`
 * (consumed intra-slice by `AgentEditor` itself and by `CreateAgentForm`'s
 * `generateAgentButtonSlot`) stay off this curated list to leave headroom
 * for the other `A1*` sub-units sharing this same budget — a caller
 * assembling the `deps` object types it structurally against `AgentEditor`'s
 * own prop type without needing a separate import.
 */
export { AgentEditor } from './ui/AgentEditor';
export type { AgentEditorProps } from './ui/AgentEditor';

/**
 * Sub-unit A1b's ("Agent core hooks, helpers & API") contribution — only
 * `parseYamlToMermaid`, per this batch's own brief: "also needed by a
 * not-yet-built `entities/import-wizard` -- export it from index.ts as
 * forward-looking public surface." Everything else A1b ported
 * (`useApplicationChat`, `useDisassociateToolkit`, the four instructions-
 * mention hooks, `useRefetchAgentDetails`/`useSetRefetchDetails`,
 * `useSaveAgentToolVariables`, `useGetAgentCategoriesQuery`/
 * `useLazyGetAgentCategoriesQuery`, `useGenerateAgentDraftMutation`,
 * `validateAgentDraft`) is consumed intra-slice only today (by whichever
 * sibling A1 sub-unit builds the chat-tab/tool-card UI that mounts them) —
 * left off this curated list to leave headroom under the shared ≤20 budget
 * for the other A1 sub-units still landing. Promote any of them here the
 * moment a real `pages/`/`widgets/`/different-slice consumer needs one
 * directly (R-L3: sibling files inside this SAME `features/agents/` slice
 * may already import them without going through this barrel).
 */
export { parseYamlToMermaid } from './lib/helpers/parseYamlToMermaid.helpers';

/**
 * Unit C6 additions: `useAgentCreation`/`useEditAgent`/`useAgentEditorUrlSync`
 * — all fully ported, intra-slice-only today, needed cross-feature by C6's
 * `deps`-composition root. Bundled into one `agentEditorHooks` object export
 * (the established convention, matching `chatInputCompositionHooks`/`voiceHooks`
 * precedent) to consume exactly the 1 remaining free slot (19/20 → 20/20).
 */
export { agentEditorHooks } from './model/agentEditorHooks';

/**
 * Unit C1 addition (`processes/chat/model`): `useApplicationsStore`, per
 * this Wave-2 run's own cross-cluster guidance — `processes/chat/model/
 * useRefetchAgentVersionDetailsOnClose.ts` reuses this store's
 * `shouldRefetchDetails`/`setShouldRefetchDetails` directly rather than
 * re-deriving a chat-scoped duplicate (a duplicate store instance would be
 * silently broken: two independent zustand singletons never see each
 * other's writes). Purely additive — no existing export above changed;
 * still 20/20 against the shared ≤20 budget.
 */
export { useApplicationsStore } from './model/applicationsStore';
