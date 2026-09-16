import type { Participant } from '@/entities/participant';
import type { VersionSummary } from '@/entities/version';
import { selectDefaultVersion } from '@/entities/version';
import { isPublicProject } from '@/entities/project';
import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';

import type { AgentEditorParticipantDetails } from './AgentEditorPanel.types';

/**
 * Pure derived-state helpers for `AgentEditorPanel.tsx`, split out to keep
 * that component's own cyclomatic complexity under the §3.5 budget (≤12) —
 * same rationale as `UserInput.tsx`'s `../lib/normalizeUserInputProps.ts`.
 */

export function isPipelineParticipant(activeParticipant: Participant | undefined): boolean {
  return activeParticipant?.entitySettings?.agentType === 'pipeline';
}

export function isDetailsLoading(
  participantDetails: AgentEditorParticipantDetails | undefined,
  activeParticipant: Participant | undefined,
): boolean {
  return !participantDetails?.name || participantDetails.id !== activeParticipant?.entityMeta?.id;
}

/**
 * Replaces the baseline's inline `selectedVersion` `useMemo` (find-by-id
 * else `LATEST_VERSION_NAME` else first-item else `{}`) with `entities/
 * version`'s `selectDefaultVersion` — per this unit's own task brief.
 * **Disclosed narrowing**: `selectDefaultVersion` has no "else first item"
 * fallback the baseline's own chain had; when neither the selected id nor
 * the `'base'` version exists (only possible with an unusual/empty
 * versions list), this now resolves to `undefined` rather than silently
 * picking `versions[0]`. An intentional, disclosed behaviour narrowing per
 * the task brief's explicit instruction to reuse `selectDefaultVersion`
 * rather than re-derive the old chain, not an accidental regression.
 */
export function resolveSelectedVersion(
  versions: readonly VersionSummary[] | undefined,
  selectedVersionId: string | undefined,
): VersionSummary | undefined {
  return selectDefaultVersion(versions ?? [], selectedVersionId);
}

export function isPublicParticipant(activeParticipant: Participant | undefined, publicProjectId: string): boolean {
  const projectId = activeParticipant?.entityMeta?.projectId;
  return projectId !== undefined && isPublicProject(projectId, publicProjectId);
}

/**
 * `getConfig()` returns a `{status: 'ok', config} | {status: 'error', ...}`
 * result, not the raw config object — matches `features/agents/lib/
 * basename.ts`'s `getAgentsBasename()`'s own established pattern for
 * reading `shared/config` from a `features/`-layer file (same safe empty-
 * string fallback while config has not resolved yet).
 */
export function resolvePublicProjectId(): string {
  const result = getConfig();
  return result.status === 'ok' ? result.config.vite_public_project_id : '';
}

/**
 * **Disclosed, conservative gap** (same pattern already established twice
 * for the identical situation — `features/toolkits/ui/ToolkitEditor.tsx`
 * and `widgets/sidebar/lib/projectOptions.ts`): `usePublicProjectAccessCheck`
 * has no port anywhere in this worktree. The baseline's `canEdit` was
 * `(!isPublic && hasEditPermission) || (isPublic && hasPublicProjectAccess)`
 * — reduced here to `!isPublic && hasEditPermission` (never MORE permissive
 * than the baseline: a public-project participant is always locked, same
 * as `ToolkitEditor.tsx`'s own `disabled={isPublic}` reduction).
 */
export function canEditParticipant(isPublic: boolean, hasEditPermission: boolean): boolean {
  return !isPublic && hasEditPermission;
}

export function isSelectedVersionPublished(selectedVersion: VersionSummary | undefined): boolean {
  return selectedVersion?.status === 'published';
}

export function isEditSettingsDisabled(isPublic: boolean, versionPublished: boolean): boolean {
  return versionPublished && !isPublic;
}

export function settingsTooltipTitle(isPipeline: boolean, versionPublished: boolean, canEdit: boolean): string {
  if (isPipeline) {
    return canEdit
      ? t('chatInput.agentEditorPanel.pipelineSettings', 'Pipeline settings')
      : t('chatInput.agentEditorPanel.viewPipelineSettings', 'View pipeline settings');
  }
  if (versionPublished) {
    return t('chatInput.agentEditorPanel.publishedNotEditable', 'Published versions are not editable');
  }
  return canEdit
    ? t('chatInput.agentEditorPanel.agentSettings', 'Agent Settings')
    : t('chatInput.agentEditorPanel.viewAgentSettings', 'View agent Settings');
}

export function switchEntityTooltip(isPipeline: boolean): string {
  return isPipeline
    ? t('chatInput.agentEditorPanel.switchPipeline', 'Switch Pipeline')
    : t('chatInput.agentEditorPanel.switchAgent', 'Switch Agent');
}

/** A14 (ELITEA-0386): whether the "Edit LLM settings" button renders — split out to keep `AgentEditorPanel`'s own cyclomatic complexity under the §3.5 budget (12). */
export function showEditLlmSettingsButton(onEditLlmSettings: (() => void) | undefined, canEdit: boolean): boolean {
  return !!onEditLlmSettings && canEdit;
}
