/**
 * Settings: Profile route.
 *
 * Same shape as every other settings route here — read `preferences.tsx` for
 * the pattern. The page component itself is `pages/settings/Profile.tsx`.
 */
import { createFileRoute } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import type { SxProps, Theme } from '@mui/material/styles';

import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { t } from '@/shared/i18n';
import Profile from '@/pages/settings/Profile';

export const Route = createFileRoute('/_shell/settings/profile')({
  component: SettingsProfilePage,
});

function SettingsProfilePage() {
  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader title={t('routes.settings.profile.title', 'Profile')} showBorder />
      <Box sx={styles.content}>
        <Profile />
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
  content: {
    flex: 1,
    minHeight: 0,
  },
};
