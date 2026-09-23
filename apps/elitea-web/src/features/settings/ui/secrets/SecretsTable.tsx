/**
 * Secrets DataGrid table — the core display component.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/secrets/
 * SecretsTable.jsx`.  This component renders:
 *
 *  - A MUI DataGrid with columns for name, value, and actions
 *  - Inline editing for add / edit (via `EditSecretInputGridTable`)
 *  - Show/hide password toggle for secret values (`SecretValueCell`)
 *  - Actions row: visibility toggle, copy, edit, delete (via menu)
 *  - Confirmation dialogs for hide / delete
 *  - Pagination footer with page size selector (5/10/50/100)
 *  - Loading skeleton when data is fetching
 *
 * Deviations from the baseline:
 *  - No tour IDs (`SECRETS_TOUR_TARGET_IDS` → dropped)
 *  - Uses shared `SecretValueCell` + `SecretActionsMenu` components
 *  - Simpler DataGrid integration (no custom `GridTableContainer` wrapper)
 *  - Client-side pagination replaces the old `usePagination` hook
 */
import { memo, useEffect, useMemo, useCallback, useState } from 'react';

import Box from '@mui/material/Box';
import Pagination from '@mui/material/Pagination';
import Select from '@mui/material/Select';
import MenuItem from '@mui/material/MenuItem';
import Skeleton from '@mui/material/Skeleton';
import Typography from '@mui/material/Typography';
import type { GridColDef, GridRenderCellParams } from '@mui/x-data-grid';
import { DataGrid, GridRowModes } from '@mui/x-data-grid';

import type { SecretRow } from '@/entities/secret';
import type { SecretPermissions } from '../../lib/secrets/secretPermissions';
import { SecretRowComponent } from './SecretRow';
import { ConfirmDialog } from './ConfirmDialog';
import { SecretActionsMenu } from './SecretActionsMenu';
import { tableStyles } from './SecretsTable.styles';
import { t } from '@/shared/i18n';

/* ── pagination config ────────────────────────────────────────────────── */

const PAGE_SIZE_OPTIONS = [5, 10, 50, 100] as const;
const DEFAULT_PAGE_SIZE = 10;

/* ── props (grouped: 5 objects, ≤ 12) ─────────────────────────────────── */

export interface SecretsTableProps {
  /* Data */
  rows: SecretRow[];
  setRows: React.Dispatch<React.SetStateAction<SecretRow[]>>;
  rowModesModel: Record<string, { mode: string; fieldToFocus?: string; ignoreModifications?: boolean }>;
  setRowModesModel: React.Dispatch<React.SetStateAction<Record<string, { mode: string; fieldToFocus?: string; ignoreModifications?: boolean }>>>;
  isFetching: boolean;

  /* Visibility state */
  isShowSecretMap: Record<string, boolean>;
  /** What the caller may do here. Every control below is gated on it. */
  permissions: SecretPermissions;

  /* Validation */
  validationErrors: Record<string, boolean>;
  onValidationChange: (rowId: string, field: string, hasError: boolean) => void;

  /* Actions */
  actions: {
    onSave: (rowId: string) => () => Promise<void>;
    onCancel: (rowId: string) => () => void;
    onShowSecret: (rowId: string) => () => Promise<void>;
    onHideSecret: (rowId: string) => void;
    onCopySecretValue: (rowId: string) => () => Promise<void>;
    onActionsMenuClick: (rowId: string) => (event: React.MouseEvent) => void;
    onEdit: (rowId: string) => () => Promise<void>;
    onHide: (rowId: string) => () => void;
    onDelete: (rowId: string) => () => void;
    onCloseAlert: () => () => void;
    onConfirmAlert: (rowId: string) => () => void;
  };

  /* Menu */
  menu: {
    anchorEl: HTMLElement | null;
    anchorRowId: string | null;
    onCloseMenu: () => void;
  };

  /* Dialog */
  dialog: {
    openAlert: string | null;
    openAlertType: 'delete' | 'hide' | '';
  };
}

/* ── columns ───────────────────────────────────────────────────────────── */

const COLUMNS: GridColDef[] = [
  {
    field: 'name',
    headerName: t('entities.secret.table.name', 'Name'),
    flex: 1,
    minWidth: 150,
    renderEditCell: () => null,
    renderCell: () => null,
  },
  {
    field: 'secretValue',
    headerName: t('entities.secret.table.value', 'Value'),
    flex: 2,
    minWidth: 200,
    renderEditCell: () => null,
    renderCell: () => null,
  },
  {
    field: 'actions',
    headerName: t('entities.secret.table.actions', 'Actions'),
    flex: 0.5,
    minWidth: 100,
    maxWidth: 150,
    renderCell: () => null,
    sortable: false,
    disableColumnMenu: true,
  },
];

/** Test hook for the loading skeletons — lets tests tell "loading" from the
 * settled-empty state without reaching for an internal MUI class (R-T6). */
export const SECRETS_SKELETON_TESTID = 'secrets-loading-skeleton';

/* ── empty state ───────────────────────────────────────────────────────── */

/**
 * Shown by the DataGrid when the list settled with no secrets — the normal
 * first-run state. Before #137 this state was indistinguishable from loading
 * because the table short-circuited to skeletons on `rows.length === 0`.
 * Copy matches the baseline's `emptyMessage="No secrets"`
 * (`apps/elitea-ui/.../SecretsTable.jsx:544`).
 */
function NoSecretsOverlay(): React.ReactElement {
  return (
    <Box sx={tableStyles.noRowsOverlay}>
      <Typography variant="bodyMedium" color="text.secondary">
        {t('entities.secret.table.empty', 'No secrets')}
      </Typography>
    </Box>
  );
}

/* ── component ─────────────────────────────────────────────────────────── */

export const SecretsTable = memo(function SecretsTable({
  rows,
  setRows,
  rowModesModel,
  setRowModesModel,
  isFetching,
  isShowSecretMap,
  permissions,
  validationErrors,
  onValidationChange,
  actions,
  menu,
  dialog,
}: SecretsTableProps) {
  const styles = tableStyles;

  /* ── pagination state ─────────────────────────────────────────────── */
  const [currentPage, setCurrentPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_PAGE_SIZE);

  /* ── hooks (must be before any early return) ─────────────────────── */

  // Auto-set new rows to edit mode
  useEffect(() => {
    rows.forEach((row) => {
      if (row.isNew && !rowModesModel[row.id]) {
        setRowModesModel((prev) => ({
          ...prev,
          [row.id]: { mode: GridRowModes.Edit, fieldToFocus: 'name' },
        }));
      }
    });
  }, [rows, rowModesModel, setRowModesModel]);

  // Sorted: new rows first, then alphabetical by name
  const sortedRows = useMemo(() => {
    const newRows = rows.filter((r) => r.isNew);
    const existingRows = rows.filter((r) => !r.isNew).sort((a, b) =>
      a.name.localeCompare(b.name),
    );
    return [...newRows, ...existingRows];
  }, [rows]);

  /*
   * elitea_issues 4860 — a new (unsaved) row always sorts to the FRONT
   * above, so it always lands on page 1's slice below. Clicking "+" while
   * viewing any other page created the row exactly as before, just off
   * the visible slice — nothing on screen changed and nothing said why.
   * Jump back to page 1 whenever an unsaved row exists, so the row that
   * was just created is the one the user is actually looking at.
   */
  const hasNewRow = useMemo(() => rows.some((row) => row.isNew), [rows]);
  useEffect(() => {
    if (hasNewRow) setCurrentPage(1);
  }, [hasNewRow]);

  // Paginate
  const totalPages = Math.max(1, Math.ceil(sortedRows.length / pageSize));
  const paginatedRows = useMemo(() => {
    const start = (currentPage - 1) * pageSize;
    return sortedRows.slice(start, start + pageSize);
  }, [sortedRows, currentPage, pageSize]);

  // Reset page when total pages changes (e.g., rows were deleted)
  useEffect(() => {
    if (currentPage > totalPages) {
      setCurrentPage(1);
    }
  }, [totalPages, currentPage]);

  const renderRowCell = useCallback(
    (params: GridRenderCellParams) => (
      <SecretRowComponent
        row={params.row as SecretRow}
        params={params}
        rowModesModel={rowModesModel}
        validationErrors={validationErrors}
        isShowSecretMap={isShowSecretMap}
        permissions={permissions}
        setRows={setRows}
        setRowModesModel={setRowModesModel}
        onValidationChange={onValidationChange}
        actions={actions}
      />
    ),
    [rowModesModel, validationErrors, isShowSecretMap, permissions, onValidationChange, actions, setRows, setRowModesModel],
  );

  // Every column shares the same cell renderer — `SecretRowComponent`
  // switches on `params.field` to render only that column's slice of
  // content (name / value / actions). Overriding only the 'name' column
  // (as a prior version of this file did) left 'secretValue' and 'actions'
  // on their placeholder `renderCell: () => null`, so those columns always
  // rendered blank.
  const columnsWithCell = COLUMNS.map((col) => ({ ...col, renderCell: renderRowCell }) as GridColDef);

  /* ── loading state ────────────────────────────────────────────────── */

  // ONLY genuine loading. A prior version also skeletoned `rows.length === 0`,
  // which made the normal first-run state (and every project with no secrets,
  // i.e. exactly what a working backend returns) an indefinite loading screen
  // with no grid, no column headers and no footer (#137). The baseline
  // (`apps/elitea-ui/.../SecretsTable.jsx:535`) branches on `isFetching`
  // alone and renders an explicit "No secrets" empty state instead.
  if (isFetching) {
    return (
      <Box sx={styles.skeletonContainer}>
        {Array.from({ length: 8 }).map((_, i) => (
          <Skeleton
            key={`skeleton-${i}`}
            data-testid={SECRETS_SKELETON_TESTID}
            variant="rectangular"
            width="100%"
            height={48}
            sx={styles.skeleton}
          />
        ))}
      </Box>
    );
  }

  /* ── render ───────────────────────────────────────────────────────── */

  return (
    <Box sx={styles.container}>
      <DataGrid
        rows={paginatedRows}
        columns={columnsWithCell}
        rowHeight={48}
        hideFooter
        getRowId={(row: SecretRow) => row.id}
        slots={{ noRowsOverlay: NoSecretsOverlay }}
        sx={styles.dataGrid!}
      />

      {/* Pagination footer */}
      <Box sx={styles.pagination}>
        <Box sx={styles.pageSizeSelector}>
          <label htmlFor={`page-size-select`}>
            {t('entities.secret.table.pageSize', 'Rows per page')}
          </label>
          <Select
            id="page-size-select"
            value={pageSize}
            onChange={(e) => {
              setPageSize(Number(e.target.value));
              setCurrentPage(1);
            }}
            size="small"
            sx={{ marginLeft: '0.5rem' }}
          >
            {PAGE_SIZE_OPTIONS.map((size) => (
              <MenuItem key={size} value={size}>
                {size}
              </MenuItem>
            ))}
          </Select>
        </Box>
        <Pagination
          count={totalPages}
          page={currentPage}
          onChange={(_e, page) => setCurrentPage(page)}
          color="primary"
          showFirstButton
          showLastButton
        />
        <Box sx={styles.pageInfo}>
          <Typography variant="bodySmall" color="text.secondary">
            {t('entities.secret.table.pageInfo', `Page ${currentPage} of ${totalPages}`, { currentPage, totalPages })}
          </Typography>
        </Box>
      </Box>

      {/* Actions menu for the active row */}
      {(() => {
        const { anchorRowId } = menu;
        if (!anchorRowId) return null;
        return (
          <SecretActionsMenu
            rowId={anchorRowId}
            isNew={(rows.find((r) => r.id === anchorRowId)?.isNew) ?? true}
            isDefault={(rows.find((r) => r.id === anchorRowId)?.isDefault) ?? false}
            permissions={permissions}
            anchorEl={menu.anchorEl}
            onClose={menu.onCloseMenu}
            onEdit={() => { void actions.onEdit(anchorRowId)(); }}
            onHide={actions.onHide(anchorRowId)}
            onDelete={actions.onDelete(anchorRowId)}
          />
        );
      })()}

      {/* Confirm dialog */}
      <ConfirmDialog
        open={!!dialog.openAlert}
        alertType={dialog.openAlertType}
        rowName={rows.find((r) => r.id === dialog.openAlert)?.name ?? ''}
        onClose={() => { actions.onCloseAlert()(); }}
        onConfirm={() => { actions.onConfirmAlert(dialog.openAlert ?? '')(); }}
      />
    </Box>
  );
});
