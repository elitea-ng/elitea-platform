import type { ReactNode } from 'react';
import { useMemo } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';

import { usePipelineTriggerSettings } from '../lib/usePipelineTriggerSettings';
import { EditPipelineScheduleCard } from './EditPipelineScheduleCard';
import { EditPipelineTriggerCard } from './EditPipelineTriggerCard';

const sectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.5rem' };

export interface EditPipelineTriggersPanelProps {
  readonly projectId: string | undefined;
  readonly versionId: number | undefined;
  readonly isReadOnly: boolean;
}

/**
 * "Triggers & schedules" — the pipeline's two UNATTENDED entry points, issues
 * 192 and 193, in one collapsible section of the configuration form.
 *
 * It is a separate file from `EditPipelineConfigurationPanel` for the reason
 * `EditPipelineToolsPanel` already is: that panel is a composition root and
 * this is a stateful feature with its own queries. Splitting it also keeps
 * both files inside the §3.5 400-line and 12-prop budgets.
 *
 * The two halves are ONE section rather than two, because a person asking
 * "how does this pipeline start when nobody is watching?" has one question
 * with two answers, and the recorded decision on both issues put them in the
 * same tab.
 *
 * The section renders NOTHING until the pipeline version is resolved. Showing
 * a "create trigger URL" button for a version id the page does not have yet
 * would offer an action whose first click cannot work.
 */
export function EditPipelineTriggersPanel(props: EditPipelineTriggersPanelProps): ReactNode {
  const { projectId, versionId, isReadOnly } = props;
  const settings = usePipelineTriggerSettings({ projectId, versionId });

  const items = useMemo(
    () => [
      {
        title: t('pages.pipelines.editPipeline.triggers.title', 'Triggers & schedules'),
        content: (
          <Box sx={sectionSx}>
            {settings.error !== undefined && (
              <Typography variant="bodySmall" color="error" data-testid="pipeline-triggers-error">
                {t('pages.pipelines.editPipeline.triggers.error', 'That change could not be saved. Try again.')}
              </Typography>
            )}
            <EditPipelineTriggerCard
              trigger={settings.trigger}
              secretUrl={settings.secretUrl}
              isBusy={settings.isBusy}
              isReadOnly={isReadOnly}
              onRotate={() => void settings.rotate()}
              onReveal={() => void settings.reveal()}
              onRevoke={() => void settings.revoke()}
              onHideSecret={settings.hideSecret}
            />
            <EditPipelineScheduleCard
              schedule={settings.schedule}
              isBusy={settings.isBusy}
              isReadOnly={isReadOnly}
              onSave={(cron, active) => void settings.saveSchedule(cron, active)}
              onRemove={() => void settings.removeSchedule()}
            />
          </Box>
        ),
      },
    ],
    [settings, isReadOnly],
  );

  if (projectId === undefined || versionId === undefined) return null;

  return <BasicAccordion items={items} data-testid="edit-pipeline-triggers-panel" />;
}
