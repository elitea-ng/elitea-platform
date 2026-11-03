/**
 * The marks of an unattended run in the chat transcript (ADR-0045 decision 4),
 * in the wording of the desktop transcript: "Continued automatically" on a
 * turn that Main started by itself, and the step-limit stop on an answer.
 */
import Alert from '@mui/material/Alert';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

/** Main started this turn by itself, the n-th time in its chain (when known). */
export function ContinuedAutomaticallyLabel({ index }: { readonly index?: number | undefined }) {
  return (
    <Typography variant="labelSmall" color="text.secondary" data-testid="chat-message-continued-automatically" sx={{ display: 'block', mt: 0.5 }}>
      {index === undefined
        ? t('features.chatMessages.continuedAutomatically', 'Continued automatically')
        : t('workspace.chain.continued', 'Continued automatically ({{n}})', { n: String(index) })}
    </Typography>
  );
}

/** The answer stopped at its step limit: under a grant that continues the run, a continuation follows. */
export function StepLimitNotice() {
  return (
    <Alert severity="info" data-testid="chat-message-step-limit" sx={{ alignSelf: 'stretch', width: '100%' }}>
      {t('features.chatMessages.stepLimitReached', 'The run stopped at its step limit. When the unattended grant allows it, the run continues automatically in a new turn.')}
    </Alert>
  );
}
