/**
 * The Evaluation tab of the agent editor — the DIMENSION LIBRARY.
 *
 * Ported from the baseline's `widgets/evaluation/ui/library/LibraryView.jsx`.
 *
 * WHAT IS DELIBERATELY NOT HERE. The baseline's Evaluation tab has three
 * sub-views (Suite config, Library, Datasets) and this is only the second one,
 * because it is the only one with a backend. There is no sub-navigation, no
 * suite, no dataset, no run and no scorecard: a tab strip whose other entries
 * render an empty panel is worse than a tab strip with one entry, and a route
 * that 404s is worse than a control that is absent.
 */
import { useCallback, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { AddButton } from '@/shared/ui/AddButton';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { useEvalDimensionMutations, useEvalDimensions } from '../model/useEvalDimensions';
import { useEvaluationPermissions } from '../model/useEvaluationPermissions';
import type { EvalDimension } from '../model/types';
import { DimensionEditorDialog } from './DimensionEditorDialog';
import { DimensionRow } from './DimensionRow';

const rootSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
};
const headerSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
};
const centeredSx: SxProps<Theme> = {
  display: 'flex',
  justifyContent: 'center',
  padding: '2rem',
};

export interface EvaluationLibraryViewProps {
  readonly projectId: string | undefined;
  /** The agent being edited. Its ad-hoc dimensions widen the listing, and it is what "This agent only" scopes to. */
  readonly applicationId: number | undefined;
}

interface EditorState {
  readonly open: boolean;
  readonly dimension: EvalDimension | undefined;
}

interface LibraryStatusProps {
  readonly isLoading: boolean;
  readonly isError: boolean;
  readonly isEmpty: boolean;
}

/**
 * The three non-list states, split out of `EvaluationLibraryView` to keep that
 * component under the §3.5 cyclomatic-complexity budget (12).
 *
 * The empty state is REPORTED, not left blank: a library with no rows and no
 * message is indistinguishable from a listing that failed silently, which is
 * the reading every "200 with an empty screen" defect in this app produces.
 */
function LibraryStatus({ isLoading, isError, isEmpty }: LibraryStatusProps): ReactNode {
  if (isLoading) {
    return (
      <Box sx={centeredSx}>
        <CircularProgress />
      </Box>
    );
  }
  if (isError) {
    return (
      <Typography
        role="alert"
        variant="bodyMedium"
        component="p"
        data-testid="evaluation-library-error"
      >
        {t('features.agentEvaluation.loadFailed', 'Failed to load the evaluation library.')}
      </Typography>
    );
  }
  if (isEmpty) {
    return (
      <Typography
        variant="bodyMedium"
        component="p"
        color="text.secondary"
        data-testid="evaluation-library-empty"
      >
        {t('features.agentEvaluation.empty', 'No dimensions yet.')}
      </Typography>
    );
  }
  return null;
}

export function EvaluationLibraryView(props: EvaluationLibraryViewProps): ReactNode {
  const { projectId, applicationId } = props;

  const permissions = useEvaluationPermissions(projectId);
  // The listing query is gated on the READ permission, not merely rendered
  // behind it: without the grant the request answers 403, and a 403 surfaced as
  // an error banner tells a viewer the product is broken when in fact they may
  // simply not author rubrics.
  const query = useEvalDimensions(permissions.canRead ? projectId : undefined, applicationId);
  const mutations = useEvalDimensionMutations(projectId);

  const [editor, setEditor] = useState<EditorState>({
    open: false,
    dimension: undefined,
  });
  const [deleteTarget, setDeleteTarget] = useState<EvalDimension | undefined>(undefined);

  const openEditor = useCallback((dimension?: EvalDimension): void => {
    setEditor({ open: true, dimension });
  }, []);
  const closeEditor = useCallback((): void => setEditor({ open: false, dimension: undefined }), []);

  const confirmDelete = useCallback((): void => {
    if (!deleteTarget) return;
    mutations.remove.mutateAsync(deleteTarget.id).then(
      () => setDeleteTarget(undefined),
      () => setDeleteTarget(undefined),
    );
  }, [deleteTarget, mutations]);

  if (!permissions.canRead) {
    return (
      <Box
        sx={centeredSx}
        data-testid="evaluation-library-view"
      >
        <Typography
          variant="bodyMedium"
          component="p"
          color="text.secondary"
        >
          {t(
            'features.agentEvaluation.noReadPermission',
            'You do not have permission to view this project’s evaluation library.',
          )}
        </Typography>
      </Box>
    );
  }

  return (
    <Box
      sx={rootSx}
      data-testid="evaluation-library-view"
    >
      <Box sx={headerSx}>
        <Box>
          <Typography variant="headingSmall" component="h2">{t('features.agentEvaluation.title', 'Evaluation library')}</Typography>
          <Typography
            variant="bodyMedium"
            component="p"
            color="text.secondary"
          >
            {t('features.agentEvaluation.subtitle', 'Define reusable scoring dimensions for this project.')}
          </Typography>
        </Box>
        {permissions.canCreate && (
          <AddButton
            tooltip={t('features.agentEvaluation.addDimension', 'New dimension')}
            onAdd={() => openEditor()}
          />
        )}
      </Box>

      <LibraryStatus
        isLoading={query.isLoading}
        isError={query.isError}
        isEmpty={(query.data ?? []).length === 0}
      />

      {(query.data ?? []).map((dimension) => (
        <DimensionRow
          key={dimension.id}
          dimension={dimension}
          canEdit={permissions.canUpdate}
          canDelete={permissions.canDelete}
          onEdit={openEditor}
          onDelete={setDeleteTarget}
        />
      ))}

      <DimensionEditorDialog
        open={editor.open}
        projectId={projectId}
        applicationId={applicationId}
        dimension={editor.dimension}
        onClose={closeEditor}
      />

      <DeleteEntityModal
        open={deleteTarget !== undefined}
        name={deleteTarget?.name ?? ''}
        confirming={mutations.remove.isPending}
        onClose={() => setDeleteTarget(undefined)}
        onConfirm={confirmDelete}
        data-testid="evaluation-delete-dimension-modal"
      />
    </Box>
  );
}
