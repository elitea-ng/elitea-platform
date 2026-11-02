/**
 * The admin nav's foot: the same account menu as the user app
 * (`widgets/account-menu`'s frame), with the admin console's own rows.
 *
 * Back to Elitea comes first, the way out to the app this console serves.
 * Then the version line (this build, and the server's) and Sign out.
 *
 * There is no theme control here. The theme is a user preference that is set
 * in the app (Settings › Personalization, or the account menu's Appearance).
 * The console reads the same stored choice (`el-mode` on this origin, and the
 * server copy through `ThemePreferenceSync`, `entries/admin/AdminApp.tsx`),
 * so it always matches the app.
 *
 * Sign out is `performLogout()`, not the reference's raw
 * `location.href = "/auth/logout"`: the same handoff plus the `el.`
 * storage-namespace sweep and the `target_to` that lands the browser on the
 * login screen (`shared/api/auth/logout.ts`).
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import ArrowBackIcon from '@mui/icons-material/ArrowBack';
import Box from '@mui/material/Box';

import { performLogout } from '@/shared/api/auth';
import { appVersion } from '@/shared/config/appVersion';
import { t } from '@/shared/i18n';
import { MenuRow, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';
import { AccountMenuFrame, useServerVersion, versionLine } from '@/widgets/account-menu';

import { adminUiUserEmail, adminUiUserName } from './adminUiConfig';

/** The user app's base: the SPA image's default `vite_base_uri`. */
const USER_APP_PATH = '/app/';

export interface AdminNavFooterProps {
  readonly collapsed: boolean;
}

export function AdminNavFooter({ collapsed }: AdminNavFooterProps): ReactNode {
  const [opened, setOpened] = useState(false);
  const serverVersion = useServerVersion(opened);
  const version = versionLine(appVersion(), serverVersion, {
    client: t('widgets.accountMenu.version', 'Version'),
    server: t('widgets.accountMenu.server', 'Server'),
  });

  return (
    <Box sx={{ padding: '0.5rem 0.75rem 1rem' }}>
      <AccountMenuFrame
        identity={{ name: adminUiUserName(), email: adminUiUserEmail() }}
        collapsed={collapsed}
        version={version}
        onOpen={() => setOpened(true)}
        onSignOut={performLogout}
        testIds={{ button: 'admin-nav-user-button', signOut: 'admin-nav-logout' }}
      >
        {(close) => [
          <MenuRow key="back" component="a" href={USER_APP_PATH} data-testid="admin-nav-back-to-app" onClick={close}>
            <MenuRowIcon>
              <ArrowBackIcon />
            </MenuRowIcon>
            <MenuRowText>{t('pages.admin.nav.backToApp', 'Back to Elitea')}</MenuRowText>
          </MenuRow>,
        ]}
      </AccountMenuFrame>
    </Box>
  );
}
