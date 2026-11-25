/**
 * Admin › Platform › Client applications (ADR-0059 decision 6; R3.6, issue
 * 2537): the native client registry and the Microsoft app registrations, as
 * two tabs of one item. Both used to be sections of the Configuration page,
 * which now sends the operator here.
 *
 * The server keeps the `native_clients` and `microsoft_apps` managed
 * surfaces until R4.9 (issue 2623). Each editor brings its own read-only
 * wrapper for the `client-applications` item.
 */
import { useState } from 'react';

import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminMicrosoftAppsEditor } from './AdminMicrosoftAppsEditor';
import { AdminNativeClientsEditor } from './AdminNativeClientsEditor';

type ClientApplicationsTab = 'native' | 'microsoft';

export function AdminClientApplications() {
  const [tab, setTab] = useState<ClientApplicationsTab>('native');
  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }} data-testid="admin-client-applications-page">
      <Typography variant="headingLarge" component="h1">
        {t('pages.admin.nav.clientApplications', 'Client applications')}
      </Typography>
      <Tabs
        value={tab}
        onChange={(_event, value: ClientApplicationsTab) => {
          setTab(value);
        }}
      >
        <Tab
          value="native"
          label={t('pages.admin.clientApplications.tab.native', 'Native clients')}
          sx={{ textTransform: 'none' }}
          data-testid="admin-client-applications-tab-native"
        />
        <Tab
          value="microsoft"
          label={t('pages.admin.clientApplications.tab.microsoft', 'Microsoft apps')}
          sx={{ textTransform: 'none' }}
          data-testid="admin-client-applications-tab-microsoft"
        />
      </Tabs>
      {tab === 'native' ? <AdminNativeClientsEditor /> : <AdminMicrosoftAppsEditor />}
    </DrawerPage>
  );
}
