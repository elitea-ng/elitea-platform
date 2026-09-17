import { useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Popover from '@mui/material/Popover';

import { contextManagementApi } from '@/entities/conversation';
import { t } from '@/shared/i18n';
import { toContextBudgetStats } from '../lib/contextStatus';
import { ContextBudgetPanel } from './ContextBudgetPanel';

/** A keyboard- and touch-accessible view of the same conversation status as the rail. */
export function ContextBudgetIndicator({ conversationId, projectId }: {
  readonly conversationId?: string | number | undefined;
  readonly projectId?: string | number | undefined;
}) {
  const [anchor, setAnchor] = useState<HTMLButtonElement | null>(null);
  const enabled = Boolean(conversationId && projectId);
  const { data } = contextManagementApi.useGetStatus(
    { conversationId: conversationId ?? '', projectId: projectId ?? '' }, { enabled },
  );
  const stats = toContextBudgetStats(data);
  if (!enabled || !stats) return null;
  const label = stats.usageAvailable
    ? t('widgets.contextBudget.indicator.measured', '{{percentage}}% context used', { percentage: stats.utilizationPercentage })
    : t('widgets.contextBudget.indicator.unknown', 'Context usage not yet measured');
  return (
    <Box sx={{ display: 'flex', justifyContent: 'flex-end', paddingBottom: 0.5 }}>
      <Button
        size="small"
        color="inherit"
        aria-label={label}
        aria-haspopup="dialog"
        aria-expanded={Boolean(anchor)}
        data-testid="context-budget-indicator"
        onClick={(event) => setAnchor(event.currentTarget)}
        startIcon={<CircularProgress aria-hidden size={16} variant="determinate" value={Math.min(stats.utilizationPercentage ?? 0, 100)} sx={{ borderRadius: 'var(--el-shape-radiusPill)', backgroundColor: 'action.hover' }} />}
      >
        {stats.usageAvailable ? t('widgets.contextBudget.indicator.short', 'Context {{percentage}}%', { percentage: stats.utilizationPercentage }) : t('widgets.contextBudget.indicator.title', 'Context')}
      </Button>
      <Popover
        open={Boolean(anchor)} anchorEl={anchor} onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: 'top', horizontal: 'right' }}
        transformOrigin={{ vertical: 'bottom', horizontal: 'right' }}
        slotProps={{ paper: { role: 'dialog', 'aria-label': t('widgets.contextBudget.indicator.details', 'Context usage'), sx: { width: 320, maxWidth: 'calc(100vw - 32px)' } } }}
      >
        <ContextBudgetPanel stats={stats} />
      </Popover>
    </Box>
  );
}
