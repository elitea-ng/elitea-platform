/**
 * The notices of a compaction the next message starts with: asked for with
 * `/compact`, or armed because the thread was idle (`useIdleCompaction`),
 * which the person can cancel.
 */
import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';

import type { SessionCommands } from './useSessionCommands';

export function CompactNotices({ commands }: { commands: SessionCommands }): React.JSX.Element | null {
  if (commands.notice === 'compactIdle') {
    return (
      <Alert
        severity="info"
        data-testid="workspace-compact-idle"
        action={
          <Button color="inherit" size="small" onClick={commands.cancelCompact}>
            {t('workspace.compactIdle.cancel', 'Don’t summarize')}
          </Button>
        }
      >
        {t('workspace.compactIdle', 'This thread was idle, so your next message first summarizes the conversation. The agent then continues with more room and costs less.')}
      </Alert>
    );
  }
  if (commands.notice === 'compactNext') {
    return (
      <Alert severity="info" onClose={commands.closeNotice} data-testid="workspace-compact-next">
        {t('workspace.compactNext', 'Your next message first summarizes this conversation, so the agent continues with more room.')}
      </Alert>
    );
  }
  return null;
}
