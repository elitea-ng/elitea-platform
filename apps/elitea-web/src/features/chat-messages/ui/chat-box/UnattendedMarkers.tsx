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

/**
 * The answer's Local work turn ran a command outside the sandbox (ADR-0061 decision 5, #3069): Main stamps
 * `meta.local_work.ran_outside_sandbox` from the commit's trust record. A still label, never a colour alone.
 */
export function RanOutsideSandboxMarker() {
  return (
    <Typography variant="labelSmall" color="warning.main" data-testid="chat-message-ran-outside-sandbox" sx={{ display: 'block', mt: 0.5 }}>
      {t('features.chatMessages.ranOutsideSandbox', 'Ran outside the sandbox')}
    </Typography>
  );
}

/**
 * The answer's Local work turn ran in Full auto (ADR-0061 amendment of 2026-11-26, issue 3079): the agent answered
 * every approval, including those asked after it read outside content. Main stores `meta.local_work.full_auto`.
 */
export function FullAutoMarker() {
  return (
    <Typography variant="labelSmall" color="error.main" data-testid="chat-message-full-auto" sx={{ display: 'block', mt: 0.5 }}>
      {t('features.chatMessages.fullAuto', 'Ran in Full auto')}
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
