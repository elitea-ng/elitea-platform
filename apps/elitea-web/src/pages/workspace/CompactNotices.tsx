/**
 * The notices of a compaction: the next message starts with one (asked for
 * with `/compact`), or the host summarized the thread in the background
 * while it was idle (`useIdleCompacted`), which the person can dismiss.
 */
import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';

import type { SessionCommands } from './useSessionCommands';

export function CompactNotices({ commands }: { commands: SessionCommands }): React.JSX.Element | null {
  if (commands.notice === 'compactNext') {
    return (
      <Alert severity="info" onClose={commands.closeNotice} data-testid="workspace-compact-next">
        {t('workspace.compactNext', 'Your next message first summarizes this conversation, so the agent continues with more room.')}
      </Alert>
    );
  }
  return null;
}

/** One quiet line above the composer: the thread was summarized in the background while idle. */
export function IdleCompactedNotice({ shown, onClose }: { shown: boolean; onClose: () => void }): React.JSX.Element | null {
  if (!shown) return null;
  return (
    <Alert severity="info" variant="outlined" onClose={onClose} data-testid="workspace-compacted-idle">
      {t('workspace.compactedIdle', 'Summarized while you were away to save cost — the newest messages are kept as they were.')}
    </Alert>
  );
}
