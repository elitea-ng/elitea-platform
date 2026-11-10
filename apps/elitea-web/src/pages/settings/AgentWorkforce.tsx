/**
 * Settings › Agent workforce (ADR-0050 decisions 5c and 5k, issue 1617): the
 * selected project's automatic hiring and dismissal policy and the list of
 * what it did. The feature owns the page body; this file is the drawer
 * page's frame.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { WorkforcePolicyPanel } from '@/features/agent-workforce';
import { t } from '@/shared/i18n';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { useSelectedProjectStore } from '@/widgets/app-shell';

export const AgentWorkforceContent = memo(function AgentWorkforceContent() {
  const projectId = useSelectedProjectStore((s) => s.project?.id ?? '');
  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader title={t('pages.settings.agentWorkforce.title', 'Agent workforce')} />
      <Box sx={styles.content} data-testid="settings-agent-workforce">
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.settings.agentWorkforce.description',
            'Let the project hire proposed agents and dismiss poorly performing ones by rule. Both are off until an agent manager turns them on, and every automatic action is audited and can be undone or objected to.',
          )}
        </Typography>
        <WorkforcePolicyPanel projectId={String(projectId)} />
      </Box>
    </Paper>
  );
});

const styles: Record<string, SxProps<Theme>> = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
    borderRadius: 'var(--el-shape-radiusSm, 0px)',
  },
  content: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
    flex: 1,
    minHeight: 0,
    padding: '0 1.5rem 1.5rem',
    overflow: 'auto',
  },
};
