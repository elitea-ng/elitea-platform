import type { ReactNode } from 'react';
import { useCallback, useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import type { PipelineSchedule } from '@/shared/api/generated/model';
import { CronField, parseCronExpression } from '@/shared/ui/cron';

/** The reference's own default for a pipeline schedule: weekly, Saturday at midnight. */
const DEFAULT_CRON = '0 0 * * 6';

const rowSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const actionsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.5rem', alignItems: 'center' };

/**
 * The last-fire outcomes the backend's `last_result` column may hold. The set
 * is CLOSED there (a CHECK constraint), and it is closed here for the reason
 * that makes the column worth having: an unmapped value would render as
 * nothing at all, which is the empty state this feature exists to remove.
 */
function lastResultText(result: string | undefined, detail: string | undefined): string | undefined {
  switch (result) {
    case 'dispatched':
      return t('pages.pipelines.editPipeline.triggers.schedule.resultDispatched', 'The last run started.');
    case 'skipped_overlap':
      return t(
        'pages.pipelines.editPipeline.triggers.schedule.resultOverlap',
        'Skipped: the previous run had not finished.',
      );
    case 'skipped_unauthorized':
      return t(
        'pages.pipelines.editPipeline.triggers.schedule.resultUnauthorized',
        'Skipped: the schedule author can no longer run agents in this project.',
      );
    case 'skipped_missing_version':
      return t(
        'pages.pipelines.editPipeline.triggers.schedule.resultMissing',
        'Skipped: this pipeline version is gone.',
      );
    case 'failed':
      return t('pages.pipelines.editPipeline.triggers.schedule.resultFailed', 'The last run could not start.');
    case undefined:
      return undefined;
    default:
      // A value the backend added and this build does not know. Showing its
      // DETAIL is better than showing nothing: an unmapped outcome would
      // otherwise render as the empty state this column exists to remove.
      return detail;
  }
}

/** The two read-only lines under the editor, split out to keep the card inside the §3.5 complexity budget. */
function ScheduleStatus(props: {
  readonly schedule: PipelineSchedule | undefined;
  readonly outcome: string | undefined;
}): ReactNode {
  const { schedule, outcome } = props;
  const nextRun = schedule?.configured === true ? schedule.next_run : undefined;
  return (
    <>
      {typeof nextRun === 'string' && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="pipeline-schedule-next-run">
          {t('pages.pipelines.editPipeline.triggers.schedule.nextRun', 'Next run: {{when}}', {
            when: new Date(nextRun).toLocaleString(),
          })}
        </Typography>
      )}
      {outcome !== undefined && (
        <Typography variant="bodySmall" color="text.secondary" data-testid="pipeline-schedule-last-result">
          {outcome}
        </Typography>
      )}
    </>
  );
}

export interface EditPipelineScheduleCardProps {
  readonly schedule: PipelineSchedule | undefined;
  readonly isBusy: boolean;
  readonly isReadOnly: boolean;
  readonly onSave: (cron: string, active: boolean) => void;
  readonly onRemove: () => void;
}

/**
 * The cron schedule — issue 193.
 *
 * ## The next-run preview comes from the SERVER
 *
 * `next_run` is computed by the same parser that will fire the row, so the
 * preview cannot disagree with the behaviour. `CronField`'s own prose preview
 * ("At 00:00, only on Saturday") is shown as well and answers a different
 * question — what the expression MEANS — which is what a person editing it
 * needs before they save.
 *
 * ## Who it runs as is stated, not implied
 *
 * A scheduled run executes as the person who last saved the schedule, and
 * their permission is re-checked every time it fires. That is a surprising
 * enough rule that the card says it, rather than leaving a person to discover
 * it when a colleague's schedule stops firing.
 */
export function EditPipelineScheduleCard(props: EditPipelineScheduleCardProps): ReactNode {
  const { schedule, isBusy, isReadOnly, onSave, onRemove } = props;

  const [cron, setCron] = useState(DEFAULT_CRON);
  const [active, setActive] = useState(false);

  // The stored schedule seeds the editor ONCE it arrives. Without the guard a
  // refetch mid-edit would discard what the person had typed.
  const storedCron = schedule?.cron;
  const storedActive = schedule?.active;
  useEffect(() => {
    if (typeof storedCron === 'string' && storedCron !== '') setCron(storedCron);
    if (typeof storedActive === 'boolean') setActive(storedActive);
  }, [storedCron, storedActive]);

  const parsed = parseCronExpression(cron);
  const onSaveClick = useCallback(() => onSave(cron, active), [onSave, cron, active]);

  const disabled = isBusy || isReadOnly;
  const configured = schedule?.configured === true;
  const outcome = lastResultText(schedule?.last_result, schedule?.last_result_detail);

  return (
    <Box sx={rowSx} data-testid="pipeline-schedule-card">
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'pages.pipelines.editPipeline.triggers.schedule.description',
          'The platform runs this pipeline version on a schedule, as the person who last saved it.',
        )}
      </Typography>

      <CronField value={cron} onChange={setCron} disabled={disabled} id="pipeline-schedule-cron" />

      <FormControlLabel
        control={
          <Switch
            checked={active}
            disabled={disabled}
            onChange={(_event, checked) => setActive(checked)}
            slotProps={{ input: { 'aria-label': t('pages.pipelines.editPipeline.triggers.schedule.enable', 'Enabled') } }}
          />
        }
        label={t('pages.pipelines.editPipeline.triggers.schedule.enable', 'Enabled')}
      />

      <ScheduleStatus schedule={schedule} outcome={outcome} />

      <Box sx={actionsSx}>
        <Button
          variant="contained"
          size="small"
          disabled={disabled || !parsed.ok}
          onClick={onSaveClick}
          data-testid="pipeline-schedule-save"
        >
          {t('pages.pipelines.editPipeline.triggers.schedule.save', 'Save schedule')}
        </Button>
        {configured && (
          <Button
            variant="outlined"
            color="error"
            size="small"
            disabled={disabled}
            onClick={onRemove}
            data-testid="pipeline-schedule-remove"
          >
            {t('pages.pipelines.editPipeline.triggers.schedule.remove', 'Remove schedule')}
          </Button>
        )}
      </Box>
    </Box>
  );
}
