import Alert from '@mui/material/Alert';
import { t } from '@/shared/i18n';
import { parseDelegationRefusal } from '../../lib/delegationRefusal';

/** FU-335: names the toolkit a refused delegation needs, above the run's failure text. */
export function DelegationRefusalNotice({ refusal }: { readonly refusal: unknown }) {
  const parsed = parseDelegationRefusal(refusal);
  if (!parsed) return null;
  const { node, toolkit } = parsed;
  let text: string;
  if (toolkit !== undefined && node === 'agent') {
    text = t('features.chatMessages.delegationRefusal.agentToolkit', 'A saved agent that an Agent node of this run runs needs the toolkit "{{toolkit}}", which its caller does not hold. Attach it to the caller, or remove it from the saved agent.', { toolkit });
  } else if (toolkit !== undefined) {
    text = t('features.chatMessages.delegationRefusal.pipelineToolkit', 'A saved pipeline that a pipeline node of this run runs needs the toolkit "{{toolkit}}", which its caller does not hold. Attach it to the caller, or remove it from the saved pipeline.', { toolkit });
  } else if (node === 'agent') {
    text = t('features.chatMessages.delegationRefusal.agentGrant', 'A saved agent that an Agent node of this run runs needs a grant its caller does not hold.');
  } else {
    text = t('features.chatMessages.delegationRefusal.pipelineGrant', 'A saved pipeline that a pipeline node of this run runs needs a grant its caller does not hold.');
  }
  return (
    <Alert severity="warning" data-testid="delegation-refusal-notice" sx={{ alignSelf: 'stretch', width: '100%', mb: 1 }}>
      {text}
    </Alert>
  );
}
