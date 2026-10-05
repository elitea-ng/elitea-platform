/**
 * The Schedules tab's table (unit A14, issue #200).
 *
 * MUI `Table` rather than the DataGrid `AdminProjectsTable` uses: two of the
 * five columns are interactive controls (a switch and an inline text editor)
 * bound to a write, and the row count is tens, not pages. A DataGrid here would
 * add virtualisation and a column API to buy nothing.
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
import type { AdminScheduleRow } from './api/adminSchedulesApi';
import type { ScheduleSort, ScheduleSortField } from './useAdminSchedulesPage';
import { monoFontFamily } from '@/shared/brand/typeScale';

export interface SchedulesTableProps {
  readonly rows: readonly AdminScheduleRow[];
  readonly sort: ScheduleSort;
  readonly onSort: (field: ScheduleSortField) => void;
  readonly onOpenHistory: (schedule: AdminScheduleRow) => void;
  /** `undefined` ⇒ the server would refuse the write, so the switch is disabled. */
  readonly onToggleActive: ((schedule: AdminScheduleRow) => void) | undefined;
  /** `undefined` ⇒ the cron cell renders as text rather than as an editor. */
  readonly onCronChange: ((schedule: AdminScheduleRow, cron: string) => void) | undefined;
  readonly isSaving: boolean;
}

/**
 * `null` is rendered as "Never", not as an em dash: a schedule that has never
 * run is a fact about the platform, and it is the one an operator looks for.
 */
function formatLastRun(value: string | null): string {
  if (value === null) return t('pages.admin.schedules.neverRun', 'Never');
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return parsed.toLocaleString();
}

function CronCell({
  row,
  onCronChange,
  isSaving,
}: {
  readonly row: AdminScheduleRow;
  readonly onCronChange: ((schedule: AdminScheduleRow, cron: string) => void) | undefined;
  readonly isSaving: boolean;
}) {
  const [draft, setDraft] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const theme = useTheme();

  // Focused programmatically rather than with `autoFocus`, which jsx-a11y bans
  // for the case it is actually bad at — a control grabbing focus on page load.
  // Here the editor exists only because the operator just clicked the cron, and
  // an inline editor that needs a second click to type in is a worse control.
  useEffect(() => {
    if (draft !== null) inputRef.current?.focus();
  }, [draft]);

  const commit = useCallback(() => {
    if (draft === null || !onCronChange) return;
    onCronChange(row, draft);
    setDraft(null);
  }, [draft, onCronChange, row]);

  if (!onCronChange) {
    return (
      <Typography variant="bodySmall" sx={{ fontFamily: monoFontFamily }}>
        {row.cron}
      </Typography>
    );
  }

  if (draft === null) {
    return (
      <Link
        component="button"
        type="button"
        underline="hover"
        onClick={() => setDraft(row.cron)}
        sx={{ fontFamily: monoFontFamily }}
      >
        {row.cron}
      </Link>
    );
  }

  return (
    <TextField
      inputRef={inputRef}
      size="small"
      value={draft}
      disabled={isSaving}
      // The five-field contract the scheduler's parser enforces. Named on the
      // control because the server's rejection arrives only after a save.
      label={t('pages.admin.schedules.cronLabel', 'Cron (5 fields)')}
      slotProps={{
        htmlInput: {
          'aria-label': t('pages.admin.schedules.cronLabel', 'Cron (5 fields)'),
          // Applied to the input element itself rather than reached through a
          // nested MUI class selector: R-T6 keeps MUI internals out of call
          // sites, and the theme gate greps for those class names in COMMENTS
          // too — so this one deliberately does not name the one it replaced.
          style: { fontFamily: theme.typography.fontFamilyMono },
        },
      }}
      onChange={(event) => setDraft(event.target.value)}
      onBlur={commit}
      onKeyDown={(event) => {
        if (event.key === 'Enter') commit();
        // Escape ABANDONS rather than committing. Without it the only way out
        // of a mistyped cron is to blur, which saves it.
        if (event.key === 'Escape') setDraft(null);
      }}
    />
  );
}

export function SchedulesTable({
  rows,
  sort,
  onSort,
  onOpenHistory,
  onToggleActive,
  onCronChange,
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
              field="name"
              sortField={sort.field}
              sortDirection={sort.direction}
              onSort={() => onSort('name')}
            />
            <TableCell>{t('pages.admin.schedules.column.cron', 'Cron')}</TableCell>
            <TableCell>{t('pages.admin.schedules.column.active', 'Active')}</TableCell>
            <SortableHeaderCell
              label={t('pages.admin.schedules.column.function', 'Function')}
              field="rpc_func"
              sortField={sort.field}
              sortDirection={sort.direction}
              onSort={() => onSort('rpc_func')}
            />
            <SortableHeaderCell
              label={t('pages.admin.schedules.column.lastRun', 'Last run')}
              field="last_run"
              sortField={sort.field}
              sortDirection={sort.direction}
              onSort={() => onSort('last_run')}
            />
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((row) => (
            // An inactive row is dimmed but never hidden: a disabled platform
            // job is exactly what an operator comes to this page to find.
            <TableRow key={row.id} sx={row.active ? undefined : { opacity: 0.6 }}>
              <TableCell>
                <Link
                  component="button"
                  type="button"
                  underline="hover"
                  onClick={() => onOpenHistory(row)}
                >
                  {row.name}
                </Link>
              </TableCell>
              <TableCell>
                <CronCell row={row} onCronChange={onCronChange} isSaving={isSaving} />
              </TableCell>
              <TableCell>
                <Switch
                  size="small"
                  checked={row.active}
                  disabled={!onToggleActive || isSaving}
                  onChange={() => onToggleActive?.(row)}
                  slotProps={{
                    input: {
                      // i18next interpolation, NOT a template literal: a bundle
                      // value carrying `{{name}}` beats the fallback, and a
                      // template-literal fallback would render the braces.
                      'aria-label': t(
                        'pages.admin.schedules.toggleLabel',
                        'Schedule enabled: {{name}}',
                        { name: row.name },
                      ),
                    },
                  }}
                />
              </TableCell>
              <TableCell>
                <Typography component="span" variant="bodySmall" sx={{ fontFamily: monoFontFamily }}>
                  {row.rpc_func}
                </Typography>
              </TableCell>
              <TableCell>{formatLastRun(row.last_run)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
