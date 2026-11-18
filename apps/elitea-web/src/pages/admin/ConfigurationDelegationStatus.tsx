/**
 * What the Delegation section says next to its fields (ADR-0044 Amendment 1,
 * A1.3; delegation epic 2156 story S6): how many delegations await review, with
 * a way to the queue, and what `report` is for. The count is the server's own
 * (`status` of the section's values), read where the administrator changes the
 * mode, so the decision to use the window is made with the number in front of them.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Link from '@mui/material/Link';

import { t } from '@/shared/i18n';

import type { AdminConfigStatus } from './api/adminConfigurationApi';

const DEFAULT_WINDOW_DAYS = 14;

export function ConfigurationDelegationStatus({ status }: { readonly status: AdminConfigStatus | undefined }): ReactNode {
  if (status === undefined) return null;
  const waiting = (status.pending ?? 0) + (status.stale ?? 0);
  const days = status.report_window_days ?? DEFAULT_WINDOW_DAYS;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }} data-testid="admin-delegation-status">
      <Alert severity={waiting === 0 ? 'success' : 'warning'} data-testid="admin-delegation-pending">
        {waiting === 0
          ? t('pages.admin.configuration.delegation.none', 'No delegation awaits review.')
          : t(
              'pages.admin.configuration.delegation.pending',
              '{{count}} delegations await review ({{legacy}} from the migration of existing compositions). Nothing was approved by the migration.',
              { count: waiting, legacy: status.legacy ?? 0 },
            )}{' '}
        <Link href="/admin/app/delegation-review" underline="hover" data-testid="admin-delegation-review-link">
          {t('pages.admin.configuration.delegation.open', 'Open the queue')}
        </Link>
      </Alert>
      <Alert severity="info" data-testid="admin-delegation-window">
        {t(
          'pages.admin.configuration.delegation.window',
          'To keep delegations that need a grant running while the owners review them, set the mode to report for a window of about {{days}} days, then return to enforce. Report never relaxes the run principal bound, and a governed run always enforces.',
          { days },
        )}
      </Alert>
    </Box>
  );
}
