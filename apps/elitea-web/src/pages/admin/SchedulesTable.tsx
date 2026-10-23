/**
 * The Schedules tab's table (issue #1369).
 *
 * MUI `Table` rather than a DataGrid: two columns are interactive controls (a
 * switch and an inline text editor) bound to a write, and the row count is a
 * dozen, not pages.
 *
 * `SortableHeaderCell` comes from `./AuditTableParts` unchanged — the Audit
 * Trail port left it behind and the sort affordance is identical.
 */
import { useCallback, useEffect, useRef, useState } from 'react';

import Link from '@mui/material/Link';
import Switch from '@mui/material/Switch';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import { useTheme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { SortableHeaderCell } from './AuditTableParts';
import type { AdminRoutineRow } from './api/adminSchedulesApi';
import type { ScheduleSort, ScheduleSortField } from './useAdminSchedulesPage';
import { monoFontFamily } from '@/shared/brand/typeScale';

function formatTime(value: string | null, absent: string): string {
  if (value === null) return absent;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleString();
}

/** `null` is "Never", not an em dash: a routine that has never run is a fact an operator looks for. */
function formatLastRun(row: AdminRoutineRow): string {
  const when = formatTime(row.last_run_at, t('pages.admin.schedules.neverRun', 'Never'));
  if (row.last_outcome === null || row.last_outcome === 'ok') return when;
  return `${when} (${row.last_outcome})`;
}

function ScheduleCell({
  row,
  onScheduleChange,
  isSaving,
}: {
  readonly row: AdminRoutineRow;
  readonly onScheduleChange: ((routine: AdminRoutineRow, schedule: string) => void) | undefined;
  readonly isSaving: boolean;
}) {
  const [draft, setDraft] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const theme = useTheme();

  // Focused programmatically rather than with `autoFocus`: the editor exists
  // only because the operator just clicked the schedule.
  useEffect(() => {
    if (draft !== null) inputRef.current?.focus();
  }, [draft]);

  // Enter commits and closes the editor, and closing it blurs the input, whose
  // onBlur still holds the old draft: without this guard the same edit is sent
  // twice and the second PUT answers 409 "changed while editing".
  const committedRef = useRef(false);
  const open = useCallback((schedule: string) => {
    committedRef.current = false;
    setDraft(schedule);
  }, []);
  const commit = useCallback(() => {
    if (draft === null || !onScheduleChange || committedRef.current) return;
    committedRef.current = true;
    onScheduleChange(row, draft);
    setDraft(null);
  }, [draft, onScheduleChange, row]);

  const overridden = row.schedule_override !== null;
  const label = overridden
    ? t('pages.admin.schedules.customised', 'Custom (default {{schedule}})', {
        schedule: row.default_schedule,
      })
    : t('pages.admin.schedules.usingDefault', 'Default');

  const value = !onScheduleChange ? (
    <Typography variant="bodySmall" sx={{ fontFamily: monoFontFamily }}>
      {row.schedule}
    </Typography>
  ) : draft === null ? (
    <Link
      component="button"
      type="button"
      underline="hover"
      onClick={() => open(row.schedule)}
      sx={{ fontFamily: monoFontFamily }}
    >
      {row.schedule}
    </Link>
  ) : (
    <TextField
      inputRef={inputRef}
      size="small"
      value={draft}
      disabled={isSaving}
      label={t('pages.admin.schedules.scheduleLabel', 'Schedule (interval or cron)')}
      helperText={t(
        'pages.admin.schedules.scheduleHelp',
        'An interval such as 30m or 6h, or five cron fields. Empty restores the default.',
      )}
      slotProps={{
        htmlInput: {
          'aria-label': t('pages.admin.schedules.scheduleLabel', 'Schedule (interval or cron)'),
          style: { fontFamily: theme.typography.fontFamilyMono },
        },
      }}
      onChange={(event) => setDraft(event.target.value)}
      onBlur={commit}
      onKeyDown={(event) => {
        if (event.key === 'Enter') commit();
        // Escape ABANDONS rather than committing; blur is the save.
        if (event.key === 'Escape') {
          committedRef.current = true;
          setDraft(null);
        }
      }}
    />
  );

  return (
    <>
      {value}
      <Typography variant="bodySmall" color="text.secondary" component="div">
        {label}
      </Typography>
    </>
  );
}

export interface SchedulesTableProps {
  readonly rows: readonly AdminRoutineRow[];
  readonly sort: ScheduleSort;
  readonly onSort: (field: ScheduleSortField) => void;
  /** `undefined` => the server would refuse the write, so the switch is disabled. */
  readonly onToggleEnabled: ((routine: AdminRoutineRow) => void) | undefined;
  /** `undefined` => the schedule renders as text rather than as an editor. */
  readonly onScheduleChange: ((routine: AdminRoutineRow, schedule: string) => void) | undefined;
  readonly isSaving: boolean;
}

export function SchedulesTable({
  rows,
  sort,
  onSort,
  onToggleEnabled,
  onScheduleChange,
  isSaving,
}: SchedulesTableProps) {
  if (rows.length === 0) {
    return (
      <Typography variant="bodyMedium" color="text.secondary" data-testid="admin-schedules-empty">
        {t('pages.admin.schedules.empty', 'No schedules match this search.')}
      </Typography>
    );
  }

  return (
    <TableContainer>
      <Table size="small" aria-label={t('pages.admin.schedules.tableLabel', 'Schedules')}>
        <TableHead>
          <TableRow>
            <SortableHeaderCell
              label={t('pages.admin.schedules.column.name', 'Name')}
              field="id"
              sortField={sort.field}
              sortDirection={sort.direction}
              onSort={() => onSort('id')}
            />
            <TableCell>{t('pages.admin.schedules.column.schedule', 'Schedule')}</TableCell>
            <TableCell>{t('pages.admin.schedules.column.active', 'Active')}</TableCell>
            <SortableHeaderCell
              label={t('pages.admin.schedules.column.lastRun', 'Last run')}
              field="last_run_at"
              sortField={sort.field}
              sortDirection={sort.direction}
              onSort={() => onSort('last_run_at')}
            />
            <SortableHeaderCell
              label={t('pages.admin.schedules.column.nextRun', 'Next run')}
              field="next_run_at"
              sortField={sort.field}
              sortDirection={sort.direction}
              onSort={() => onSort('next_run_at')}
            />
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((row) => (
            // A switched-off routine is dimmed but never hidden: it is exactly
            // what an operator comes to this page to find.
            <TableRow key={row.id} sx={row.enabled ? undefined : { opacity: 0.6 }}>
              <TableCell>
                <Typography component="span" variant="bodySmall" sx={{ fontFamily: monoFontFamily }}>
                  {row.id}
                </Typography>
                <Typography variant="bodySmall" color="text.secondary" component="div">
                  {row.description}
                </Typography>
              </TableCell>
              <TableCell>
                <ScheduleCell row={row} onScheduleChange={onScheduleChange} isSaving={isSaving} />
              </TableCell>
              <TableCell>
                <Switch
                  size="small"
                  checked={row.enabled}
                  disabled={!onToggleEnabled || isSaving}
                  onChange={() => onToggleEnabled?.(row)}
                  slotProps={{
                    input: {
                      // i18next interpolation, NOT a template literal: a bundle
                      // value carrying `{{name}}` beats the fallback.
                      'aria-label': t(
                        'pages.admin.schedules.toggleLabel',
                        'Schedule enabled: {{name}}',
                        { name: row.id },
                      ),
                    },
                  }}
                />
              </TableCell>
              <TableCell data-testid="admin-schedules-time">{formatLastRun(row)}</TableCell>
              <TableCell data-testid="admin-schedules-time">
                {row.enabled
                  ? formatTime(row.next_run_at, t('pages.admin.schedules.notPlanned', 'Not planned yet'))
                  : t('pages.admin.schedules.off', 'Off')}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
