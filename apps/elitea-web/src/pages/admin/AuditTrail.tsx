/**
 * Admin › Audit Trail — the deployment-wide record of what happened
 * (unit A14, issue #200).
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/pages/AuditTrailPage/`.
 * Rewritten against this app's stack: Redux Toolkit → TanStack Query
 * (`./api/adminAuditApi`), nivo → a CSS-grid heatmap (`./AuditHeatmap`), MUI 7 →
 * 9. State and query wiring live in `./useAdminAuditTrailPage`.
 *
 * ## Zero mutations, by nature
 *
 * Four reads and nothing else. An audit trail that the product could edit would
 * not be one, so there is no write path here to render, gate, or leave
 * unavailable — and correspondingly no control on this page that fails to reach
 * a server.
 *
 * ## What was actually missing
 *
 * The four endpoints this page needs were not all there. `audit` and
 * `audit_heatmap` had no route in elitea-main at all; `audit_traces` and
 * `audit_trace_heatmap` were stubs returning empty arrays with the request
 * discarded — and `audit_traces` returned `items` where the client reads `rows`,
 * so even its emptiness would not have rendered. All four are implemented for
 * real in `services/elitea-main/internal/api/v2/eliteacore/audit.go` against
 * `centry.audit_events`, per the decision on #200 to build rather than to ship
 * dead controls.
 *
 * ## Authorisation
 *
 * `window.admin_ui_config.permissions` is presentation state and never a gate —
 * see `./adminUiConfig`. All four reads are gated SERVER-side on
 * `models.admin.audit_trail.view`, resolved from the database per request. This
 * page's own permission check only decides whether rendering it is worth doing;
 * a caller without the permission gets 403s either way.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AuditHeatmap } from './AuditHeatmap';
import { AuditSpanTable } from './AuditSpanTable';
import { AuditTraceTable } from './AuditTraceTable';
import { AuditTrailFilters } from './AuditTrailFilters';
import { AUDIT_PAGE_SIZE_OPTIONS, useAdminAuditTrailPage } from './useAdminAuditTrailPage';


/** How much of a trace id fits in a filter chip before it stops being a label. */
const TRACE_CHIP_LENGTH = 12;

export function AdminAuditTrail() {
  const state = useAdminAuditTrailPage();

  const { total, page, pageSize } = state;
  const lastPage = total === 0 ? 0 : Math.ceil(total / pageSize) - 1;
  const firstShown = total === 0 ? 0 : page * pageSize + 1;
  const lastShown = Math.min((page + 1) * pageSize, total);

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
          {t('pages.admin.audit.title', 'Audit Trail')}
        </Typography>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.75rem', flexWrap: 'wrap' }}>
          <SimpleSearchBar
            value={state.search}
            onChange={state.onSearchChange}
            placeholder={t('pages.admin.audit.search', 'Search actions, tools, users')}
            data-testid="admin-audit-search"
            sx={{ width: '18rem' }}
          />
          {/*
            Active drill-downs, each removable. They are shown because they
            change what the tables below contain: a trace filter applied
            invisibly is a table that looks like it lost most of its rows.
          */}
          {state.traceFilter ? (
            <Chip
              size="small"
              label={`${t('pages.admin.audit.chip.trace', 'trace')}: ${state.traceFilter.slice(0, TRACE_CHIP_LENGTH)}…`}
              onDelete={state.onClearTrace}
            />
          ) : null}
          {state.cellFilter ? (
            <Chip
              size="small"
              color="primary"
              label={`${state.cellFilter.timeLabel} · ${state.cellFilter.bandLabel}`}
              onDelete={state.onClearCell}
            />
          ) : null}
        </Box>
      </Box>

      <Tabs value={state.tab} onChange={state.onTabChange} sx={{ minHeight: '2.5rem' }}>
        <Tab
          value="user"
          label={t('pages.admin.audit.tab.user', 'User')}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
        <Tab
          value="system"
          label={t('pages.admin.audit.tab.system', 'System')}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
      </Tabs>

      <AuditTrailFilters
        filters={state.draftFilters}
        tab={state.tab}
        activePreset={state.activePreset}
        onChange={state.onDraftChange}
        onPresetSelect={state.onPresetSelect}
        onApply={state.onApply}
        onRefresh={state.onRefresh}
      />

      <AuditHeatmap
        heatmap={state.heatmap}
        isFetching={state.isHeatmapFetching}
        viewMode={state.viewMode}
        onCellSelect={state.onCellSelect}
      />

      <Tabs value={state.viewMode} onChange={state.onViewModeChange} sx={{ minHeight: '2.5rem' }}>
        <Tab
          value="traces"
          label={t('pages.admin.audit.view.traces', 'Traces')}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
        <Tab
          value="spans"
          label={t('pages.admin.audit.view.spans', 'Spans')}
          sx={{ textTransform: 'none', minHeight: '2.5rem' }}
        />
      </Tabs>

      {state.isError ? (
        <Alert severity="error">
          {t('pages.admin.audit.error.load', 'Failed to load the audit trail.')}
        </Alert>
      ) : (
        <Box sx={{ display: 'flex', flexDirection: 'column', flex: 1, minHeight: 0 }}>
          <Box sx={{ height: '0.25rem' }}>{state.isFetching ? <LinearProgress /> : null}</Box>

          {state.viewMode === 'traces' ? (
            <AuditTraceTable
              rows={state.traceRows}
              sortField={state.sortField}
              sortDirection={state.sortDirection}
              onSort={state.onSort}
              onTraceSelect={state.onTraceSelect}
            />
          ) : (
            <AuditSpanTable
              rows={state.spanRows}
              sortField={state.sortField}
              sortDirection={state.sortDirection}
              onSort={state.onSort}
              onTraceSelect={state.onTraceSelect}
            />
          )}

          <Box
            sx={{
              display: 'flex',
              justifyContent: 'flex-end',
              alignItems: 'center',
              gap: '0.75rem',
              paddingTop: '0.5rem',
            }}
          >
            <TextField
              select
              size="small"
              label={t('pages.admin.audit.pagination.pageSize', 'Rows')}
              value={String(pageSize)}
              onChange={(event) => state.onPageSizeChange(Number(event.target.value))}
              sx={{ width: '6rem' }}
            >
              {AUDIT_PAGE_SIZE_OPTIONS.map((option) => (
                <MenuItem key={option} value={String(option)}>
                  {option}
                </MenuItem>
              ))}
            </TextField>
            <Typography variant="bodyMedium" color="text.secondary">
              {`${firstShown}–${lastShown} / ${total}`}
            </Typography>
            <Button
            variant="elitea" color="tertiary" size="small" disabled={page === 0} onClick={() => state.onPageChange(page - 1)}>
              {t('pages.admin.audit.pagination.previous', 'Previous')}
            </Button>
            <Button
            variant="elitea" color="tertiary"
              size="small"
              disabled={page >= lastPage}
              onClick={() => state.onPageChange(page + 1)}
            >
              {t('pages.admin.audit.pagination.next', 'Next')}
            </Button>
          </Box>
        </Box>
      )}
    </DrawerPage>
  );
}
