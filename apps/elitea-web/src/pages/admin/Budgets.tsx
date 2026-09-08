/**
 * Admin › Budgets — per-project and per-member LLM spend limits (gap G4).
 *
 * ## Why this page did not exist
 *
 * The server routes have been there since issue #246, `v2.yaml` has described
 * them, and orval has generated a client for them — with ZERO non-generated
 * callers. The reference exposed eight cost-budget fields in Configuration plus
 * a read-only Usage tab; this platform had one global soft-alert percentage and
 * an authored default ceiling. Two columns the LLM gateway reads on every call,
 * `budget_period` and `nats_fail_mode`, had no writer anywhere. This page and
 * `./api/adminBudgetsApi` are that caller.
 *
 * ## Authoring is not enforcement
 *
 * A budget is STORED here and ENFORCED by the gateway's NATS counter. With no
 * `GATEWAY_NATS_URL` the gateway serves `/llm` with budget enforcement off:
 * every write on this page succeeds, every number round-trips, and no call is
 * refused. `BudgetEnforcementWarning` is the only place that says so, and it
 * warns only on a positive report from the gateway — never on silence.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate
 * (see `./adminUiConfig`). Every read and write here is authorised server-side
 * on each request against `models.admin.project_budgets.{view,edit}`, resolved
 * centrally in `internal/api/router.go`.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import Typography from '@mui/material/Typography';

import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminBudgetDialog } from './AdminBudgetDialog';
import { AdminBudgetMembersDrawer } from './AdminBudgetMembersDrawer';
import { AdminBudgetsTable } from './AdminBudgetsTable';
import { BudgetEnforcementWarning } from './BudgetEnforcementWarning';
import {
  ADMIN_BUDGETS_PAGE_SIZE,
  useAdminBudgetsPage,
  type AdminBudgetsPageState,
} from './useAdminBudgetsPage';
import type { ProjectTypeFilter } from './api/adminBudgetsApi';

/** Tab index ↔ the server's `project_type` filter. `all` sends no filter. */
const TAB_FILTERS: readonly ProjectTypeFilter[] = ['all', 'team', 'personal'];

function savedText(kind: string): string {
  return kind === 'cleared'
    ? t('pages.admin.budgets.saved.cleared', 'The budget was cleared back to the default.')
    : t('pages.admin.budgets.saved.saved', 'The budget was saved.');
}

export function AdminBudgets() {
  const state = useAdminBudgetsPage();
  const activeTab = Math.max(0, TAB_FILTERS.indexOf(state.projectType));

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
        <Typography variant="h5" sx={{ fontWeight: 600 }}>
          {t('pages.admin.budgets.title', 'Budgets')}
        </Typography>
        <SimpleSearchBar
          value={state.search}
          onChange={state.onSearchChange}
          placeholder={t('pages.admin.budgets.search', 'Search by project, id or owner')}
          data-testid="admin-budgets-search"
        />
      </Box>

      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.budgets.subtitle',
          'A limit takes effect on the next LLM call, not at the next period.',
        )}
      </Typography>

      <Tabs
        value={activeTab}
        onChange={(_event, value: number) => {
          state.onProjectTypeChange(TAB_FILTERS[value] ?? 'all');
        }}
        sx={{ minHeight: '2.5rem' }}
      >
        <Tab
          label={t('pages.admin.budgets.tab.all', 'All')}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
        <Tab
          label={`${t('pages.admin.budgets.tab.team', 'Team')} (${state.counts['team'] ?? 0})`}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
        <Tab
          label={`${t('pages.admin.budgets.tab.personal', 'Personal')} (${state.counts['personal'] ?? 0})`}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
      </Tabs>

      <BudgetEnforcementWarning hasBudgets={state.anyBudgetAuthored} />

      {state.errorMessage !== '' ? (
        <Alert severity="error" onClose={state.onDismissError}>
          {state.errorMessage}
        </Alert>
      ) : null}
      {state.savedMessage !== '' ? (
        <Alert severity="success" onClose={state.onDismissSaved}>
          {savedText(state.savedMessage)}
        </Alert>
      ) : null}

      {/* A failed read is reported rather than rendered as an empty table: the
          two look identical and mean opposite things (#130). */}
      {state.loadFailed ? (
        <Alert severity="warning" data-testid="admin-budgets-unavailable">
          {t('pages.admin.budgets.error.load', 'Failed to load project budgets.')}
        </Alert>
      ) : (
        <Box sx={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
          <AdminBudgetsTable
            rows={state.rows}
            isLoading={state.isLoading}
            onEdit={state.onEditProject}
            onClear={state.onClearProject}
            onOpenMembers={state.onOpenMembers}
          />
          <BudgetsPager state={state} />
        </Box>
      )}

      <AdminBudgetDialog
        open={state.dialogOpen}
        scope={state.dialogScope}
        subjectName={state.dialogSubject}
        initial={state.dialogInitial}
        isLoading={state.dialogLoading}
        isSaving={state.isSaving}
        serverError={state.saveError}
        onClose={state.onDialogClose}
        onSubmit={state.onDialogSubmit}
      />

      <AdminBudgetMembersDrawer
        open={state.membersOpen}
        projectName={state.membersProjectName}
        rows={state.memberRows}
        isLoading={state.membersLoading}
        loadError={state.membersError}
        onClose={state.onCloseMembers}
        onEdit={state.onEditMember}
        onClear={state.onClearMember}
      />
    </DrawerPage>
  );
}

function BudgetsPager({ state }: { readonly state: AdminBudgetsPageState }) {
  const { total, page } = state;
  const lastPage = total === 0 ? 0 : Math.ceil(total / ADMIN_BUDGETS_PAGE_SIZE) - 1;
  const firstShown = total === 0 ? 0 : page * ADMIN_BUDGETS_PAGE_SIZE + 1;
  const lastShown = Math.min((page + 1) * ADMIN_BUDGETS_PAGE_SIZE, total);

  return (
    <Box
      sx={{
        display: 'flex',
        justifyContent: 'flex-end',
        alignItems: 'center',
        gap: '0.75rem',
        paddingTop: '0.5rem',
      }}
    >
      <Typography variant="bodyMedium" color="text.secondary">
        {`${firstShown}–${lastShown} / ${total}`}
      </Typography>
      <Button
        variant="elitea"
        color="tertiary"
        size="small"
        disabled={page === 0}
        onClick={state.onPreviousPage}
        data-testid="admin-budgets-previous"
      >
        {t('pages.admin.budgets.previous', 'Previous')}
      </Button>
      <Button
        variant="elitea"
        color="tertiary"
        size="small"
        disabled={page >= lastPage}
        onClick={state.onNextPage}
        data-testid="admin-budgets-next"
      >
        {t('pages.admin.budgets.next', 'Next')}
      </Button>
    </Box>
  );
}
