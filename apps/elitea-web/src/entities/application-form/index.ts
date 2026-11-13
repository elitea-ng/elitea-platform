/**
 * `entities/application-form` — the Wave-2 promotion pass's new home for
 * the "create/validate/save an Application entity" scaffolding that used
 * to live sideways-coupled between the baseline's agent and pipeline pages
 * (`apps/elitea-ui/src/pages/Applications/Components/Applications/*`).
 *
 * **Why a separate slice from `entities/application`, not an extension of
 * it:** `entities/application/index.ts` is ALREADY at the §3.5 budget cap —
 * exactly 20 named exports (8 types + 5 selectors + 7 normalisers), zero
 * headroom. Adding this promotion's ~20 new exports there would blow the
 * budget outright, and depcruise's `no-deep-slice-import-cross-slice` rule
 * only recognises `src/entities/<slice>/index.ts` (one path segment) as a
 * valid cross-slice entry point — a nested `entities/application/
 * application-form/index.ts` would NOT count as a public API for any other
 * slice, so nesting inside the existing slice was not an option either. A
 * new sibling slice is the only depcruise-legal way to expose a second,
 * budget-independent public surface, and the promotion brief's own
 * language ("its own dedicated index.ts") anticipated exactly this.
 *
 * `entities/application-form` may NOT import `entities/application` (or
 * vice versa) — `no-sideways-entities` forbids it. Where both slices need
 * the same shape (e.g. `LATEST_VERSION_NAME`), it is deliberately
 * duplicated locally, same convention `entities/application/model/types.ts`
 * already documents for its own duplication against `entities/pipeline`.
 *
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20 —
 * currently exactly 20, see the promotion report for what was deliberately
 * left off this list, e.g. `buildApplicationValidationKey`/
 * `parseApplicationValidationMessage`, which remain available via
 * intra-slice import and can be promoted to this list once a real
 * toolkit-validation endpoint exists for them to serve).
 */
export { applicationCreationSchema, applicationEditSchema } from './model/validation';
export type { ApplicationCreationInput } from './model/validation';

export { useCreateApplicationInitialValues } from './model/initialValues';
export type { ApplicationDraft, ApplicationVersionDraft } from './model/initialValues';

export { useCreateApplicationDraft, useSaveApplicationVersion } from './model/mutations';
export type { ApplicationDraftInput } from './model/mutations';

export { isAttachmentsEnabled, applyMcpToolStatus } from './model/toolStatus';

export { subApplicationTools } from './model/validationStatus';

export { CreateApplicationTabBar } from './ui/CreateApplicationTabBar';
export type { CreateApplicationTabBarProps } from './ui/CreateApplicationTabBar';

export { ApplicationSaveButton } from './ui/ApplicationSaveButton';

export { ApplicationValidator } from './ui/ApplicationValidator';
export type { ApplicationValidatorProps, UseValidateVersion } from './ui/ApplicationValidator';

export { ApplicationConfigurationLayout } from './ui/ApplicationConfigurationLayout';
export type { ApplicationConfigurationLayoutProps } from './ui/ApplicationConfigurationLayout';
