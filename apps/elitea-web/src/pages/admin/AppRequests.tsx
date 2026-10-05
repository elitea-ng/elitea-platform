/**
 * Admin › App Requests — unit A14, issue #200.
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/pages/AppRequestsPage/`.
 * Rewritten against this app's stack: Redux Toolkit → TanStack Query
 * (`./api/adminAppRequestsApi`), axios → `eliteaFetch`, react-router → TanStack
 * Router (`./router`), MUI 7 → 9. State and handlers live in
 * `./useAdminAppRequestsPage`.
 *
 * ## What a row on this page is
 *
 * A user asked for access to something the application catalogue offers but
 * their project is not configured for — the "Request Access" button on a
 * catalogue card. `centry.moderation_state` is a generic access-request table,
 * NOT the prompt/application publish moderation, which is a different mechanism
 * on application versions and never touches these rows.
 *
 * ## What approving does, stated because it decides what this page may claim
 *
 * It records the operator's answer and TELLS the requester: the decision writes
 * a `centry.notifications` row in the same transaction as the status change.
 * It does not itself grant a capability — nothing in this stack or in pylon
 * reads an approved row and unlocks anything, and a catalogue card's real gate
 * is whether a toolkit schema exists for it. So the two buttons say "approve"
 * and "reject", the page says what that means, and neither is dressed as
 * provisioning. The gap between "approved" and "provisioned" is a product
 * question, not something a port should paper over with a control that implies
 * an outcome it cannot produce.
 *
 * ## What this page replaces
 *
 * All four endpoints behind this surface were unusable before this unit. The
 * queue read was a stub returning a fixed empty page, the decision had no route
 * at all, and the product-side pair that fills the queue answered
 * `{"status":"approved"}` to every caller while its POST created nothing — so
 * the catalogue's Request Access button wrote nowhere and the gate it feeds
 * always said yes. See
 * `services/elitea-main/internal/api/v2/moderation/requests.go`.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate —
 * see `./adminUiConfig`. The read is gated server-side on `admin.moderation` and
 * the decision on `admin.moderation.edit`, resolved from `auth_core__user_role`
 * per request.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AppRequestsTable } from './AppRequestsTable';
import { RejectAppRequestDialog } from './RejectAppRequestDialog';
import {
  ADMIN_APP_REQUESTS_PAGE_SIZE,
  useAdminAppRequestsPage,
  type AppRequestStatusFilter,
} from './useAdminAppRequestsPage';


const TAB_SX = { textTransform: 'none', minHeight: '2.5rem' } as const;

/** The status filter's four positions, in the order they are drawn. */
const STATUS_TABS: readonly AppRequestStatusFilter[] = ['pending', 'approved', 'rejected', 'all'];

function statusTabLabel(filter: AppRequestStatusFilter): string {
  switch (filter) {
    case 'pending':
      return t('pages.admin.appRequests.tab.pending', 'Pending');
    case 'approved':
      return t('pages.admin.appRequests.tab.approved', 'Approved');
    case 'rejected':
      return t('pages.admin.appRequests.tab.rejected', 'Rejected');
    case 'all':
      return t('pages.admin.appRequests.tab.all', 'All');
  }
}

function savedText(status: string): string {
  return status === 'approved'
    ? t('pages.admin.appRequests.saved.approved', 'Request approved. The user has been notified.')
    : t('pages.admin.appRequests.saved.rejected', 'Request rejected. The user has been notified.');
}

export function AdminAppRequests() {
  const state = useAdminAppRequestsPage();

  const { total, page } = state;
  const lastPage = total === 0 ? 0 : Math.ceil(total / ADMIN_APP_REQUESTS_PAGE_SIZE) - 1;
  const firstShown = total === 0 ? 0 : page * ADMIN_APP_REQUESTS_PAGE_SIZE + 1;
  const lastShown = Math.min((page + 1) * ADMIN_APP_REQUESTS_PAGE_SIZE, total);

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Box
        sx={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
          gap: '1rem',
          flexWrap: 'wrap',
        }}
      >
        <Typography variant="headingLarge" component="h1">
          {t('pages.admin.appRequests.title', 'App Requests')}
        </Typography>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.75rem', flexWrap: 'wrap' }}>
          {/*
            Server-side, via the `issue_type` param the queue endpoint has
            always accepted (`requests.go`'s `queueFilters`) — no client sent
            it before this control. Options come from a bounded, unfiltered
            sample (`useAdminAppRequestsPage`'s header explains why), not from
            a fixed enum: the queue is issue-type-agnostic by design, so there
            is no closed set of labels to hardcode.
          */}
          <TextField
            select
            size="small"
            label={t('pages.admin.appRequests.filter.issueType', 'Issue Type')}
            value={state.issueTypeFilter}
            onChange={(event) => state.onIssueTypeFilterChange(event.target.value)}
            sx={{ minWidth: '11rem' }}
            data-testid="admin-app-requests-issue-type-filter"
          >
            <MenuItem value="">
              {t('pages.admin.appRequests.filter.allIssueTypes', 'All')}
            </MenuItem>
            {state.issueTypeOptions.map((issueType) => (
              <MenuItem key={issueType} value={issueType}>
                {issueType}
              </MenuItem>
            ))}
          </TextField>
          <SimpleSearchBar
            value={state.search}
            onChange={state.onSearchChange}
            placeholder={t('pages.admin.appRequests.search', 'Search by requesting user')}
            data-testid="admin-app-requests-search"
          />
        </Box>
      </Box>

      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.appRequests.description',
          'Access requests raised from the application catalogue. Approving or rejecting one records your answer and notifies the person who asked; it does not itself configure the application.',
        )}
      </Typography>

      <Tabs
        value={state.statusFilter}
        onChange={(_event, next: AppRequestStatusFilter) => state.onStatusFilterChange(next)}
        sx={{ minHeight: '2.5rem' }}
      >
        {STATUS_TABS.map((filter) => (
          <Tab key={filter} value={filter} sx={TAB_SX} label={statusTabLabel(filter)} />
        ))}
      </Tabs>

      {state.isFetching ? <LinearProgress /> : null}

      {state.errorMessage !== '' ? (
        <Alert
          severity="error"
          onClose={state.onDismissError}
          data-testid="admin-app-requests-error"
        >
          {state.errorMessage}
        </Alert>
      ) : null}
      {state.savedMessage !== '' ? (
        <Alert
          severity="success"
          onClose={state.onDismissSaved}
          data-testid="admin-app-requests-saved"
        >
          {savedText(state.savedMessage)}
        </Alert>
      ) : null}

      {state.isError ? (
        <Alert severity="warning" data-testid="admin-app-requests-unavailable">
          {state.unavailableReason ??
            t('pages.admin.appRequests.error.load', 'Failed to load app requests.')}
        </Alert>
      ) : (
        <Box sx={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
          <AppRequestsTable
            requests={state.rows}
            isLoading={state.isFetching}
            sortField={state.sortField}
            sortDirection={state.sortDirection}
            onSort={state.onSort}
            onApprove={state.onApprove}
            onOpenReject={state.onOpenReject}
            pendingIds={state.pendingIds}
          />

          <Box
            sx={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'flex-end',
              gap: '0.75rem',
              paddingTop: '0.5rem',
            }}
          >
            <Typography variant="bodySmall" color="text.secondary">
              {`${firstShown}–${lastShown} / ${total}`}
            </Typography>
            <Button
            variant="elitea" color="tertiary" size="small" disabled={page === 0} onClick={state.onPreviousPage}>
              {t('pages.admin.appRequests.previous', 'Previous')}
            </Button>
            <Button
            variant="elitea" color="tertiary" size="small" disabled={page >= lastPage} onClick={state.onNextPage}>
              {t('pages.admin.appRequests.next', 'Next')}
            </Button>
          </Box>
        </Box>
      )}

      <RejectAppRequestDialog
        request={state.rejecting}
        onCancel={state.onCancelReject}
        onConfirm={state.onConfirmReject}
      />
    </DrawerPage>
  );
}
