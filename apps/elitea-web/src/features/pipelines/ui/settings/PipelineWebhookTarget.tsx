import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { ThreadTargetSection, useThreadTargetForm, type StoredThreadTarget, type ThreadTargetFieldsBody } from '@/entities/thread-target';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

export interface WebhookTargetSectionProps {
  /** Absent: the dialog shows no "Runs" choice. */
  readonly projectId: string | undefined;
  readonly open: boolean;
  readonly stored: StoredThreadTarget | undefined;
  readonly isLoading: boolean;
  /** Rotates the credential with the new target; resolves to the error the rotation caught. */
  readonly onApply: (target: ThreadTargetFieldsBody) => unknown;
}

const sectionSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };

/**
 * The webhook dialog's "Runs" choice (ADR-0045 decision 9): this pipeline, or
 * a turn on a handed-off workspace thread.
 *
 * The target is written on the create/rotate route and nowhere else, so
 * applying it rotates the credential, as a change of type does — and the
 * dialog says so beside the button, in the words the type change uses. A
 * refusal of the target stays here, on its field.
 */
export function WebhookTargetSection({ projectId, open, stored, isLoading, onApply }: WebhookTargetSectionProps): ReactNode {
  const form = useThreadTargetForm(open, stored, 'pipeline');
  if (projectId === undefined) return null;
  const apply = (): void => {
    const pending = form.pending();
    if (pending.kind !== 'body') return;
    void form.submit(() => onApply(pending.body));
  };
  return (
    <Box sx={sectionSx}>
      <ThreadTargetSection
        projectId={projectId}
        {...form.section}
        disabled={isLoading}
      />
      {form.section.choice === 'thread' && (
        <Typography
          variant="bodySmall"
          color="text.secondary"
          data-testid="pipeline-webhook-thread-hint"
        >
          {t('pipelines.pipelineWebhookModal.threadHint', 'Each call starts a turn on the thread with the message, then the call’s input or payload. A call while the thread is busy is answered 409.')}
        </Typography>
      )}
      {form.changed && (
        <Box sx={rowSx}>
          <Typography
            variant="bodySmall"
            color="text.secondary"
            data-testid="pipeline-webhook-target-pending"
          >
            {t('pipelines.pipelineWebhookModal.targetPending', 'Applying this target rotates the credential: the current secret stops working and the sender has to be given the new one.')}
          </Typography>
          <BaseBtn
            variant="secondary"
            onClick={apply}
            disabled={isLoading}
            data-testid="pipeline-webhook-apply-target"
          >
            {t('pipelines.pipelineWebhookModal.applyMode', 'Apply and rotate')}
          </BaseBtn>
        </Box>
      )}
    </Box>
  );
}
