/**
 * Artifacts › a bucket's "Manage access" dialog.
 *
 * Reference (read-only): `frontends/EliteaUI/src/[fsd]/features/artifacts/ui/
 * bucket-access/` — `ManagePermissionsModal.jsx`, `BucketAccessTable.jsx`,
 * `AddBucketUserDialog.jsx`, `EditBucketUserDialog.jsx` and
 * `DefaultPermissionsBanner.jsx`. Its title, its banner sentence, its column
 * set and its three permission options are reproduced.
 *
 * ## What the screen is saying
 *
 * The access model is EXCEPTIONS to a default of full access, not an
 * allow-list — see `internal/api/v2/artifacts/bucket_permissions.go`. So the
 * table is headed "Exceptions" and the banner states the default out loud. A
 * member who is not in the table may read and write the bucket; that is the
 * fact an operator most needs on screen before they add a row, and it is why
 * an empty table is a complete, correct answer rather than a blank state.
 *
 * ## Two things this dialog does NOT do
 *
 *  1. It does not offer "Read/write" when ADDING. The reference does not
 *     either (`ADD_EXCEPTION_OPTIONS` holds only Read-only and No access),
 *     because read/write IS the default: adding an exception that grants the
 *     default is adding a row that changes nothing.
 *  2. Choosing "Read/write (default)" on an EXISTING row REMOVES the row
 *     rather than storing `["read","write"]`. Same reason, and the same
 *     behaviour as the reference's `isRemoval` branch.
 *
 * ## Bulk edit (issue 940/A10, ELITEA-2480)
 *
 * A header checkbox selects/deselects every VISIBLE exception row at once;
 * each row also carries its own checkbox. With one or more selected, the
 * pencil icon above the table opens a small modal offering the same three
 * access options a single row's own `<Select>` does — applied to every
 * selected user via the SAME `onSetAccess` the single-row path already
 * calls, once per selected id. Choosing "Read/write (default)" here removes
 * every selected row from the table (the same "isRemoval" behaviour #2
 * above already gives a single row) — no special-cased bulk-remove path,
 * because `onSetAccess`/`permissionsFromAccess` already treat that choice
 * as a removal regardless of how many calls arrive.
 */
import { useMemo, useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Autocomplete from '@mui/material/Autocomplete';
import Box from '@mui/material/Box';
import Checkbox from '@mui/material/Checkbox';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import IconButton from '@mui/material/IconButton';
import MenuItem from '@mui/material/MenuItem';
import Select from '@mui/material/Select';
import type { SxProps, Theme } from '@mui/material/styles';
import TextField from '@mui/material/TextField';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import type { BucketPermissionRow } from '../api/bucketAccessApi';
import type { BucketAccessCandidate } from '../model/useBucketAccess';
import {
  accessFromPermissions,
  BUCKET_ACCESS,
  type BucketAccess,
} from '../lib/bucketAccess';

export interface BucketAccessDialogProps {
  readonly open: boolean;
  readonly bucket: string;
  /** Every exception in the project; this dialog renders the ones for `bucket`. */
  readonly rows: readonly BucketPermissionRow[];
  readonly candidates: readonly BucketAccessCandidate[];
  readonly isLoading: boolean;
  readonly isSaving: boolean;
  readonly errorMessage?: string | undefined;
  readonly onClose: () => void;
  readonly onSetAccess: (userId: number, access: BucketAccess) => void;
  readonly onRemove: (userId: number) => void;
}

/** One rendered exception row. */
interface ExceptionRow {
  readonly userId: number;
  readonly name: string;
  readonly email: string;
  readonly access: BucketAccess;
}

function accessLabel(access: BucketAccess): string {
  if (access === BUCKET_ACCESS.readWrite) {
    return t('artifacts.bucketAccess.option.readWrite', 'Read/write (default)');
  }
  if (access === BUCKET_ACCESS.read) return t('artifacts.bucketAccess.option.read', 'Read-only');
  return t('artifacts.bucketAccess.option.noAccess', 'No access');
}

/**
 * The exceptions FOR THIS BUCKET.
 *
 * A member whose map carries other buckets but not this one has no exception
 * here, so they are not a row. `bucket in map` is the test, never a truthiness
 * check on the value: `[]` is an exception ("no access") and it is falsy.
 */
function exceptionRows(
  rows: readonly BucketPermissionRow[],
  bucket: string,
): readonly ExceptionRow[] {
  const result: ExceptionRow[] = [];
  for (const row of rows) {
    const map = row.bucket_permissions;
    if (!Object.prototype.hasOwnProperty.call(map, bucket)) continue;
    result.push({
      userId: row.user_id,
      name: row.name ?? '',
      email: row.email ?? '',
      access: accessFromPermissions(map[bucket]),
    });
  }
  return result;
}

export function BucketAccessDialog(props: BucketAccessDialogProps): ReactNode {
  const [pendingUser, setPendingUser] = useState<BucketAccessCandidate | null>(null);
  const [pendingAccess, setPendingAccess] = useState<BucketAccess>(BUCKET_ACCESS.read);
  // Issue 940/A10 — bulk edit. `selectedUserIds` only ever holds ids of rows
  // currently ON SCREEN (`toggleSelectAll`/row checkboxes both source from
  // `rows` below); a row that leaves the table after a save is simply no
  // longer selectable, not specially reconciled.
  const [selectedUserIds, setSelectedUserIds] = useState<ReadonlySet<number>>(new Set());
  const [isBulkEditOpen, setIsBulkEditOpen] = useState(false);
  const [bulkAccess, setBulkAccess] = useState<BucketAccess>(BUCKET_ACCESS.noAccess);

  const rows = useMemo(
    () => exceptionRows(props.rows, props.bucket),
    [props.rows, props.bucket],
  );
  const listed = useMemo(() => new Set(rows.map((row) => row.userId)), [rows]);
  const selectable = useMemo(
    () => props.candidates.filter((candidate) => !listed.has(candidate.id)),
    [props.candidates, listed],
  );

  const addException = (): void => {
    if (pendingUser === null) return;
    props.onSetAccess(pendingUser.id, pendingAccess);
    setPendingUser(null);
    setPendingAccess(BUCKET_ACCESS.read);
  };

  const allSelected = rows.length > 0 && rows.every((row) => selectedUserIds.has(row.userId));
  const someSelected = rows.some((row) => selectedUserIds.has(row.userId));

  const toggleSelectAll = (): void => {
    setSelectedUserIds(allSelected ? new Set() : new Set(rows.map((row) => row.userId)));
  };

  const toggleRow = (userId: number): void => {
    setSelectedUserIds((prev) => {
      const next = new Set(prev);
      if (next.has(userId)) next.delete(userId);
      else next.add(userId);
      return next;
    });
  };

  const applyBulkEdit = (): void => {
    for (const userId of selectedUserIds) props.onSetAccess(userId, bulkAccess);
    setSelectedUserIds(new Set());
    setIsBulkEditOpen(false);
  };

  return (
    <Dialog open={props.open} onClose={props.onClose} maxWidth="md" fullWidth>
      <DialogTitle>{t('artifacts.bucketAccess.title', 'Manage Permissions')}</DialogTitle>
      <DialogContent data-testid="bucket-access-dialog">
        <Alert severity="info" sx={bannerSx}>
          {t(
            'artifacts.bucketAccess.default',
            'All users have read/write permissions by default.',
          )}
        </Alert>

        {props.errorMessage !== undefined && (
          <Alert severity="warning" sx={bannerSx} data-testid="bucket-access-error">
            {props.errorMessage}
          </Alert>
        )}

        <Box sx={exceptionsHeadingRowSx}>
          <Typography variant="bodyMedium">
            {t('artifacts.bucketAccess.exceptions', 'Exceptions')}
            {` – ${rows.length}`}
          </Typography>
          {rows.length > 0 && (
            <Box sx={bulkHeaderControlsSx}>
              <Checkbox
                size="small"
                checked={allSelected}
                indeterminate={someSelected && !allSelected}
                disabled={props.isSaving}
                slotProps={{ input: { 'aria-label': 'Select all exceptions' } }}
                data-testid="bucket-access-select-all"
                onChange={toggleSelectAll}
              />
              <Tooltip title={t('artifacts.bucketAccess.bulkEdit', 'Bulk edit permissions')}>
                <span>
                  <IconButton
                    size="small"
                    color="tertiary"
                    disabled={!someSelected || props.isSaving}
                    aria-label={t('artifacts.bucketAccess.bulkEdit', 'Bulk edit permissions')}
                    data-testid="bucket-access-bulk-edit-open"
                    onClick={() => setIsBulkEditOpen(true)}
                  >
                    <EditOutlinedIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>
            </Box>
          )}
        </Box>

        {props.isLoading && (
          <Typography variant="bodyMedium" color="text.secondary">
            {t('artifacts.bucketAccess.loading', 'Loading exceptions…')}
          </Typography>
        )}

        {!props.isLoading && rows.length === 0 && (
          <Typography variant="bodyMedium" color="text.secondary" data-testid="bucket-access-empty">
            {t('artifacts.bucketAccess.empty', 'No exceptions added yet.')}
          </Typography>
        )}

        {rows.map((row) => (
          <Box key={row.userId} sx={rowSx} data-testid={`bucket-access-row-${row.userId}`}>
            <Checkbox
              size="small"
              checked={selectedUserIds.has(row.userId)}
              disabled={props.isSaving}
              slotProps={{ input: { 'aria-label': `Select ${row.name === '' ? row.userId : row.name}` } }}
              onChange={() => toggleRow(row.userId)}
            />
            <Typography variant="bodyMedium" sx={cellSx}>
              {row.name === '' ? `#${row.userId}` : row.name}
            </Typography>
            <Typography variant="bodySmall" color="text.secondary" sx={cellSx}>
              {row.email}
            </Typography>
            <Select
              size="small"
              value={row.access}
              disabled={props.isSaving}
              inputProps={{ 'aria-label': `Permissions for ${row.name === '' ? row.userId : row.name}` }}
              onChange={(event) => props.onSetAccess(row.userId, event.target.value)}
            >
              {[BUCKET_ACCESS.readWrite, BUCKET_ACCESS.read, BUCKET_ACCESS.noAccess].map((option) => (
                <MenuItem key={option} value={option}>{accessLabel(option)}</MenuItem>
              ))}
            </Select>
            <IconButton
              size="small"
              color="tertiary"
              disabled={props.isSaving}
              aria-label={`Remove exception for ${row.name === '' ? row.userId : row.name}`}
              onClick={() => props.onRemove(row.userId)}
            >
              <DeleteOutlinedIcon fontSize="small" />
            </IconButton>
          </Box>
        ))}

        <Box sx={addRowSx}>
          <Autocomplete
            sx={pickerSx}
            size="small"
            options={[...selectable]}
            value={pendingUser}
            onChange={(_event, value) => setPendingUser(value)}
            getOptionLabel={(option) => (option.name === '' ? option.email : option.name)}
            isOptionEqualToValue={(option, value) => option.id === value.id}
            renderInput={(params) => (
              <TextField {...params} label={t('artifacts.bucketAccess.users', 'Users')} />
            )}
          />
          <Select
            size="small"
            value={pendingAccess}
            inputProps={{ 'aria-label': 'New exception permissions' }}
            onChange={(event) => setPendingAccess(event.target.value)}
          >
            {/* Read/write is absent on purpose — see this file's header. */}
            {[BUCKET_ACCESS.read, BUCKET_ACCESS.noAccess].map((option) => (
              <MenuItem key={option} value={option}>{accessLabel(option)}</MenuItem>
            ))}
          </Select>
          <BaseBtn
            variant="contained"
            disabled={pendingUser === null || props.isSaving}
            onClick={addException}
          >
            {t('artifacts.bucketAccess.add', 'Add exception')}
          </BaseBtn>
        </Box>
      </DialogContent>
      <DialogActions>
        <BaseBtn variant="text" onClick={props.onClose}>
          {t('artifacts.bucketAccess.close', 'Close')}
        </BaseBtn>
      </DialogActions>

      <Dialog open={isBulkEditOpen} onClose={() => setIsBulkEditOpen(false)}>
        <DialogTitle>{t('artifacts.bucketAccess.bulkEdit', 'Bulk edit permissions')}</DialogTitle>
        <DialogContent data-testid="bucket-access-bulk-edit-dialog">
          <Typography variant="bodyMedium" sx={headingSx}>
            {t('artifacts.bucketAccess.bulkEditCount', '{{count}} users selected', { count: selectedUserIds.size })}
          </Typography>
          <Select
            fullWidth
            size="small"
            value={bulkAccess}
            inputProps={{ 'aria-label': 'Bulk edit permissions value' }}
            onChange={(event) => setBulkAccess(event.target.value)}
          >
            {[BUCKET_ACCESS.readWrite, BUCKET_ACCESS.read, BUCKET_ACCESS.noAccess].map((option) => (
              <MenuItem key={option} value={option}>{accessLabel(option)}</MenuItem>
            ))}
          </Select>
        </DialogContent>
        <DialogActions>
          <BaseBtn variant="text" onClick={() => setIsBulkEditOpen(false)}>
            {t('artifacts.bucketAccess.cancel', 'Cancel')}
          </BaseBtn>
          <BaseBtn
            variant="contained"
            disabled={props.isSaving}
            data-testid="bucket-access-bulk-edit-save"
            onClick={applyBulkEdit}
          >
            {t('artifacts.bucketAccess.save', 'Save')}
          </BaseBtn>
        </DialogActions>
      </Dialog>
    </Dialog>
  );
}

const bannerSx: SxProps<Theme> = (theme) => ({ marginBottom: theme.spacing(2) });
const headingSx: SxProps<Theme> = (theme) => ({ marginBottom: theme.spacing(1) });
const rowSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  paddingBlock: theme.spacing(1),
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.conversationItemDivider}`,
});
const cellSx: SxProps<Theme> = { flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis' };
const addRowSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1.5),
  marginTop: theme.spacing(2),
});
const pickerSx: SxProps<Theme> = { flex: 1, minWidth: '12rem' };
/** Issue 940/A10 — the "Exceptions – N" heading's own row, now also holding the Select-All checkbox + bulk-edit icon on its trailing edge. */
const exceptionsHeadingRowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'space-between' };
const bulkHeaderControlsSx: SxProps<Theme> = { display: 'flex', alignItems: 'center' };
