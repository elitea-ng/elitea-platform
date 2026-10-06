import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Typography from '@mui/material/Typography';
import { t } from '@/shared/i18n';
import type { NodeRecoveryBinding } from '@/shared/lib/nodeRecovery';

/** This notice does not grant operator recovery controls. */
export function NodeRecoveryNotice({ binding }: { readonly binding: NodeRecoveryBinding }) {
  const timedOut = binding.receipt.failure_class === 'attempt_timeout';
  return <Alert severity="warning">
    <AlertTitle>{t('features.chatMessages.nodeRecovery.paused', 'Execution paused')}</AlertTitle>
    <Typography variant="bodySmall" component="p">
      {t('features.chatMessages.nodeRecovery.visit', 'Node {{node}}, attempt {{attempt}} requires operator recovery.', {
        node: binding.receipt.node_id, attempt: binding.receipt.attempt,
      })}
    </Typography>
    <Typography variant="bodySmall" component="p">
      {timedOut ? t('features.chatMessages.nodeRecovery.timeout', 'The node reached its execution time limit.')
        : t('features.chatMessages.nodeRecovery.required', 'The node cannot continue without an authorized recovery action.')}
    </Typography>
    <Typography variant="bodySmall" component="p">
      {t('features.chatMessages.nodeRecovery.disabled', 'Contact an operator to recover this execution. You can stop this execution before starting another test or message.')}
    </Typography>
  </Alert>;
}
