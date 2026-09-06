/**
 * The permission matrix of the admin Roles page (unit A14).
 *
 * Reference (read-only):
 * `frontends/admin_ui/frontend/src/pages/RolesPage/PermissionMatrix.jsx`.
 * Same behaviour — permissions grouped by their first two dotted segments,
 * groups collapsed by default and auto-expanded by a search, a tri-state group
 * checkbox per role — rewritten as a real table (see `./PermissionMatrixRows`).
 *
 * Edits are reported UP as a row transform, never applied here: the draft lives
 * in `useAdminRolesPage`, which is also what decides whether it is dirty.
 *
 * The expansion state is stored WITH the search it belongs to rather than being
 * re-seeded by an effect. The reference's effect re-expanded every match on any
 * render while a search was active, so collapsing a group during a search did
 * nothing at all — a control that silently no-ops.
 */
import { Fragment, memo, useCallback, useMemo, useState } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import EditOutlinedIcon from '@mui/icons-material/EditOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { PermissionMatrixRow } from './api/adminRolesApi';
import { PermissionCellRow, PermissionGroupRow, humaniseRole } from './PermissionMatrixRows';

export interface PermissionMatrixProps {
  readonly rows: readonly PermissionMatrixRow[];
  readonly roles: readonly string[];
  readonly search: string;
  readonly readOnly: boolean;
  readonly onChange: (updater: (rows: readonly PermissionMatrixRow[]) => PermissionMatrixRow[]) => void;
  /**
   * Role DEFINITION controls, per column (gap G9). Returning `undefined` for a
   * role means the control is not offered for it — a built-in role, or an
   * operator who is not shown the permission. The server refuses either way;
   * these two exist so a control does not appear to work and then 409.
   */
  readonly renameHandlerFor?: (role: string) => (() => void) | undefined;
  readonly deleteHandlerFor?: (role: string) => (() => void) | undefined;
}

type PermissionGroup = readonly [string, PermissionMatrixRow[]];

/** Expansion, remembered against the search term it was chosen under. */
interface ExpansionState {
  readonly search: string;
  readonly groups: ReadonlySet<string>;
}

/** `models.admin.audit_trail.view` → `models.admin`. */
function groupKeyOf(permission: string): string {
  const parts = permission.split('.');
  return parts.length >= 2 ? `${parts[0]}.${parts[1]}` : permission;
}

function groupRows(rows: readonly PermissionMatrixRow[], search: string): PermissionGroup[] {
  const needle = search.trim().toLowerCase();
  const groups = new Map<string, PermissionMatrixRow[]>();
  for (const row of rows) {
    if (needle && !row.name.toLowerCase().includes(needle)) continue;
    const key = groupKeyOf(row.name);
    const bucket = groups.get(key);
    if (bucket) bucket.push(row);
    else groups.set(key, [row]);
  }
  return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b));
}

/**
 * A NEW search expands every group it matched; anything else keeps whatever the
 * operator last chose. Clearing the search keeps it too, so the groups they were
 * reading do not slam shut.
 */
function resolveExpanded(
  state: ExpansionState,
  search: string,
  grouped: readonly PermissionGroup[],
): ReadonlySet<string> {
  if (state.search === search || search.trim() === '') return state.groups;
  return new Set(grouped.map(([groupName]) => groupName));
}

export const PermissionMatrix = memo(function PermissionMatrix({
  rows,
  roles,
  search,
  readOnly,
  onChange,
  renameHandlerFor,
  deleteHandlerFor,
}: PermissionMatrixProps) {
  const [expansion, setExpansion] = useState<ExpansionState>({ search: '', groups: new Set() });

  const grouped = useMemo(() => groupRows(rows, search), [rows, search]);
  const expanded = useMemo(
    () => resolveExpanded(expansion, search, grouped),
    [expansion, search, grouped],
  );

  const onToggleExpand = useCallback(
    (groupName: string) => {
      const next = new Set(expanded);
      if (next.has(groupName)) next.delete(groupName);
      else next.add(groupName);
      setExpansion({ search, groups: next });
    },
    [expanded, search],
  );

  const onToggle = useCallback(
    (permissionName: string, role: string, value: boolean) => {
      onChange((current) =>
        current.map((row) => (row.name === permissionName ? { ...row, [role]: value } : row)),
      );
    },
    [onChange],
  );

  const onToggleGroupRole = useCallback(
    (groupName: string, role: string, value: boolean) => {
      onChange((current) =>
        current.map((row) => (groupKeyOf(row.name) === groupName ? { ...row, [role]: value } : row)),
      );
    },
    [onChange],
  );

  const expandAll = useCallback(
    () => setExpansion({ search, groups: new Set(grouped.map(([groupName]) => groupName)) }),
    [grouped, search],
  );
  const collapseAll = useCallback(() => setExpansion({ search, groups: new Set() }), [search]);

  if (grouped.length === 0) {
    return (
      <Box sx={{ padding: '2rem', textAlign: 'center' }}>
        <Typography variant="bodyMedium" color="text.secondary">
          {t('pages.admin.roles.empty', 'No permissions match the search.')}
        </Typography>
      </Box>
    );
  }

  return (
    <TableContainer sx={{ flex: 1, minHeight: 0, overflowX: 'auto' }}>
      <Table size="small" stickyHeader aria-label={t('pages.admin.roles.table', 'Permission matrix')}>
        <TableHead>
          <TableRow>
            <TableCell>
              <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
                <Typography variant="labelMedium">
                  {t('pages.admin.roles.column.permission', 'Permission')}
                </Typography>
                <Button size="small" variant="text" onClick={expandAll}>
                  {t('pages.admin.roles.action.expandAll', 'Expand all')}
                </Button>
                <Button size="small" variant="text" onClick={collapseAll}>
                  {t('pages.admin.roles.action.collapseAll', 'Collapse all')}
                </Button>
              </Box>
            </TableCell>
            {roles.map((role) => {
              const onRename = renameHandlerFor?.(role);
              const onDelete = deleteHandlerFor?.(role);
              return (
                <TableCell key={role} align="center">
                  <Box
                    sx={{
                      display: 'flex',
                      flexDirection: 'column',
                      alignItems: 'center',
                      gap: '0.125rem',
                    }}
                  >
                    <Typography variant="labelMedium" sx={{ textTransform: 'capitalize' }}>
                      {humaniseRole(role)}
                    </Typography>
                    {onRename || onDelete ? (
                      <Box sx={{ display: 'flex', gap: '0.125rem' }}>
                        {onRename ? (
                          <IconButton
                            size="small"
                            aria-label={t('pages.admin.roles.action.renameRole', 'Rename role: {{role}}', {
                              role,
                            })}
                            onClick={onRename}
                          >
                            <EditOutlinedIcon fontSize="inherit" />
                          </IconButton>
                        ) : null}
                        {onDelete ? (
                          <IconButton
                            size="small"
                            aria-label={t('pages.admin.roles.action.deleteRole', 'Delete role: {{role}}', {
                              role,
                            })}
                            onClick={onDelete}
                          >
                            <DeleteOutlinedIcon fontSize="inherit" />
                          </IconButton>
                        ) : null}
                      </Box>
                    ) : null}
                  </Box>
                </TableCell>
              );
            })}
          </TableRow>
        </TableHead>
        <TableBody>
          {grouped.map(([groupName, permissions]) => (
            <Fragment key={groupName}>
              <PermissionGroupRow
                groupName={groupName}
                permissions={permissions}
                roles={roles}
                expanded={expanded.has(groupName)}
                onToggleExpand={onToggleExpand}
                onToggleGroupRole={onToggleGroupRole}
                readOnly={readOnly}
              />
              {expanded.has(groupName)
                ? permissions.map((permission) => (
                    <PermissionCellRow
                      key={permission.name}
                      permission={permission}
                      roles={roles}
                      onToggle={onToggle}
                      readOnly={readOnly}
                    />
                  ))
                : null}
            </Fragment>
          ))}
        </TableBody>
      </Table>
    </TableContainer>
  );
});
