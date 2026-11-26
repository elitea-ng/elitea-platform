/**
 * Admin › Home (ADR-0059 decision 6, "Home" and the failure states; R3.3a,
 * issue 2533): the index of the console under the six-area navigation.
 *
 *  - One card per area the capability document opens, listing the items it
 *    opens there.
 *  - The queues that wait for the caller, with their counts: App requests,
 *    Connector requests and Delegations awaiting review. A queue shows only
 *    when the document gives its item at `write`: a caller who can read a
 *    queue but not decide it has nothing waiting for them. The level comes
 *    from the document, never from a permission name.
 *
 * Each count is the total the queue's own list route already returns; Home
 * adds no count route. A count that is loading or could not be read says so
 * and is never shown as zero: failure must not read as an empty queue.
 *
 * `./AdminConsoleOutlet` decides that the index is Home: a caller with one
 * area lands on it instead, and a caller with none sees
 * `AdminConsoleNoAccess` below. With the `admin_console_areas` switch off the
 * index keeps rendering Users. The elevation card, "Request elevation" and
 * the break-glass queues are R3.3b (issue 2612).
 */
import type { ReactNode } from 'react';

import { Link as RouterLink } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import Link from '@mui/material/Link';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminSupportContact } from './AdminItemAccessDenied';
import type { AdminConsoleNavArea, ConsoleItemLevel } from './adminConsoleAreas';
import { useAdminAppRequests } from './api/adminAppRequestsApi';
import { useAdminConnectorRequestCount } from './api/adminConnectorRequestsApi';
import { useDelegationReview } from './api/adminDelegationReviewApi';

export interface AdminHomeProps {
  /** The areas the document opens, as the navigation shows them. */
  readonly areas: readonly AdminConsoleNavArea[];
  readonly itemLevel: ConsoleItemLevel;
}

interface QueueCount {
  /** `undefined` while the count is unknown. */
  readonly count: number | undefined;
  readonly failed: boolean;
}

function useAppRequestsCount(): QueueCount {
  const query = useAdminAppRequests({ limit: 1, offset: 0, status: 'pending' });
  return { count: query.data?.total, failed: query.isError };
}

function useConnectorRequestsCount(): QueueCount {
  const query = useAdminConnectorRequestCount('submitted');
  return { count: query.data, failed: query.isError };
}

/** Pending grants and grants whose callee changed after approval: both wait for a review. */
function useDelegationsCount(): QueueCount {
  const query = useDelegationReview();
  return { count: query.data === undefined ? undefined : query.data.pending + query.data.stale, failed: query.isError };
}

interface QueueDefinition {
  readonly itemId: string;
  readonly path: string;
  readonly label: () => string;
  readonly useCount: () => QueueCount;
}

const QUEUES: readonly QueueDefinition[] = [
  {
    itemId: 'app-requests',
    path: '/app-requests',
    label: () => t('pages.admin.home.queue.appRequests', 'App requests awaiting a decision'),
    useCount: useAppRequestsCount,
  },
  {
    itemId: 'connector-requests',
    path: '/connector-requests',
    label: () => t('pages.admin.home.queue.connectorRequests', 'Connector requests awaiting a decision'),
    useCount: useConnectorRequestsCount,
  },
  {
    itemId: 'delegation-review',
    path: '/delegation-review',
    label: () => t('pages.admin.home.queue.delegations', 'Delegations awaiting review'),
    useCount: useDelegationsCount,
  },
];

const cardSx = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  padding: '1rem',
  borderRadius: theme.vars.shape.radiusMd,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  background: theme.vars.palette.background.secondary,
});

function countText({ count, failed }: QueueCount): string {
  if (failed) return t('pages.admin.home.queue.failed', 'Could not be read');
  if (count === undefined) return t('pages.admin.home.queue.loading', 'Counting…');
  return String(count);
}

function QueueCard({ queue }: { readonly queue: QueueDefinition }): ReactNode {
  const state = queue.useCount();
  const label = queue.label();
  return (
    <Box component="li" sx={cardSx} data-testid={`admin-home-queue-${queue.itemId}`}>
      <Typography
        variant="headingSmall"
        component="p"
        sx={{ margin: 0 }}
        data-testid={`admin-home-queue-count-${queue.itemId}`}
        data-count={state.count ?? ''}
      >
        {countText(state)}
      </Typography>
      <Link component={RouterLink} to={queue.path} variant="bodyMedium">
        {label}
      </Link>
    </Box>
  );
}

function AreaCard({ area }: { readonly area: AdminConsoleNavArea }): ReactNode {
  const headingId = `admin-home-area-label-${area.id}`;
  return (
    <Box component="section" aria-labelledby={headingId} sx={cardSx} data-testid={`admin-home-area-${area.id}`}>
      <Typography id={headingId} variant="headingSmall" component="h2" sx={{ margin: 0 }}>
        {area.label}
      </Typography>
      <Box component="ul" sx={{ margin: 0, paddingInlineStart: '1.25rem', display: 'flex', flexDirection: 'column', gap: '0.25rem' }}>
        {area.items.map((entry) => (
          <li key={entry.id}>
            <Link component={RouterLink} to={entry.path} variant="bodyMedium" data-testid={`admin-home-item-${entry.id}`}>
              {entry.label}
            </Link>
          </li>
        ))}
      </Box>
    </Box>
  );
}

const gridSx = {
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fill, minmax(16rem, 1fr))',
  gap: '1rem',
  margin: 0,
  padding: 0,
  listStyle: 'none',
} as const;

export function AdminHome({ areas, itemLevel }: AdminHomeProps): ReactNode {
  const queues = QUEUES.filter((queue) => itemLevel(queue.itemId) === 'write');
  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '1rem' }} data-testid="admin-home">
      <Typography variant="headingLarge" component="h1" sx={{ margin: 0 }}>
        {t('pages.admin.home.title', 'Home')}
      </Typography>
      {queues.length === 0 ? null : (
        <Box component="section" aria-labelledby="admin-home-queues-label" sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }}>
          <Typography id="admin-home-queues-label" variant="headingMedium" component="h2" sx={{ margin: 0 }}>
            {t('pages.admin.home.queues', 'Waiting for you')}
          </Typography>
          <Box component="ul" sx={gridSx} data-testid="admin-home-queues">
            {queues.map((queue) => (
              <QueueCard key={queue.itemId} queue={queue} />
            ))}
          </Box>
        </Box>
      )}
      <Box sx={gridSx} data-testid="admin-home-areas">
        {areas.map((area) => (
          <AreaCard key={area.id} area={area} />
        ))}
      </Box>
    </DrawerPage>
  );
}

/**
 * The page of a caller whose document opens no area (decision 6, failure
 * states): it says so and whom to ask, the deployment's IT support line of
 * R3.2. It names no administrator, and the layout shows no navigation beside
 * it: there is nothing to navigate to.
 */
export function AdminConsoleNoAccess(): ReactNode {
  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }} data-testid="admin-no-access">
      <Typography variant="headingLarge" component="h1" sx={{ margin: 0 }}>
        {t('pages.admin.noAccess.title', 'You have no administration access')}
      </Typography>
      <Typography component="p" variant="bodyMedium" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, margin: 0 })}>
        {t(
          'pages.admin.noAccess.explanation',
          'Your administration roles open no part of this console. The server refuses its pages and changes for you.',
        )}
      </Typography>
      <AdminSupportContact
        lead={t('pages.admin.noAccess.askSupport', 'To ask for administration access, contact your IT support:')}
        fallback={t(
          'pages.admin.noAccess.askFallback',
          'To ask for administration access, contact the people who manage this deployment.',
        )}
      />
    </DrawerPage>
  );
}
