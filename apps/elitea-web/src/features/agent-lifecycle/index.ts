/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * The agent and pipeline LIFECYCLE: publish, unpublish, share, fork, import.
 * A slice of its own rather than more surface on `features/agents`, whose
 * barrel is already at its 20/20 budget, and because pipelines need the same
 * four affordances from a different page — a shared slice is what keeps the
 * two editors from growing two copies of the same menu.
 */
export { EntityLifecycleControls } from './ui/EntityLifecycleControls';
export { EntityLifecycleMenu } from './ui/EntityLifecycleMenu';
export { PublishVersionDialog } from './ui/PublishVersionDialog';
export { ForkEntityDialog } from './ui/ForkEntityDialog';
export type { ForkTargetProject } from './ui/ForkEntityDialog';
export { EntityImportButton } from './ui/EntityImportButton';
export { useEntityLifecycle, useEntityImport, lifecycleErrorMessage } from './model/useEntityLifecycle';
export type { EntityLifecycle } from './model/useEntityLifecycle';
export { buildEntityShareLink, getLifecycleBasename } from './lib/shareLink';
export type { LifecycleEntity } from './lib/shareLink';
export type { EntityExportDocument } from './api/lifecycleApi';
