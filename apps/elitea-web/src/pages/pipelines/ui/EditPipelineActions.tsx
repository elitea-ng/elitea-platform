import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ApplicationDetail, ApplicationVersionDetail } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { EntityLifecycleControls } from '@/features/agent-lifecycle';

import { useForkTargetProjects } from '../lib/useForkTargetProjects';
import { usePipelineEntityActions } from '../lib/usePipelineEntityActions';
import { ChatWithPipelineButton } from './ChatWithPipelineButton';

/**
 * The pipeline editor's entity-level actions, mounted next to the save bar —
 * the pipelines twin of `pages/agents/ui/EditApplicationActions.tsx`. Split
 * into its own file rather than inlined into `EditPipeline.tsx` for the same
 * reason the agents page did it: the error state and its `?.` unwrapping would
 * push that page over its §3.5 complexity/line budgets.
 *
 * EXPORT AND DELETE ARE REAL NOW. This file used to state that both "have no
 * pipeline-side mount point yet", and the legacy suite's
 * `test_delete_pipeline_via_ui_menu` and `test_export_pipeline_if_available`
 * had nothing to drive. Both are items of the `⋮` menu, which is where the
 * reference puts them (Share, Fork, Export, Delete) — the agent editor keeps
 * its own toolbar buttons instead, so neither control is offered twice. The
 * work itself is `../lib/usePipelineEntityActions.ts`; delete is confirmed by
 * typing the pipeline's name, the same gate the agent editor uses.
 *
 * Rendered only for a writer (`EditPipeline` gates on the same
 * `isReadOnlyView` it already uses for the save bar): the participant's
 * `entity_meta.project_id` must equal the conversation's project, so a
 * public viewer's chat would attach cleanly and then refuse every message —
 * see `ChatWithPipelineButton`'s own doc comment.
 */
export interface EditPipelineActionsProps {
  /** The pipeline's id, as the route carries it. `undefined` while the route param is unparseable — the action then renders disabled rather than acting on a wrong id. */
  readonly applicationId: string | undefined;
  readonly detail: ApplicationDetail | undefined;
  /** Currently-open version — the Chat button pins the conversation's participant to it. */
  readonly activeVersion: ApplicationVersionDetail | undefined;
  /** The selected project — the Chat button creates its conversation there. */
  readonly projectId: string | undefined;
  /** The list tab the editor was opened from; the share link points back at it. */
  readonly tab: string | undefined;
}

export function EditPipelineActions({ applicationId, detail, activeVersion, projectId, tab }: EditPipelineActionsProps): ReactNode {
  const forkTargets = useForkTargetProjects(projectId);
  const [error, setError] = useState<string | undefined>(undefined);
  const [deleteOpen, setDeleteOpen] = useState(false);

  const handleChatError = useCallback(
    () => setError(t('pages.pipelines.editPipeline.chatError', 'Failed to open a chat with this pipeline.')),
    [],
  );
  const handleExportError = useCallback(
    () => setError(t('pages.pipelines.editPipeline.exportError', 'Failed to export this pipeline.')),
    [],
  );
  const handleDeleteError = useCallback(
    () => setError(t('pages.pipelines.editPipeline.deleteError', 'Failed to delete this pipeline.')),
    [],
  );

  const actions = usePipelineEntityActions({
    projectId,
    applicationId,
    name: detail?.name,
    versionId: activeVersion?.id,
    tab,
    onExportError: handleExportError,
    onDeleteError: handleDeleteError,
  });

  return (
    <Box sx={wrapperSx}>
      {error !== undefined && (
        <Typography
          role="alert"
          variant="bodySmall"
        >
          {error}
        </Typography>
      )}
      <ChatWithPipelineButton
        projectId={projectId}
        applicationId={applicationId}
        name={detail?.name}
        activeVersion={activeVersion}
        onError={handleChatError}
      />
      {/*
       * Share and Fork, the two lifecycle affordances a pipeline can actually
       * use. It gets NO publish item: `Publish` refuses `agent_type =
       * 'pipeline'` with 400 `pipeline_not_publishable`
       * (internal/api/v2/eliteacore/handler.go), so the control is omitted
       * rather than offered and refused. `EntityLifecycleControls` makes that
       * choice from its `entity` prop.
       */}
      <EntityLifecycleControls
        entity="pipelines"
        projectId={projectId}
        projects={forkTargets}
        entityId={applicationId}
        entityName={detail?.name ?? ''}
        tab={tab}
        activeVersionId={activeVersion?.id}
        activeVersionStatus={activeVersion?.status}
        onExport={() => {
          setError(undefined);
          void actions.exportPipeline();
        }}
        onDelete={() => {
          setError(undefined);
          setDeleteOpen(true);
        }}
      />
      <DeleteEntityModal
        open={deleteOpen}
        onClose={() => setDeleteOpen(false)}
        onConfirm={() => {
          void actions.deletePipeline().then(() => setDeleteOpen(false));
        }}
        shouldRequestInputName
        confirming={actions.isDeleting}
        data-testid="pipeline-delete-modal"
        copy={{ title: t('pages.pipelines.editPipeline.deleteTitle', 'Delete pipeline') }}
        {...(detail?.name === undefined ? {} : { name: detail.name })}
      />
    </Box>
  );
}

const wrapperSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.25rem' };
