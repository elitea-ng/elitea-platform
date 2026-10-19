/** Where a thread runs: Local / In cloud / Returning. */
import Chip from '@mui/material/Chip';

import type { HandoffExecutor } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

export interface ExecutorBadgeProps {
  executor: HandoffExecutor;
  /** `small` for a row in the sidebar. */
  size?: 'small' | 'medium';
}

function executorLabel(executor: HandoffExecutor): string {
  switch (executor) {
    case 'local':
      return t('workspace.handoff.badge.local', 'Local');
    case 'cloud':
      return t('workspace.handoff.badge.cloud', 'In cloud');
    case 'returning':
      return t('workspace.handoff.badge.returning', 'Returning');
  }
}

export function ExecutorBadge({ executor, size = 'small' }: ExecutorBadgeProps): React.JSX.Element {
  return (
    <Chip
      size={size}
      data-testid="executor-badge"
      data-executor={executor}
      color={executor === 'local' ? 'default' : executor === 'cloud' ? 'info' : 'warning'}
      variant={executor === 'local' ? 'outlined' : 'filled'}
      label={executorLabel(executor)}
    />
  );
}
