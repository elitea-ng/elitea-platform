/**
 * `/settings/usage` — this project's current-period spend against its budget.
 *
 * The tab is gated on `cost_budgets_enabled` in `settings-layout.tsx`, but the
 * ROUTE has to exist unconditionally: a file route is registered at build time,
 * and a bookmarked or shared URL must resolve rather than 404 while the flag
 * query is still in flight. The page itself reports honestly when there is no
 * data behind it.
 */
import { createFileRoute } from '@tanstack/react-router';
import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import type { SxProps, Theme } from '@mui/material/styles';

import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { RouteError, RoutePending } from '@/routes/-ui/RouteStatus';
import { t } from '@/shared/i18n';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import Usage from '@/pages/settings/Usage';

export const Route = createFileRoute('/_shell/settings/usage')({
  pendingComponent: RoutePending,
  errorComponent: RouteError,
  component: SettingsUsagePage,
});

function SettingsUsagePage() {
  const projectId = useSelectedProjectStore((s) => s.project?.id ?? '');
  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader title={t('routes.settings.usage.title', 'Usage')} showBorder />
      <Box sx={styles.content}>
        <Usage projectId={projectId} />
      </Box>
    </Paper>
  );
}

const styles: Record<string, SxProps<Theme>> = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
    borderRadius: 'var(--el-shape-radiusSm, 0px)',
  },
  content: { flex: 1, minHeight: 0, overflowY: 'auto' },
};
