/**
 * Cells and chrome shared by the two audit tables (unit A14).
 *
 * Both tables are plain MUI `Table`s rather than the `DataGrid` the admin Users
 * page uses. The trace table needs nested rows — a trace expands to reveal its
 * spans — and master/detail is not in the free `@mui/x-data-grid` tier this app
 * depends on. Using `Table` for the span view too keeps one technology on one
 * page instead of two that look almost but not quite alike.
 */
import type { ComponentType, ReactNode } from 'react';

import AssignmentOutlinedIcon from '@mui/icons-material/AssignmentOutlined';
import AutoAwesomeOutlinedIcon from '@mui/icons-material/AutoAwesomeOutlined';
import BuildOutlinedIcon from '@mui/icons-material/BuildOutlined';
import CableOutlinedIcon from '@mui/icons-material/CableOutlined';
import HelpOutlineOutlinedIcon from '@mui/icons-material/HelpOutlineOutlined';
import HttpOutlinedIcon from '@mui/icons-material/HttpOutlined';
import ScheduleOutlinedIcon from '@mui/icons-material/ScheduleOutlined';
import SmartToyOutlinedIcon from '@mui/icons-material/SmartToyOutlined';
import SyncAltOutlinedIcon from '@mui/icons-material/SyncAltOutlined';
import Box from '@mui/material/Box';
import type { SvgIconProps } from '@mui/material/SvgIcon';
import TableCell from '@mui/material/TableCell';
import TableSortLabel from '@mui/material/TableSortLabel';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { eventTypeColor } from './auditFormat';
import { eventTypeLabel } from './AuditTrailFilters';

const EVENT_TYPE_ICONS: Record<string, ComponentType<SvgIconProps>> = {
  api: HttpOutlinedIcon,
  socketio: CableOutlinedIcon,
  rpc: SyncAltOutlinedIcon,
  agent: SmartToyOutlinedIcon,
  tool: BuildOutlinedIcon,
  llm: AutoAwesomeOutlinedIcon,
  schedule: ScheduleOutlinedIcon,
  admin_task: AssignmentOutlinedIcon,
};

/**
 * The event-type glyph. An unknown type gets a question mark and its raw name,
 * not a blank cell — a type the server grows before this map learns it should
 * still be legible.
 */
export function EventTypeIcon({ eventType }: { readonly eventType: string | null }): ReactNode {
  const resolved = eventType ?? '';
  const IconComponent = EVENT_TYPE_ICONS[resolved] ?? HelpOutlineOutlinedIcon;
  const label = resolved ? eventTypeLabel(resolved) : '—';
  return (
    <Tooltip title={label} placement="top">
      <Box sx={{ display: 'flex', alignItems: 'center' }}>
        {/*
          `titleAccess` gives the <svg> its own accessible name, so the type is
          announced rather than being a decorative glyph whose only label is a
          hover tooltip.
        */}
        <IconComponent fontSize="small" titleAccess={label} sx={{ color: eventTypeColor(resolved) }} />
      </Box>
    </Tooltip>
  );
}

/**
 * A status code, red from 400 up.
 *
 * `null` renders as a dash. Note what this does NOT do: it reads the row's own
 * `status_code`, so an event that carries none says so. The admin Users
 * reference page's status chip read a column that does not exist and therefore
 * rendered one constant for every row — the reason every field on this page was
 * checked against the real table before being rendered.
 */
export function StatusCodeCell({ statusCode }: { readonly statusCode: number | null }): ReactNode {
  if (statusCode === null) {
    return (
      <Typography variant="bodyMedium" color="text.secondary">
        —
      </Typography>
    );
  }
  return (
    <Typography variant="bodyMedium" color={statusCode >= 400 ? 'error' : 'text.secondary'}>
      {statusCode}
    </Typography>
  );
}

export interface SortableHeaderCellProps {
  readonly field: string;
  readonly label: string;
  readonly sortField: string;
  readonly sortDirection: 'asc' | 'desc';
  readonly onSort: (field: string) => void;
}

/**
 * A sortable column header. It reports a click and renders the caller's current
 * sort; WHERE the sort happens is the caller's business.
 *
 * For the audit tables it is SERVER-side, and has to be: they show one page of a
 * table with millions of rows, so a client-side comparator would only ever order
 * the 50 rows already fetched while claiming to order all of them. The Schedules
 * table (unit A14) reuses this header for a client-side sort instead, which is
 * honest there because its endpoint is unpaginated and returns every row.
 */
export function SortableHeaderCell({
  field,
  label,
  sortField,
  sortDirection,
  onSort,
}: SortableHeaderCellProps): ReactNode {
  const active = sortField === field;
  return (
    <TableCell sortDirection={active ? sortDirection : false}>
      <TableSortLabel
        active={active}
        direction={active ? sortDirection : 'desc'}
        onClick={() => onSort(field)}
      >
        {label}
      </TableSortLabel>
    </TableCell>
  );
}

/** A plain, ellipsised text cell. */
export function TextCell({ children }: { readonly children: ReactNode }): ReactNode {
  return (
    <Typography
      variant="bodyMedium"
      color="text.secondary"
      sx={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
    >
      {children}
    </Typography>
  );
}
