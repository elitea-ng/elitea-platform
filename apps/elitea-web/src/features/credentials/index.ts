/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20
 * exported symbols, enforced by `scripts/check-budgets.mjs`).
 *
 * `CredentialWarning` (A7-api-model adversarial-review fix, consolidating
 * an A7-ui pass that landed moments earlier in the same file): the
 * credential-change-warning hook, modal, and banner
 * (`useCredentialWarningModal`, `CredentialWarningModal`,
 * `CredentialWarningBanner`) were fully built and tested but reachable only
 * as two separate flat exports (hook + modal), because a third flat export
 * for the banner would have pushed this barrel to 21/20
 * (`scripts/lib/budgets-core.mjs`'s `countExports` counts each named
 * specifier; see that file for the exact rule) — the prior version of this
 * comment recorded that as a dead end ("budget: 20/20, zero room left").
 * It isn't: the three are ONE cohesive unit in the baseline too (a single
 * `entities/credential-warning` slice — `hooks/`, `ui/`, `helpers/` all
 * under it), always consumed together (the hook's `showWarning` drives the
 * modal's `open`; the banner is the inline sibling for the "no matching
 * credential" case the same flow can hit). Exporting them as ONE grouped
 * object, `CredentialWarning: { useModal, Modal, Banner }`, costs exactly 1
 * export slot instead of 3 and gets the banner reachable too — the §3.5
 * "bundle related exports into an object instead of gaming the budget
 * checker" resolution, applied to the slice-barrel row instead of the
 * component-props/hook-deps rows it's usually invoked for. Their prop/param
 * TYPES (`CredentialWarningModalProps`, `CredentialWarningBannerProps`,
 * `UseCredentialWarningModalParams`, `UseCredentialWarningModalResult`,
 * `ToolDetailLike`) are deliberately NOT re-exported here for the same
 * budget reason — a caller passing an object literal to
 * `CredentialWarning.useModal(...)` or JSX props to
 * `CredentialWarning.Modal`/`.Banner` gets full structural type-checking
 * against the real signatures without needing to name the types.
 *
 * ROUTING NOTE for whoever wires this up (outside A7-api-model's file
 * scope — do not fold this into this cluster's own fix):
 * `features/toolkits` cannot import this slice directly
 * (`no-sideways-features`/R-L1 — sibling `features/*` never import each
 * other) and already proved that out by hand-porting an equivalent
 * `useCredentialWarning`/`CredentialWarningModal` pair locally
 * (`features/toolkits/model/useCredentialWarning.hooks.ts` +
 * `features/toolkits/ui/form/ToolkitForm/CredentialWarningModal.tsx`), live
 * and wired via `ToolkitsOperationButtons.tsx` for the standalone
 * `pages/toolkits/{Create,Edit}Toolkit.tsx` flows — `features/agents/ui/
 * ToolCard.types.ts` independently hit the same wall for the banner's
 * "credential not found" case (its own doc comment: "which this slice
 * cannot import"). The one toolkit-editing path NOT covered by the
 * toolkits-local port is the chat-mounted editor:
 * `processes/chat/ui/ChatWithEditors.tsx` renders `<ToolkitEditor deps={{
 * renderShell, createToolkit, saveToolkit }}>` with no `checkBeforeSave`
 * entry, so `ToolkitEditorParts.tsx`'s `onBeforeSave={deps.checkBeforeSave}`
 * is `undefined` there and the credential-change warning is silently
 * skipped end-to-end for that flow (matches `ToolkitEditor.tsx`'s own
 * documented no-op default for an omitted `checkBeforeSave`, but the
 * omission was never meant to be permanent — that file's own
 * `deps.checkBeforeSave` doc paragraph names this exact export as the
 * blocker). `processes/chat/ui/` sits ABOVE both `features/toolkits` and
 * `features/credentials` in `.dependency-cruiser.cjs`'s `LAYERS_ABOVE`
 * table (only `src/app/` is barred from importing `processes/`), so it is
 * the correct — and only currently legal — place to import
 * `CredentialWarning` from `@/features/credentials`, call
 * `CredentialWarning.useModal({...})` there, thread its `checkBeforeSave`
 * into that `deps` object, and render `<CredentialWarning.Modal
 * open={showWarning} ... />` alongside the other editor modals already in
 * that file.
 *
 * `CredentialsSelect` (exported below) has its own separate "zero live call
 * site" status for the same `no-sideways-features` reason (A7-ui cluster) —
 * see that file's own doc comment for the full routing writeup; nothing
 * about its barrel export needed to change here.
 *
 * `CredentialsActions`: adding `CredentialWarning` above bought back two
 * slots but spent three, so this barrel landed on 21/20 and
 * `node scripts/check-budgets.mjs` had been failing on the
 * `slice-public-api` row ever since — the gate is not part of the unit test
 * suite, so nothing else reported it. The pair that gives a slot back
 * without losing any reach is `CredentialsTabBar` + `CredentialsControls`,
 * for the same three reasons the warning trio was grouped. They are ONE
 * unit in the baseline: both port out of the single folder
 * `apps/elitea-ui/src/[fsd]/features/credentials/ui/credentials-tab-bar/`
 * (`CredentialsTabBar.jsx` and `CredentialsControls.jsx` — each file's own
 * doc comment names that path, and `pages/credentials/
 * useCredentialDeleteGuard.ts:21` names it again). They are always consumed
 * together: `pages/credentials/CredentialForm.tsx:40` is the only external
 * importer of either, takes both in one import statement, and renders them
 * as adjacent siblings inside one `<Box sx={actionsRowSx}>`
 * (`CredentialForm.tsx:152-168`) — the Save/Discard pair and the kebab menu
 * that sits beside it are the same actions row, not two features. And their
 * prop types stay unexported for the same reason the warning trio's do: JSX
 * props on `CredentialsActions.TabBar`/`.Controls` are structurally checked
 * against the real signatures without a caller ever naming
 * `CredentialsTabBarProps`/`CredentialsControlsProps`.
 *
 * The alternative — dropping `CredentialWarning` again — was rejected: it
 * is the export that makes a fully built and tested hook/modal/banner
 * reachable at all, and un-exporting it would put the slice straight back
 * into the dead-code state the paragraph at the top of this file was
 * written to end. Every other symbol here has at least one live external
 * importer, so grouping was the only move that did not cost reach.
 */
import { useCheckStoredConfigurationConnection, useTestConfigurationConnection } from './api/useConfigurations';
import { classifySchemaField, configurationSectionsOf, initialDataForSchema } from './lib/schemaField';
import { useCredentialWarningModal } from './model/useCredentialWarningModal';
import { CredentialsControls } from './ui/CredentialsControls';
import { CredentialsTabBar } from './ui/CredentialsTabBar';
import { CredentialWarningBanner } from './ui/CredentialWarningBanner';
import { CredentialWarningModal } from './ui/CredentialWarningModal';

export type {
  ConfigSchemaNode,
  ConfigSchemaSection,
  ConfigSchemaSubsection,
  ConfigurationTypeDescriptor,
} from './api/configurations';
export {
  useAvailableConfigurationsType,
  useConfigurationDetail,
  useConfigurationsList,
  useCreateConfiguration,
  useDeleteConfiguration,
  useUpdateConfiguration,
} from './api/useConfigurations';
export { extractInformationFromCredentialError } from './lib/credentialError';
export { generateCredentialTagList } from './lib/credentialTags';
export { normalizeCredentialPage } from './lib/normalizeCredential';
export { useCredentialValidation } from './model/useCredentialValidation';
export { CredentialsSelect } from './ui/CredentialsSelect';

/**
 * The schema-field classifier and its two companions, grouped for the same
 * budget reason as `CredentialWarning`/`CredentialsActions` below (this
 * barrel was at 20/20 and the picker field needed a third symbol).
 *
 * They are one unit: `classify` decides a property's widget kind,
 * `configurationSections` answers the follow-up question the new
 * `'configuration'` kind raises ("which sections does this reference draw
 * from?"), and `initialData` seeds a form with values that agree with
 * `classify`'s own decisions. A caller that has one always needs at least
 * one of the others.
 */
export const SchemaField = {
  classify: classifySchemaField,
  configurationSections: configurationSectionsOf,
  initialData: initialDataForSchema,
} as const;

/** See this file's own doc comment above for why these three are grouped into one export instead of three. */
export const CredentialWarning = {
  useModal: useCredentialWarningModal,
  Modal: CredentialWarningModal,
  Banner: CredentialWarningBanner,
} as const;

/**
 * The two connection checks, grouped for the same reason (and by the same
 * rule) as `CredentialWarning`/`CredentialsActions` above: this barrel was
 * already at 20/20, and `useCheckStoredConfigurationConnection` has no
 * meaning apart from `useTestConfigurationConnection` — they are the two
 * halves of ONE control. Which half runs is decided by whether the browser
 * currently holds the secret:
 *
 *   - `useUnsaved` posts the candidate payload, and is the only form that can
 *     work while the user is still typing a new secret;
 *   - `useStored` posts NOTHING and names the saved row instead, because a
 *     stored secret is sealed and never leaves the vault.
 *
 * Exporting them as one object costs 1 slot instead of 2 and keeps a caller
 * from reaching for the payload form on a saved row, which is the mistake
 * that motivated the stored route in the first place.
 */
export const CredentialConnectionChecks = {
  useUnsaved: useTestConfigurationConnection,
  useStored: useCheckStoredConfigurationConnection,
} as const;

/** See this file's own doc comment above for why these two are grouped into one export instead of two. */
export const CredentialsActions = {
  TabBar: CredentialsTabBar,
  Controls: CredentialsControls,
} as const;
