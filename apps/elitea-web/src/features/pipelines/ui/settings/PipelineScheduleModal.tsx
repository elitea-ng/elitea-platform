import type { ReactNode } from 'react';
import { useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { ThreadTargetSection, useThreadTargetForm, type StoredThreadTarget, type ThreadTargetFieldsBody, type ThreadTargetForm } from '@/entities/thread-target';
import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';
import { InfoTooltip } from '@/shared/ui/InfoTooltip';
import { RadioButtonGroup } from '@/shared/ui/RadioButtonGroup';
import { CronField, describeCronState, parseCronExpression } from '@/shared/ui/cron';

const PIPELINE_CRON_DEFAULT = '0 0 * * 6';

export interface PipelineScheduleModalProps {
  readonly open: boolean;
  readonly onClose: () => void;
  /**
   * Saves the schedule. `target` is sent only when the "Runs" choice changes
   * what the schedule starts (ADR-0045 decision 9). With a target, the dialog
   * waits for the save: it resolves to the error the save caught, and a
   * refusal of the target stays in the dialog, on its field.
   */
  readonly onSubmit: (cronExpression: string, target?: ThreadTargetFieldsBody) => unknown;
  readonly cron?: string;
  readonly isLoading?: boolean;
  /** Shows the "Runs" choice (this pipeline, or a handed-off workspace thread); its thread picker lists this project's threads. */
  readonly projectId?: string | undefined;
  /** The stored schedule: its target seeds the "Runs" choice. */
  readonly target?: StoredThreadTarget | undefined;
}

const contentWrapperSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1.75rem', minWidth: '25rem' };
const cronWrapperSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', justifyContent: 'center', gap: '0.5rem' };
const descriptionContainerSx: SxProps<Theme> = { display: 'flex', justifyContent: 'center', alignItems: 'center', marginTop: '0.25rem', gap: '0.25rem' };
const cronDescriptionSx: SxProps<Theme> = { color: 'text.secondary', textAlign: 'center' };

/**
 * Apply: with no change of target, the save the dialog always made (and it
 * closes at once); with one, it waits and stays open on a refusal.
 */
function applySchedule(args: {
  readonly form: ThreadTargetForm;
  readonly showsTarget: boolean;
  readonly cronExpression: string;
  readonly onSubmit: PipelineScheduleModalProps['onSubmit'];
  readonly onClose: () => void;
}): void {
  const { form, showsTarget, cronExpression, onSubmit, onClose } = args;
  const pending = showsTarget ? form.pending() : { kind: 'unchanged' as const };
  if (pending.kind === 'invalid') return;
  if (pending.kind === 'unchanged') {
    onSubmit(cronExpression);
    onClose();
    return;
  }
  void form.submit(() => onSubmit(cronExpression, pending.body)).then(saved => {
    if (saved) onClose();
  });
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * settings/PipelineScheduleModal.jsx` (unit A2h) -- named as this exact
 * component's parity target by `shared/ui/cron`'s own module doc comment
 * ("Parity target: `IndexScheduleModal.jsx` / `PipelineScheduleModal.jsx`
 * ... wiring the mode toggle is Wave-2 feature work"), i.e. this file.
 *
 * `react-js-cron`'s `<Cron>` (Default mode) -> `shared/ui/cron`'s
 * `CronField` (unit S7, hand-rolled MUI replacement -- rejected `antd`
 * peer dependency, see that module's own doc comment). Advanced mode stays
 * a plain text field, per `CronField`'s own doc comment ("The 'Advanced'
 * raw-text mode from the old modals is a plain `TextField` at the call
 * site... Wave-2 modal-wiring scope, not part of this component").
 * `validateCronExpression` (baseline: `features/toolkits/indexes/lib/
 * helpers/indexSchedule.helpers.js`, a DIFFERENT slice --
 * `no-sideways-features`) -> `shared/ui/cron`'s own `parseCronExpression`,
 * built against the identical field grammar (`shared/ui/cron/model.ts`'s
 * own doc comment: "mirrors the field grammar `indexSchedule.helpers.js`
 * validates against").
 */
export function PipelineScheduleModal(props: PipelineScheduleModalProps): ReactNode {
  const { open, onClose, onSubmit, cron, isLoading = false, projectId, target } = props;
  const form = useThreadTargetForm(open, target, 'pipeline');

  const [cronExpression, setCronExpression] = useState(PIPELINE_CRON_DEFAULT);
  const [cronType, setCronType] = useState<'default' | 'advanced'>('default');

  useEffect(() => {
    if (open) {
      if (cron) setCronExpression(cron);
      return undefined;
    }
    return () => setCronType('default');
  }, [open, cron]);

  const parsed = useMemo(() => parseCronExpression(cronExpression), [cronExpression]);
  // `BaseModal`'s `actions.confirming` disables the Apply button while true
  // (`BaseModal.tsx`'s own `ModalActions`: `disabled={actions?.confirming}`)
  // -- reused here for "invalid or loading", not just "in flight", since
  // `BaseModalProps` has no separate validity-disable flag and omitting
  // `onConfirm` entirely (the baseline's `applyIsDisabled ? undefined :
  // applyChanges` shape) removes the button altogether rather than
  // disabling it.
  const applyIsDisabled = !parsed.ok || Boolean(isLoading);

  const applyChanges = (): void => applySchedule({ form, showsTarget: projectId !== undefined, cronExpression, onSubmit, onClose });

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      title={t('pipelines.pipelineScheduleModal.title', 'Schedule settings')}
      content={
        <Box sx={contentWrapperSx}>
          <Box sx={cronWrapperSx}>
            <Typography
              variant="headingSmall"
              color={parsed.ok ? 'text.secondary' : 'error'}
            >
              {/* Default mode leaves this blank because `CronField` already renders its
                  own internal preview (`CronField.tsx`'s own `describeCronState` call);
                  Advanced mode has no preview of its own, so it needs the description
                  here, matching the baseline's `IndexScheduleModal.jsx` /
                  `PipelineScheduleModal.jsx`, which always shows `cronState.message` in
                  this exact slot regardless of mode. */}
              {!parsed.ok ? parsed.error : cronType === 'advanced' ? describeCronState(parsed.state) : ''}
            </Typography>

            <RadioButtonGroup
              aria-label={t('pipelines.pipelineScheduleModal.scheduleType', 'Schedule type')}
              value={cronType}
              items={[
                { value: 'default', label: t('pipelines.pipelineScheduleModal.default', 'Default') },
                { value: 'advanced', label: t('pipelines.pipelineScheduleModal.advanced', 'Advanced') },
              ]}
              onChange={value => setCronType(value === 'advanced' ? 'advanced' : 'default')}
            />

            {cronType === 'default' ? (
              <CronField
                value={cronExpression}
                onChange={setCronExpression}
                disabled={isLoading}
              />
            ) : (
              <InputBase
                value={cronExpression}
                onChange={event => setCronExpression(event.target.value)}
                placeholder="* * * * *"
                error={!parsed.ok}
                disabled={isLoading}
              />
            )}

            <Box sx={descriptionContainerSx}>
              <Typography
                variant="bodySmall"
                sx={cronDescriptionSx}
              >
                {t('pipelines.pipelineScheduleModal.fieldOrder', 'minute - hour - day (month) - month - day (week)')}
              </Typography>
              <InfoTooltip
                title={t('pipelines.pipelineScheduleModal.cronHelp', 'Cron expression help')}
                href="https://crontab.guru/#*_*_*_*"
              />
            </Box>
          </Box>
          {projectId !== undefined && (
            <ThreadTargetSection
              projectId={projectId}
              {...form.section}
              disabled={isLoading}
            />
          )}
        </Box>
      }
      actions={{
        confirming: applyIsDisabled,
        confirmText: t('pipelines.pipelineScheduleModal.apply', 'Apply'),
        cancelText: t('pipelines.pipelineScheduleModal.cancel', 'Cancel'),
      }}
      onConfirm={applyChanges}
    />
  );
}
