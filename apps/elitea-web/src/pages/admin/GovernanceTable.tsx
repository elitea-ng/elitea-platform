/**
 * The definition table for Admin › LLM Governance (#218).
 *
 * The `Applies to` column is the one that earns its place. A governance row's
 * effect depends entirely on its scope, and a table showing only name and type
 * would make two rows that behave completely differently look identical. It
 * renders the scope in the operator's own words — "all projects" rather than an
 * empty cell — because an empty cell reads as "not configured" when it in fact
 * means "everything".
 */
import { memo, useMemo } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import LockOutlinedIcon from '@mui/icons-material/LockOutlined';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { GridColDef, GridRenderCellParams } from '@mui/x-data-grid';
import { DataGrid } from '@mui/x-data-grid';

import { SimpleSearchBar } from '@/shared/ui/SimpleSearchBar';
import { t } from '@/shared/i18n';

import { isEditableGovernanceRow, type GovernanceRow } from './api/adminGovernanceApi';

export interface GovernanceTableProps {
  readonly rows: readonly GovernanceRow[];
  readonly search: string;
  readonly onSearchChange: (value: string) => void;
  readonly onEdit: (row: GovernanceRow) => void;
  readonly onDelete: (row: GovernanceRow) => void;
}

interface GovernanceGridRow {
  readonly id: string;
  readonly name: string;
  readonly type: string;
  readonly scope: string;
  readonly enabled: boolean;
  /** False for a row another surface owns — its actions are withheld. */
  readonly editable: boolean;
  readonly source: GovernanceRow;
}

function list(value: unknown): string[] {
  return Array.isArray(value) ? value.map((entry) => String(entry)) : [];
}

/**
 * An egress allowlist has no scope at all: it is global by construction, and the
 * server refuses a scoped one. Falling through to "all projects" would be true
 * but useless — the column would repeat the same three words for every egress
 * row, while the thing the operator wants to see, the destinations, is nowhere
 * on the list.
 */
function describeEgress(row: GovernanceRow): string {
  const egress = (row.data as Record<string, unknown>).egress;
  const group = typeof egress === 'object' && egress !== null ? (egress as Record<string, unknown>) : {};
  const destinations = list(group.allowlist);
  return destinations.length > 0
    ? destinations.join(', ')
    : t('pages.admin.governance.scope.noDestinations', 'no destinations');
}

/**
 * Renders a row's scope as a sentence.
 *
 * `model_config` is described differently on purpose: for that type the
 * provider and model lists are the ALLOWLIST, not the selector, so describing
 * them as "applies to" would state the opposite of what the row does.
 */
export function describeScope(row: GovernanceRow): string {
  const scope = (row.data as Record<string, unknown>).scope;
  const record = typeof scope === 'object' && scope !== null ? (scope as Record<string, unknown>) : {};
  const projects = list(record.project_ids);
  const providers = list(record.providers);
  const models = list(record.models);

  // An egress allowlist has no scope at all: it is global by construction, and
  // the server refuses a scoped one. Falling through to "all projects" would be
  // true but useless — the column would repeat the same three words for every
  // egress row while the thing the operator wants to see, the destinations, is
  // nowhere on the list.
  if (row.type === 'egress_allowlist') return describeEgress(row);

  // Composed rather than interpolated. A `{{ids}}` placeholder in the fallback
  // is a trap here: once the key is backfilled into en.json the BUNDLE value
  // wins over the call-site fallback, and i18next then owns an interpolation
  // this function resolves by hand — so the braces render literally. Joining a
  // translated noun to the id list keeps both halves translatable and leaves
  // no placeholder for the bundle to disagree about.
  const projectPart =
    projects.length > 0
      ? `${t('pages.admin.governance.scope.projects', 'projects')} ${projects.join(', ')}`
      : t('pages.admin.governance.scope.allProjects', 'all projects');

  if (row.type === 'model_config') {
    const permitted = [
      providers.length > 0
        ? providers.join(', ')
        : t('pages.admin.governance.scope.allProviders', 'every provider'),
      models.length > 0 ? models.join(', ') : t('pages.admin.governance.scope.allModels', 'every model'),
    ].join(' · ');
    return `${projectPart} → ${permitted}`;
  }

  const parts = [projectPart];
  if (providers.length > 0) parts.push(providers.join(', '));
  if (models.length > 0) parts.push(models.join(', '));
  return parts.join(' · ');
}

export const GovernanceTable = memo(function GovernanceTable({
  rows,
  search,
  onSearchChange,
  onEdit,
  onDelete,
}: GovernanceTableProps) {
  const gridRows: GovernanceGridRow[] = useMemo(
    () =>
      rows.map((row) => ({
        id: row.id,
        name: row.name,
        type: row.type,
        scope: describeScope(row),
        enabled: row.enabled,
        editable: isEditableGovernanceRow(row),
        source: row,
      })),
    [rows],
  );

  const columns: GridColDef<GovernanceGridRow>[] = useMemo(
    () => [
      {
        field: 'name',
        headerName: t('pages.admin.governance.column.name', 'Name'),
        flex: 1,
        minWidth: 140,
      },
      {
        field: 'type',
        headerName: t('pages.admin.governance.column.type', 'Type'),
        flex: 1,
        minWidth: 140,
        renderCell: (params: GridRenderCellParams<GovernanceGridRow>) => (
          <Chip label={params.row.type} size="small" variant="outlined" />
        ),
      },
      {
        field: 'scope',
        headerName: t('pages.admin.governance.column.scope', 'Applies to'),
        flex: 2,
        minWidth: 220,
        renderCell: (params: GridRenderCellParams<GovernanceGridRow>) => (
          <Typography variant="bodySmall" color="text.secondary" noWrap title={params.row.scope}>
            {params.row.scope}
          </Typography>
        ),
      },
      {
        field: 'enabled',
        headerName: t('pages.admin.governance.column.enabled', 'Enabled'),
        width: 110,
        renderCell: (params: GridRenderCellParams<GovernanceGridRow>) =>
          params.row.enabled ? (
            <Chip label={t('common.yes', 'Yes')} size="small" color="success" variant="outlined" />
          ) : (
            <Chip label={t('common.no', 'No')} size="small" variant="outlined" />
          ),
      },
      {
        field: 'actions',
        // Named, not blank. An empty column header is an axe `empty-table-header`
        // violation (caught by checkA11y in journey J37b), and the sibling admin
        // tables label theirs the same way. `common.actions` already exists.
        headerName: t('common.actions', 'Actions'),
        width: 96,
        sortable: false,
        filterable: false,
        renderCell: (params: GridRenderCellParams<GovernanceGridRow>) => {
          // A row another surface owns is shown and NOT offered an editor. The
          // reason is on the row rather than in a runbook: this page's editor
          // has no field for that row's keys, so saving it here would drop
          // them. Hiding the row instead would leave an operator hunting for a
          // definition that does exist.
          if (!params.row.editable) {
            const reason = t(
              'pages.admin.governance.readOnlyRow',
              'This entry is authored on the budget-alerts surface. Editing it here would discard its settings.',
            );
            // `titleAccess` renders an SVG <title>, so the reason has an
            // accessible name of its own. A Tooltip alone is hover-only: a
            // keyboard or screen-reader user would meet an unexplained icon
            // where the other rows offer two buttons.
            return (
              <Tooltip title={reason}>
                <LockOutlinedIcon
                  fontSize="small"
                  color="disabled"
                  titleAccess={reason}
                  data-testid="governance-row-readonly"
                />
              </Tooltip>
            );
          }
          return (
            <Box>
              <Tooltip title={t('common.edit', 'Edit')}>
                <IconButton
                  size="small"
                  onClick={() => onEdit(params.row.source)}
                  aria-label={t('pages.admin.governance.action.edit', 'Edit entry')}
                >
                  <EditOutlinedIcon fontSize="small" />
                </IconButton>
              </Tooltip>
              <Tooltip title={t('common.delete', 'Delete')}>
                <IconButton
                  size="small"
                  onClick={() => onDelete(params.row.source)}
                  aria-label={t('pages.admin.governance.action.delete', 'Delete entry')}
                >
                  <DeleteOutlinedIcon fontSize="small" />
                </IconButton>
              </Tooltip>
            </Box>
          );
        },
      },
    ],
    [onDelete, onEdit],
  );

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <SimpleSearchBar
        value={search}
        onChange={onSearchChange}
        placeholder={t('pages.admin.governance.search', 'Search by name or type')}
        data-testid="governance-search"
      />
      <DataGrid
        rows={gridRows}
        columns={columns}
        density="compact"
        disableRowSelectionOnClick
        autoHeight
        hideFooterSelectedRowCount
        localeText={{
          noRowsLabel: t(
            'pages.admin.governance.empty',
            'No governance definitions. Every project uses its own budget, every model is permitted, and no rate limit applies.',
          ),
        }}
        data-testid="governance-table"
      />
    </Box>
  );
});
