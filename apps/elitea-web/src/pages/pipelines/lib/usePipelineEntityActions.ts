import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';
import { useNavigate } from '@tanstack/react-router';

import { getDeleteApplicationQueryOptions } from '@/shared/api/generated/applications/applications';
import { getConfig } from '@/shared/config';
import { exportMarkdown } from '@/shared/lib/download';
import { disarmUnsavedChangesNavBlocker } from '@/widgets/app-shell';

/**
 * Export and delete for one PIPELINE, for the editor's `⋮` menu.
 *
 * `pages/pipelines/ui/EditPipelineActions.tsx` used to state that "export and
 * delete have no pipeline-side mount point": the agents editor mounts
 * `features/agents`' `ExportApplicationButton`/`DeleteApplicationButton` as
 * toolbar buttons, and both components hard-code agent copy ("Delete agent",
 * "Export agent") — their own doc comments say the pipelines domain gets its
 * own copy. This is that copy, as a hook rather than two buttons, because the
 * reference puts both actions in the pipeline's menu (Share, Fork, Export,
 * Delete) and not on its toolbar.
 *
 * A pipeline IS an application row (`agent_type: 'pipeline'`), so both
 * actions use the same two application routes the agents editor uses:
 * `GET /elitea_core/export_import/prompt_lib/{project}/{application}?format=md`
 * through `shared/lib/download`'s already-ported `exportMarkdown`, and
 * `DELETE /elitea_core/application/prompt_lib/{project}/{application}`.
 *
 * The delete DISARMS the unsaved-changes nav blocker before it navigates. The
 * editor arms that guard from its own dirty state, and without this the reader
 * is asked to confirm unsaved changes to a pipeline that no longer exists —
 * the same #133 contract `pages/agents/ui/EditApplicationActions.tsx` follows.
 */
export interface PipelineEntityActionsOptions {
  readonly projectId: string | undefined;
  readonly applicationId: string | undefined;
  readonly name: string | undefined;
  /** The open version; export follows exactly this one, matching `ExportApplicationButton`'s contract. */
  readonly versionId: string | undefined;
  /** The list tab to return to once the pipeline is gone. */
  readonly tab: string | undefined;
  readonly onExportError: () => void;
  readonly onDeleteError: () => void;
}

export interface PipelineEntityActions {
  readonly isExporting: boolean;
  readonly isDeleting: boolean;
  readonly exportPipeline: () => Promise<void>;
  readonly deletePipeline: () => Promise<void>;
}

export function usePipelineEntityActions({
  projectId,
  applicationId,
  name,
  versionId,
  tab,
  onExportError,
  onDeleteError,
}: PipelineEntityActionsOptions): PipelineEntityActions {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [isExporting, setIsExporting] = useState(false);
  const [isDeleting, setIsDeleting] = useState(false);

  const exportPipeline = useCallback(async () => {
    const config = getConfig();
    if (config.status !== 'ok' || projectId === undefined || applicationId === undefined) return;
    setIsExporting(true);
    try {
      const result = await exportMarkdown(
        {
          baseUrl: config.config.vite_server_url,
          projectId,
          applicationId,
          ...(versionId === undefined ? {} : { followVersionIds: [versionId] }),
        },
        name ?? applicationId,
      );
      if (!result.ok) onExportError();
    } finally {
      setIsExporting(false);
    }
  }, [projectId, applicationId, versionId, name, onExportError]);

  const deletePipeline = useCallback(async () => {
    if (projectId === undefined || applicationId === undefined) return;
    setIsDeleting(true);
    try {
      await queryClient.query(getDeleteApplicationQueryOptions(projectId, Number(applicationId)));
      disarmUnsavedChangesNavBlocker();
      void navigate({ to: '/pipelines/$tab', params: { tab: tab ?? 'all' } });
    } catch {
      // Handled (§3.6): a refused delete is reported to the reader, who stays
      // on the page. Rethrowing would take down the editor for a recoverable
      // server answer.
      onDeleteError();
    } finally {
      setIsDeleting(false);
    }
  }, [projectId, applicationId, queryClient, navigate, tab, onDeleteError]);

  return { isExporting, isDeleting, exportPipeline, deletePipeline };
}
