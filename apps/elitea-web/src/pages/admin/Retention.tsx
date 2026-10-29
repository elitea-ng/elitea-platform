/**
 * Admin > Data retention (ADR-0042 decision 4, issue 1495).
 *
 * Two sections:
 *
 *  - Retention policy: the platform row of each data class (bounds, maximum
 *    age, whether projects may override) and the project overrides.
 *  - Legal holds: place a hold on a project, an agent or a user, and release
 *    it. A hold suspends every deletion of the conversations it covers.
 *
 * The server authorises every call (`retention.manage`, `legal_hold.manage`);
 * nothing here is a gate. Every write is audited.
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { RetentionLegalHolds } from './RetentionLegalHolds';
import { RetentionPolicies } from './RetentionPolicies';
import { retentionFailureReason, useRetentionPolicies } from './api/adminRetentionApi';

type RetentionTab = 'policies' | 'holds';

function PoliciesSection() {
  const policies = useRetentionPolicies();
  if (policies.isError) {
    return (
      <Alert severity="warning" data-testid="admin-retention-unavailable">
        {retentionFailureReason(policies.error) ??
          t('pages.admin.retention.error.load', 'Failed to load the retention policy.')}
      </Alert>
    );
  }
  if (!policies.data) {
    return policies.isFetching ? <LinearProgress /> : null;
  }
  return <RetentionPolicies policies={policies.data} />;
}

export function AdminRetention() {
  const [tab, setTab] = useState<RetentionTab>('policies');

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Typography variant="headingLarge" component="h1">
        {t('pages.admin.retention.title', 'Data retention')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.retention.description',
          'How long the platform keeps each kind of data, and the legal holds that suspend deletion. The defaults keep conversations forever; nothing is deleted until you set a maximum age.',
        )}
      </Typography>
      <Box sx={{ borderBottom: 1, borderColor: 'divider' }}>
        <Tabs value={tab} onChange={(_event, value: RetentionTab) => setTab(value)}>
          <Tab value="policies" label={t('pages.admin.retention.tab.policies', 'Retention policy')} />
          <Tab value="holds" label={t('pages.admin.retention.tab.holds', 'Legal holds')} />
        </Tabs>
      </Box>
      {tab === 'policies' ? <PoliciesSection /> : <RetentionLegalHolds />}
    </DrawerPage>
  );
}
