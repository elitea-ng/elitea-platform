/**
 * The global-vault table for the admin Secrets page (unit A14).
 *
 * Reference: `frontends/admin_ui/frontend/src/pages/SecretsPage/SecretsTable.jsx`
 * (read-only). A rewrite, not a copy — that component is MUI 7 over a bespoke
 * `GridTable` plus a `useResponsiveColumns` hook that reads `window.innerWidth`
 * once at render; here the columns are MUI X DataGrid `flex` definitions,
 * matching `AdminUsersTable` and `AdminProjectsTable`.
 *
 * ## The value cell is REUSED, not rewritten
 *
 * `features/settings/ui/secrets/SecretValueCell` already implements exactly this
 * cell for the PROJECT vault: masked by default, a reveal toggle gated on a
 * permission, and a copy button whose plaintext the caller fetches fresh rather
 * than reading off the rendered DOM. That last property is the one worth
 * inheriting — copying `displayText` would copy the mask whenever the value is
 * hidden — so the component is imported rather than reimplemented. The data
 * behind it differs (a different vault, a different endpoint); the cell does not.
 *
 * ## Revealed values live here and nowhere else
 *
 * The plaintext a reveal returns is held in this component's state, keyed by
 * name, and dropped whenever the row set changes. It is never written to the
 * query cache (see `./api/adminSecretsApi`), never persisted, and never included
 * in a row object that something else might serialise.
 */
import { memo, useCallback, useEffect, useMemo, useState } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { GridColDef, GridRenderCellParams } from '@mui/x-data-grid';
import { DataGrid } from '@mui/x-data-grid';

import { secretsFeature } from '@/features/settings';
import { t } from '@/shared/i18n';

import type { AdminSecret } from './api/adminSecretsApi';
import { monoFontFamily } from '@/shared/brand/typeScale';

const { SecretValueCell } = secretsFeature;

/** What a masked value renders as before the operator reveals it. */
const MASK = '••••••••';

export interface AdminSecretsTableProps {
  readonly secrets: readonly AdminSecret[];
  readonly isLoading: boolean;
  /**
   * Fetches one plaintext value. `null` means the server does not hold that
   * name; a rejection means it refused, and the row simply stays masked.
   */
  readonly onReveal: (name: string) => Promise<string | null>;
  /** Absent ⇒ the reveal/copy affordances are not rendered at all. */
  readonly canReveal: boolean;
  /** Absent ⇒ the control is not rendered (no permission, or the Internal tab). */
  readonly onEdit: ((name: string) => void) | undefined;
  readonly onDelete: ((name: string) => void) | undefined;
}

interface AdminSecretGridRow {
  readonly id: string;
  readonly name: string;
}

export const AdminSecretsTable = memo(function AdminSecretsTable({
  secrets,
  isLoading,
  onReveal,
  canReveal,
  onEdit,
  onDelete,
}: AdminSecretsTableProps) {
  // name → plaintext. Presence in this map is what "revealed" means.
  const [revealed, setRevealed] = useState<Record<string, string>>({});

  // Drop every revealed value when the row set changes — after a create, an
  // edit or a delete, a value held here is stale and may belong to a name that
  // no longer exists.
  // Comma-joined: secret names match `[A-Za-z0-9_]+`, so no name can contain
  // the separator and two different row sets cannot produce the same key.
  const rowKey = secrets.map((secret) => secret.name).join(',');
  useEffect(() => {
    setRevealed({});
  }, [rowKey]);

  const handleToggle = useCallback(
    (name: string) => {
      if (name in revealed) {
        setRevealed((current) => {
          const next = { ...current };
          delete next[name];
          return next;
        });
        return;
      }
      void onReveal(name)
        .then((value) => {
          if (value !== null) setRevealed((current) => ({ ...current, [name]: value }));
        })
        .catch(() => {
          // A refused reveal leaves the row masked. The page renders the
          // server's reason; a thrown error here would take the table down.
        });
    },
    [onReveal, revealed],
  );

  const handleCopy = useCallback(
    async (name: string): Promise<void> => {
      // Always fetched fresh, never read off `revealed`: the reference does the
      // same, and it means Copy works on a masked row without unmasking it.
      const value = await onReveal(name);
      if (value === null) return;
      await navigator.clipboard.writeText(value);
    },
    [onReveal],
  );

  const columns = useMemo<GridColDef<AdminSecretGridRow>[]>(
    () => [
      {
        field: 'name',
        headerName: t('pages.admin.secrets.column.name', 'Name'),
        flex: 1,
        minWidth: 160,
        renderCell: (params: GridRenderCellParams<AdminSecretGridRow>) => (
          <Typography variant="bodySmall" color="text.secondary" sx={{ fontFamily: monoFontFamily }}>
            {params.row.name}
          </Typography>
        ),
      },
      {
        field: 'value',
        headerName: t('pages.admin.secrets.column.value', 'Value'),
        flex: 2,
        minWidth: 200,
        sortable: false,
        renderCell: (params: GridRenderCellParams<AdminSecretGridRow>) => (
          <SecretValueCell
            label={MASK}
            value={revealed[params.row.name] ?? ''}
            isVisible={params.row.name in revealed}
            onCopy={() => handleCopy(params.row.name)}
            onToggleVisibility={() => handleToggle(params.row.name)}
            canToggleVisibility={canReveal}
            // Copy fetches the plaintext through the same route the reveal
            // uses, so it carries the same permission. This prop's contract
            // already said "absent ⇒ the reveal/copy affordances are not
            // rendered at all"; only the reveal half was wired (#402).
            canCopy={canReveal}
          />
        ),
      },
      {
        field: 'actions',
        headerName: t('pages.admin.secrets.column.actions', 'Actions'),
        flex: 0.4,
        minWidth: 110,
        sortable: false,
        disableColumnMenu: true,
        renderCell: (params: GridRenderCellParams<AdminSecretGridRow>) => (
          <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.125rem' }}>
            {onEdit ? (
              <Tooltip title={t('pages.admin.secrets.action.edit', 'Edit')}>
                <IconButton
                  size="small"
                  aria-label={`${t('pages.admin.secrets.action.edit', 'Edit')}: ${params.row.name}`}
                  onClick={() => onEdit(params.row.name)}
                >
                  <EditOutlinedIcon fontSize="small" />
                </IconButton>
              </Tooltip>
            ) : null}
            {onDelete ? (
              <Tooltip title={t('pages.admin.secrets.action.delete', 'Delete')}>
                <IconButton
                  size="small"
                  color="error"
                  aria-label={`${t('pages.admin.secrets.action.delete', 'Delete')}: ${params.row.name}`}
                  onClick={() => onDelete(params.row.name)}
                >
                  <DeleteOutlinedIcon fontSize="small" />
                </IconButton>
              </Tooltip>
            ) : null}
          </Box>
        ),
      },
    ],
    [revealed, canReveal, handleCopy, handleToggle, onEdit, onDelete],
  );

  const rows = useMemo<AdminSecretGridRow[]>(
    () => secrets.map((secret) => ({ id: secret.name, name: secret.name })),
    [secrets],
  );

  return (
    <DataGrid
      rows={rows}
      columns={columns}
      loading={isLoading}
      disableRowSelectionOnClick
      hideFooter
      aria-label={t('pages.admin.secrets.table', 'Global secrets')}
      localeText={{ noRowsLabel: t('pages.admin.secrets.empty', 'No secrets found') }}
      sx={{ border: 'none', flex: 1, minHeight: '12rem' }}
    />
  );
});
