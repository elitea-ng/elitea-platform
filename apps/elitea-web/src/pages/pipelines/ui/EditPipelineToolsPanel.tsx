import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { AgentToolsPanel } from '@/features/agents';
import { getGetApplicationQueryKey } from '@/shared/api/generated/applications/applications';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import { ViewMode } from '@/shared/lib/enums';

import type { EditPipelineVersionFieldsState } from '../lib/useEditPipelineVersionFields';

/**
 * The pipeline editor's half of the TOOLS/MODULES panel — the pipelines
 * mirror of `pages/agents/ui/EditApplicationToolsPanel.tsx`, and a separate
 * file for the same reason: `EditPipeline.tsx` is at both its §3.5 400-line
 * and complexity-12 budgets.
 *
 * Everything intra-slice (the accordion, the attach menu, the per-tool cards,
 * the per-row disassociate hook) is composed inside `features/agents`'
 * `AgentToolsPanel`. What is left here is the part that genuinely IS page
 * state:
 *  - the entity ids and the ACTIVE version's id/status/meta, which this page
 *    resolves (it may be showing an explicitly-requested version);
 *  - `internal_tools`, which unlike attached toolkits is ordinary form state
 *    saved through the version PUT's `meta` blob;
 *  - the refetch on attach, because `ToolMenu` invalidates its own
 *    `getApplication` cache entry and this page reads the SAME query through
 *    `useEditPipelineData`.
 *
 * `isPipeline` is the one prop the agents twin does not pass, and it is not
 * cosmetic: `ApplicationTools` narrows the MODULES grid to `attachments`
 * alone for a pipeline (baseline `ApplicationTools.jsx:91-94`). The other
 * internal tools are agent-executor features; offering their switches on a
 * pipeline would store `meta.internal_tools` entries the pipeline runtime
 * never reads.
 */
export interface EditPipelineToolsPanelProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly activeVersion: ApplicationVersionDetail | undefined;
  readonly versionFields: EditPipelineVersionFieldsState;
  readonly isDirty: boolean;
  readonly isReadOnly: boolean;
}

export function EditPipelineToolsPanel({
  projectId,
  applicationId,
  activeVersion,
  versionFields,
  isDirty,
  isReadOnly,
}: EditPipelineToolsPanelProps): ReactNode {
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
      ...(typeof attachmentToolkitId === 'number' || typeof attachmentToolkitId === 'string' ? { attachmentToolkitId } : {}),
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
      isPipeline
      viewMode={isReadOnly ? ViewMode.Public : ViewMode.Owner}
    />
  );
}
