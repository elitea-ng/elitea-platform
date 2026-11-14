/**
 * Pure derivations extracted from `pages/NewChat/PipelineEditor.jsx`'s
 * top-level component body (baseline lines 48-51, 152-154, 211-213,
 * 454-457) so `ui/PipelineEditor.tsx` can stay under the §3.5 per-function
 * complexity budget and so this logic is directly unit-testable without
 * mounting the whole editor. Mirrors `features/agents/lib/
 * agentEditorViewState.ts`'s own split for `AgentEditor.jsx` — same
 * precedent, adapted for the pipeline shape's extra `meta.id` fallback.
 */

import { canEditApplication } from '@/shared/lib/applicationEditAccess';

export const PUBLIC_PROJECT_ID = 'public';

export interface PipelineEditorPipelineLike {
  readonly id?: string | number;
  readonly entity_meta?: { readonly id?: string | number; readonly project_id?: string | number };
  readonly entity_settings?: { readonly version_id?: string | number };
  readonly meta?: { readonly id?: string | number; readonly name?: string };
  readonly name?: string;
}

/**
 * `getPipelineId` — `PipelineEditor.jsx:48-51`. Pipeline participant shapes
 * have THREE possible id locations (agents only have two —
 * `entity_meta.id`/`id`): `entity_meta.id` (the normal chat-participant
 * shape), `id` (a bare application row), or `meta.id` (a legacy/alternate
 * shape the baseline's own comment calls out).
 */
export function getPipelineId(pipeline: PipelineEditorPipelineLike | null | undefined): string | number | undefined {
  return pipeline?.entity_meta?.id ?? pipeline?.id ?? pipeline?.meta?.id;
}

/** `pipeline?.entity_meta?.project_id === PUBLIC_PROJECT_ID` — `PipelineEditor.jsx:152`. */
export function isPublicPipeline(pipeline: PipelineEditorPipelineLike | null | undefined): boolean {
  return pipeline?.entity_meta?.project_id === PUBLIC_PROJECT_ID;
}

/** `!isPublic && hasEditPermission` — `PipelineEditor.jsx:153`. */
export function canEditPipeline(isPublic: boolean, hasEditPermission: boolean): boolean {
  return canEditApplication(isPublic, hasEditPermission);
}

/** The baseline's `ViewMode.Owner`/`ViewMode.Public` string literals (`common/constants.js`) — `PipelineEditor.jsx:154`. */
export function pipelineViewMode(canEditIt: boolean): 'Owner' | 'Public' {
  return canEditIt ? 'Owner' : 'Public';
}

/** `pipeline?.meta?.name || pipeline?.name || fallback` — `PipelineEditor.jsx:287,316,456`. */
export function pipelineDisplayName(pipeline: PipelineEditorPipelineLike | null | undefined, fallback = ''): string {
  return pipeline?.meta?.name || pipeline?.name || fallback;
}
