import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { AgentToolsPanel } from '@/features/agents';
import { getGetApplicationQueryKey } from '@/shared/api/generated/applications/applications';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import { ViewMode } from '@/shared/lib/enums';

import type { EditApplicationVersionFieldsState } from '../lib/useEditApplicationVersionFields';

/**
 * The agent edit page's half of the Tools panel wiring (#307's last piece).
 * A separate file for the same reason `./EditApplicationActions.tsx` and
 * `../lib/useEditApplicationVersionFields.ts` are: `EditApplication.tsx` is
 * at both its §3.5 400-line and complexity-12 budgets, so anything with its
 * own branching lands beside it rather than in it.
 *
 * Everything intra-slice — the accordion, the attach menu, the per-tool
 * cards, the per-row disassociate hook — is composed inside
 * `features/agents`' `AgentToolsPanel` (one public-API slot instead of
 * four; see that file and the barrel's own note). What is left here is the
 * part that genuinely IS page state:
 *  - the entity ids and the ACTIVE version's id/status/meta, which this
 *    page resolves (it may be showing an explicitly-requested version, not
 *    the default one);
 *  - `internal_tools`, which unlike attached toolkits is ordinary form
 *    state saved through the version PUT's `meta` blob — routed into
 *    `useEditApplicationVersionFields` so Save actually sends it and the
 *    unsaved-changes nav blocker can see it;
 *  - the refetch on attach: `ToolMenu` invalidates its own
 *    `getApplication` cache entry, but this page reads the SAME query
 *    through `useEditApplicationData`, so invalidating it here is what makes
 *    a newly attached tool appear without a reload.
 *  - `viewMode`, which the public/read-only check upstream already resolved.
 */
export interface EditApplicationToolsPanelProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly activeVersion: ApplicationVersionDetail | undefined;
  readonly versionFields: EditApplicationVersionFieldsState;
  readonly isDirty: boolean;
  readonly isReadOnly: boolean;
}

export function EditApplicationToolsPanel({
  projectId,
  applicationId,
  activeVersion,
  versionFields,
  isDirty,
  isReadOnly,
}: EditApplicationToolsPanelProps): ReactNode {
  const queryClient = useQueryClient();

  const onToolsChanged = useCallback(() => {
    if (projectId === undefined || applicationId === undefined) return;
    void queryClient.invalidateQueries({ queryKey: getGetApplicationQueryKey(projectId, applicationId) });
  }, [queryClient, projectId, applicationId]);

  const onInternalToolsChange = useCallback(
    (next: readonly string[]) => {
      versionFields.applyFieldChange('version_details.meta.internal_tools', next);
    },
    [versionFields],
  );

  const meta: Record<string, unknown> = activeVersion?.meta ?? {};
  const attachmentToolkitId = meta['attachment_toolkit_id'];

  const entity = useMemo(
    () => ({
      applicationId,
      versionId: activeVersion === undefined ? undefined : Number(activeVersion.id),
      versionStatus: activeVersion?.status,
      ...(typeof attachmentToolkitId === 'number' || typeof attachmentToolkitId === 'string'
        ? { attachmentToolkitId }
        : {}),
    }),
    [applicationId, activeVersion, attachmentToolkitId],
  );

  return (
    <AgentToolsPanel
      entity={entity}
      versionTools={activeVersion?.tools}
      dirty={isDirty}
      internalTools={{ value: versionFields.fields.internalTools, onChange: onInternalToolsChange }}
      onToolsChanged={onToolsChanged}
      readOnly={isReadOnly}
      viewMode={isReadOnly ? ViewMode.Public : ViewMode.Owner}
    />
  );
}
