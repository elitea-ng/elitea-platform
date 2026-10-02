import { useState } from 'react';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Popover from '@mui/material/Popover';
import Typography from '@mui/material/Typography';

import { contextManagementApi } from '@/entities/conversation';
import { t } from '@/shared/i18n';
import { toContextBudgetStats, type ContextBudgetStats } from '../lib/contextStatus';
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
  const { compacting, label, caption, announcement } = indicatorPresentation(stats);
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
        startIcon={<CircularProgress aria-hidden size={16} variant={compacting ? 'indeterminate' : 'determinate'} value={Math.min(stats.utilizationPercentage ?? 0, 100)} sx={{ borderRadius: 'var(--el-shape-radiusPill)', backgroundColor: 'action.hover' }} />}
      >
        {caption}
      </Button>
      <Typography component="output" sx={{ position: 'absolute', width: 1, height: 1, overflow: 'hidden', clipPath: 'inset(50%)' }}>
        {announcement}
      </Typography>
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

function indicatorPresentation(stats: ContextBudgetStats) {
  const compacting = stats.runtime?.phase === 'compacting' && stats.runtime.active;
  if (compacting) {
    const text = t('widgets.contextBudget.compacting', 'Compacting context…');
    return { compacting, label: text, caption: text, announcement: text };
  }
  return {
    compacting,
    label: stats.usageAvailable
      ? t('widgets.contextBudget.indicator.measured', '{{percentage}}% context used', { percentage: stats.utilizationPercentage })
      : t('widgets.contextBudget.indicator.unknown', 'Context usage not yet measured'),
    caption: stats.usageAvailable
      ? t('widgets.contextBudget.indicator.short', 'Context {{percentage}}%', { percentage: stats.utilizationPercentage })
      : t('widgets.contextBudget.indicator.title', 'Context'),
    announcement: stats.runtime?.phase === 'compacted' ? t('widgets.contextBudget.compactedShort', 'Context compacted') : '',
  };
}
