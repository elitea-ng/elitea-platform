/**
 * The Evaluation tab's own sub-navigation: Library, Datasets, Runs.
 *
 * THREE ENTRIES NOW, AND ONE BEFORE. The tab strip that mounts this used to say
 * "ONE TAB, NOT THREE ... only the Library has a backend in this release", and
 * that was true of slice 1. Slice 2 (#617) serves the dataset, the run and the
 * scorecard, so Datasets and Runs have backends and appear. The reference's
 * fourth entry — Suite config — still does not: there is no suite table, and a
 * sub-tab that renders an empty panel is indistinguishable, to the person
 * looking at it, from a feature that is broken.
 *
 * EACH PANEL IS UNMOUNTED WHEN IT IS NOT SELECTED, unlike the agent editor's
 * outer strip. The outer one keeps the configuration panel mounted because it
 * holds unsaved edits; nothing here does. Unmounting stops the run poll when
 * the person is not looking at it, which is the difference between a request
 * every three seconds and a request every three seconds for the rest of the
 * session.
 */
import { useState, type ReactNode, type SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';

import { EvaluationDatasetsView } from './EvaluationDatasetsView';
import { EvaluationLibraryView } from './EvaluationLibraryView';
import { EvaluationRunsView } from './EvaluationRunsView';

const EVALUATION_VIEWS = {
  library: 'library',
  datasets: 'datasets',
  runs: 'runs',
} as const;
type EvaluationView = (typeof EVALUATION_VIEWS)[keyof typeof EVALUATION_VIEWS];

const stripSx: SxProps<Theme> = { marginBottom: '0.75rem' };

export interface EvaluationPanelProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  /** The version a run scores. `undefined` disables the start control with a stated reason. */
  readonly applicationVersionId: number | undefined;
}

export function EvaluationPanel(props: EvaluationPanelProps): ReactNode {
  const { projectId, applicationId, applicationVersionId } = props;
  const [view, setView] = useState<EvaluationView>(EVALUATION_VIEWS.library);

  const handleChange = (_event: SyntheticEvent, value: EvaluationView): void => setView(value);

  return (
    <Box data-testid="evaluation-panel">
      <BaseTabs
        sx={stripSx}
        value={view}
        onChange={handleChange}
        aria-label={t('features.agentEvaluation.views.label', 'Evaluation sections')}
      >
        <BaseTab
          value={EVALUATION_VIEWS.library}
          label={t('features.agentEvaluation.views.library', 'Library')}
          data-testid="evaluation-view-library"
        />
        <BaseTab
          value={EVALUATION_VIEWS.datasets}
          label={t('features.agentEvaluation.views.datasets', 'Datasets')}
          data-testid="evaluation-view-datasets"
        />
        <BaseTab
          value={EVALUATION_VIEWS.runs}
          label={t('features.agentEvaluation.views.runs', 'Runs')}
          data-testid="evaluation-view-runs"
        />
      </BaseTabs>

      {view === EVALUATION_VIEWS.library && (
        <EvaluationLibraryView projectId={projectId} applicationId={applicationId} />
      )}
      {view === EVALUATION_VIEWS.datasets && (
        <EvaluationDatasetsView projectId={projectId} applicationId={applicationId} />
      )}
      {view === EVALUATION_VIEWS.runs && (
        <EvaluationRunsView
          projectId={projectId}
          applicationId={applicationId}
          applicationVersionId={applicationVersionId}
        />
      )}
    </Box>
  );
}
