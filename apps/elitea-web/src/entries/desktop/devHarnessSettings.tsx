/**
 * DEV ONLY — Settings on the desktop harness (#3078): the drawer's desktop rows, the `/settings/$tab` catch-all's entry
 * for each (`DesktopSettingsEntry`) and the real unknown-tab redirect (`SettingsRedirect`), under the route ids the app
 * has (`/_shell/settings/$tab`), so a click on a row goes through the same redirect check as in the app.
 */
import Box from '@mui/material/Box';
import { createRoute, Outlet, useNavigate, useParams, type AnyRoute } from '@tanstack/react-router';

import { DesktopSettingsEntry } from '@/pages/workspace/desktopEntry';
import { DESKTOP_SETTINGS_TABS } from '@/shared/ui/settings/desktopSettingsTabs';
import { SettingsDrawer } from '@/shared/ui/settings/SettingsDrawer';
import { SettingsRedirect } from '@/shared/ui/settings/SettingsRedirect';

const SECTIONS = [{ section: 'PERSONAL', tabs: DESKTOP_SETTINGS_TABS.map(({ id, label }) => ({ id, label })) }];

function HarnessSettings(): React.JSX.Element {
  const navigate = useNavigate();
  return (
    <Box sx={{ display: 'flex', height: '100%' }}>
      <Box sx={{ width: '13.75rem', flexShrink: 0 }}>
        <SettingsDrawer sections={SECTIONS} variant="native" onItemClick={(tab) => void navigate({ to: '/settings/$tab', params: { tab } })} />
      </Box>
      <Box component="section" sx={{ flex: 1, overflow: 'auto' }}>
        <Outlet />
      </Box>
      <SettingsRedirect />
    </Box>
  );
}

function HarnessSettingsTab(): React.JSX.Element | null {
  const { tab } = useParams({ strict: false });
  return <DesktopSettingsEntry tab={tab ?? ''} />;
}

/** `/settings` and `/settings/$tab` under a pathless `_shell`, as the app's route tree names them. */
export function harnessSettingsRoutes(rootRoute: AnyRoute): AnyRoute {
  const shell = createRoute({ getParentRoute: () => rootRoute, id: '_shell', component: Outlet });
  const settings = createRoute({ getParentRoute: () => shell, path: 'settings', component: HarnessSettings });
  return shell.addChildren([settings.addChildren([createRoute({ getParentRoute: () => settings, path: '$tab', component: HarnessSettingsTab })])]);
}
