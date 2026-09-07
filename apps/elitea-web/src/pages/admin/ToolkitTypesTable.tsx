/**
 * The toolkit TYPE listing for the admin Toolkits page (shared migration 0114).
 *
 * One row per type this deployment can serve. The columns answer the four
 * questions an operator has about a tile in the "+ Toolkit" chooser: what it is
 * called, what was decided about it, WHICH LAYER decided, and whether an
 * admitted worker can actually build it.
 *
 * ## The capability chip is advisory, and it says so
 *
 * `unverified` does NOT mean the type fails. It means neither the pinned Python
 * worker image nor the Rust worker is KNOWN to carry it. The server sends a
 * sentence with every verdict and this table puts it in the chip's title, so
 * the chip cannot be read as a refusal on its own. Nothing here filters on it:
 * the served catalogue is decided by the policy, never by this column.
 *
 * ## A row with a decision this build does not know renders it verbatim
 *
 * The server can gain an availability before this page does, and rendering an
 * unfamiliar one as "Default" would be a confident wrong answer where the raw
 * word is a correct one.
 */
import { memo, useMemo } from 'react';

import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Stack from '@mui/material/Stack';
import Tooltip from '@mui/material/Tooltip';
import type { GridColDef, GridRenderCellParams } from '@mui/x-data-grid';
import { DataGrid } from '@mui/x-data-grid';

import { t } from '@/shared/i18n';

import type { AdminToolkitType } from './api/adminToolkitTypesApi';

export interface AdminToolkitTypesTableProps {
  readonly types: readonly AdminToolkitType[];
  /** Asked to decide about one type. */
  readonly onDecide?: (toolkitType: AdminToolkitType) => void;
  /** Asked to open the per-project exceptions for one type. */
  readonly onProjects?: (toolkitType: AdminToolkitType) => void;
}

interface ToolkitTypeGridRow extends AdminToolkitType {
  readonly id: string;
}

type ChipColour = 'success' | 'warning' | 'error' | 'default' | 'info';

const AVAILABILITY_CHIPS: Record<string, { readonly color: ChipColour; readonly label: () => string }> =
  {
    default: {
      color: 'default',
      label: () => t('pages.admin.toolkitTypes.availability.default', 'Default'),
    },
    enabled: {
      color: 'success',
      label: () => t('pages.admin.toolkitTypes.availability.enabled', 'Enabled'),
    },
    disabled: {
      color: 'error',
      label: () => t('pages.admin.toolkitTypes.availability.disabled', 'Disabled'),
    },
    restricted: {
      color: 'warning',
      label: () => t('pages.admin.toolkitTypes.availability.restricted', 'Restricted'),
    },
  };

function availabilityChip(value: unknown): { color: ChipColour; label: string } {
  const key = typeof value === 'string' && value !== '' ? value : 'default';
  const known = AVAILABILITY_CHIPS[key];
  return known ? { color: known.color, label: known.label() } : { color: 'default', label: key };
}

const SOURCE_LABELS: Record<string, () => string> = {
  default: () => t('pages.admin.toolkitTypes.source.default', 'Not decided'),
  deployment: () => t('pages.admin.toolkitTypes.source.deployment', 'Deployment'),
  project: () => t('pages.admin.toolkitTypes.source.project', 'Project'),
};

function sourceLabel(value: unknown): string {
  const key = typeof value === 'string' ? value : '';
  const known = SOURCE_LABELS[key];
  return known ? known() : key;
}

export const AdminToolkitTypesTable = memo(function AdminToolkitTypesTable({
  types,
  onDecide,
  onProjects,
}: AdminToolkitTypesTableProps) {
  const rows = useMemo<ToolkitTypeGridRow[]>(
    () => types.map((toolkitType) => ({ ...toolkitType, id: toolkitType.type })),
    [types],
  );

  const columns = useMemo<GridColDef<ToolkitTypeGridRow>[]>(
    () => [
      {
        field: 'label',
        headerName: t('pages.admin.toolkitTypes.column.name', 'Toolkit'),
        flex: 1.2,
        minWidth: 160,
      },
      {
        field: 'type',
        headerName: t('pages.admin.toolkitTypes.column.type', 'Type key'),
        flex: 1,
        minWidth: 150,
      },
      {
        field: 'category',
        headerName: t('pages.admin.toolkitTypes.column.category', 'Category'),
        flex: 0.9,
        minWidth: 130,
      },
      {
        field: 'availability',
        headerName: t('pages.admin.toolkitTypes.column.availability', 'Availability'),
        flex: 0.8,
        minWidth: 130,
        renderCell: (params: GridRenderCellParams<ToolkitTypeGridRow>) => {
          const chip = availabilityChip(params.row.availability);
          return (
            <Tooltip title={params.row.reason}>
              <Chip size="small" color={chip.color} label={chip.label} />
            </Tooltip>
          );
        },
      },
      {
        field: 'source',
        headerName: t('pages.admin.toolkitTypes.column.decidedBy', 'Decided by'),
        flex: 1,
        minWidth: 150,
        valueGetter: (_value, row) =>
          row.decided_by === '' ? sourceLabel(row.source) : `${sourceLabel(row.source)} · ${row.decided_by}`,
      },
      {
        field: 'capability',
        headerName: t('pages.admin.toolkitTypes.column.worker', 'Worker support'),
        flex: 0.9,
        minWidth: 150,
        sortable: false,
        renderCell: (params: GridRenderCellParams<ToolkitTypeGridRow>) => {
          const capability = params.row.capability;
          const supported = capability?.verdict === 'supported';
          return (
            <Tooltip title={capability?.reason ?? ''}>
              <Chip
                size="small"
                color={supported ? 'info' : 'default'}
                variant={supported ? 'filled' : 'outlined'}
                label={
                  supported
                    ? t('pages.admin.toolkitTypes.worker.supported', 'Carried')
                    : t('pages.admin.toolkitTypes.worker.unverified', 'Unverified')
                }
              />
            </Tooltip>
          );
        },
      },
      {
        field: 'project_grants',
        headerName: t('pages.admin.toolkitTypes.column.exceptions', 'Project exceptions'),
        flex: 0.8,
        minWidth: 150,
        sortable: false,
        valueGetter: (_value, row) => row.project_grants.length,
      },
      // The actions column is APPENDED, not rendered inert. Without both
      // handlers there is no write to make, and a disabled button would imply
      // there was one.
      ...(onDecide && onProjects
        ? [
            {
              field: 'actions',
              headerName: t('pages.admin.toolkitTypes.column.actions', 'Actions'),
              width: 200,
              sortable: false,
              filterable: false,
              renderCell: (params: GridRenderCellParams<ToolkitTypeGridRow>) => (
                <Stack direction="row" sx={{ alignItems: 'center', height: '100%' }}>
                  <Button size="small" onClick={() => onDecide(params.row)}>
                    {t('pages.admin.toolkitTypes.action.decide', 'Decide')}
                  </Button>
                  {/* Exceptions hang off a decision, so a type at its default
                      offers no projects control: the server answers 409 for a
                      grant with no decision, and a button whose only outcome is
                      a refusal is worse than no button. */}
                  {params.row.availability === 'default' ? null : (
                    <Button size="small" color="secondary" onClick={() => onProjects(params.row)}>
                      {t('pages.admin.toolkitTypes.action.projects', 'Projects')}
                    </Button>
                  )}
                </Stack>
              ),
            } satisfies GridColDef<ToolkitTypeGridRow>,
          ]
        : []),
    ],
    [onDecide, onProjects],
  );

  return (
    <DataGrid
      rows={rows}
      columns={columns}
      disableRowSelectionOnClick
      autoHeight
      hideFooterSelectedRowCount
      aria-label={t('pages.admin.toolkitTypes.tableLabel', 'Toolkit types this platform can serve')}
      localeText={{
        noRowsLabel: t('pages.admin.toolkitTypes.empty', 'No toolkit type matches these filters.'),
      }}
    />
  );
});
