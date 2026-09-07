/**
 * The DATASETS sub-view of the agent editor's Evaluation tab.
 *
 * A dataset is a named set of cases: what the agent is asked, and the answer
 * the author expected if they stated one. A run walks it once per case.
 *
 * WHAT IS DELIBERATELY NOT HERE. The reference's dataset view also imports
 * CSV/JSON and promotes conversations into cases. Neither has a route in this
 * release, so neither control is drawn — a button that 404s is worse than a
 * control that is absent.
 */
import { useCallback, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { AddButton } from '@/shared/ui/AddButton';

import { useEvalDatasetMutations, useEvalDatasets } from '../model/useEvalRuns';
import { useEvaluationPermissions } from '../model/useEvaluationPermissions';
import { DatasetCasesPanel } from './DatasetCasesPanel';
import { DatasetCreateDialog } from './DatasetCreateDialog';
import { EvaluationStatus } from './EvaluationStatus';

const rootSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const headerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between' };
const rowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.75rem',
  padding: '0.5rem 0.75rem',
  borderRadius: 'var(--el-shape-radiusSm, 4px)',
  cursor: 'pointer',
};
const centeredSx: SxProps<Theme> = { display: 'flex', justifyContent: 'center', padding: '2rem' };

export interface EvaluationDatasetsViewProps {
  readonly projectId: string | undefined;
  /** The agent being edited. Its own datasets widen the listing beyond the project-wide ones. */
  readonly applicationId: number | undefined;
}

export function EvaluationDatasetsView(props: EvaluationDatasetsViewProps): ReactNode {
  const { projectId, applicationId } = props;
  const permissions = useEvaluationPermissions(projectId);
  const [isCreateOpen, setCreateOpen] = useState(false);
  const [selectedId, setSelectedId] = useState<string | undefined>(undefined);

  // The QUERY is gated, not just the controls. Without the read permission the
  // request would answer 403, and a 403 rendered as an error banner tells a
  // viewer their product is broken when they simply may not read datasets.
  const datasetsQuery = useEvalDatasets(permissions.canReadDatasets ? projectId : undefined, applicationId);
  const mutations = useEvalDatasetMutations(projectId);

  const handleCreated = useCallback(() => setCreateOpen(false), []);

  if (!permissions.canReadDatasets) {
    return (
      <Typography component="output" variant="bodyMedium">
        {t(
          'features.agentEvaluation.datasets.noReadPermission',
          'You do not have permission to view this project’s evaluation datasets.',
        )}
      </Typography>
    );
  }

  const datasets = datasetsQuery.data ?? [];

  return (
    <Box sx={rootSx} data-testid="evaluation-datasets-view">
      <Box sx={headerSx}>
        <Typography variant="labelMedium">
          {t('features.agentEvaluation.datasets.title', 'Datasets')}
        </Typography>
        {permissions.canCreateDatasets && (
          <AddButton
            onAdd={() => setCreateOpen(true)}
            tooltip={t('features.agentEvaluation.datasets.add', 'Add dataset')}
          />
        )}
      </Box>

      {datasetsQuery.isPending ? (
        <Box sx={centeredSx}>
          <CircularProgress aria-label={t('features.agentEvaluation.datasets.loading', 'Loading datasets')} />
        </Box>
      ) : (
        <EvaluationStatus
          isError={datasetsQuery.isError}
          isEmpty={datasets.length === 0}
          errorText={t('features.agentEvaluation.datasets.loadFailed', 'Failed to load the evaluation datasets.')}
          emptyText={t(
            'features.agentEvaluation.datasets.empty',
            'No datasets yet. Add one, then add the questions you want the agent scored on.',
          )}
        />
      )}

      {datasets.map((dataset) => (
        <Box
          key={dataset.id}
          // A REAL <button>, not a div wearing role="button". oxlint's
          // jsx-a11y/prefer-tag-over-role refuses the attribute, and the tag
          // brings the keyboard behaviour with it — Enter and Space already
          // activate a button, which the hand-written key handler this replaced
          // had to reimplement (and would have had to keep correct).
          component="button"
          type="button"
          sx={rowSx}
          data-testid={`evaluation-dataset-row-${dataset.id}`}
          aria-pressed={selectedId === dataset.id}
          onClick={() => setSelectedId(selectedId === dataset.id ? undefined : dataset.id)}
        >
          <Typography variant="bodyMedium">{dataset.name}</Typography>
          {/*
            `case_count` is the STORED count and not the length of the page the
            detail read returns. A badge built from the page would report a
            large dataset as having exactly the page size.
          */}
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentEvaluation.datasets.caseCount', '{{count}} case(s)', {
              count: dataset.case_count,
            })}
          </Typography>
        </Box>
      ))}

      {selectedId !== undefined && (
        <DatasetCasesPanel
          projectId={projectId}
          datasetId={selectedId}
          canEdit={permissions.canUpdateDatasets}
          canDelete={permissions.canDeleteDatasets}
          onDeleteDataset={() => {
            mutations.remove.mutate(selectedId, { onSuccess: () => setSelectedId(undefined) });
          }}
        />
      )}

      <DatasetCreateDialog
        open={isCreateOpen}
        applicationId={applicationId}
        isSaving={mutations.create.isPending}
        error={mutations.create.error}
        onClose={() => setCreateOpen(false)}
        onSubmit={(input) => mutations.create.mutate(input, { onSuccess: handleCreated })}
      />
    </Box>
  );
}
