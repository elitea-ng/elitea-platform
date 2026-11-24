/**
 * The two row kinds of the admin Roles matrix (unit A14).
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/pages/RolesPage/`
 * `PermissionGroupRow.jsx` and `PermissionRow.jsx`. Rewritten against this
 * app's stack: `BaseCheckbox` instead of MUI's raw `Checkbox`, rem spacing, and
 * a real `<tr>`/`<td>` structure instead of a CSS grid of `<div>`s — a matrix
 * of checkboxes with row and column headers is a table, and screen readers need
 * it announced as one.
 *
 * ## `system` is disabled here AND refused by the server
 *
 * `internal/api/v2/admin/roles.go` drops every `system` cell from a submitted
 * body. Disabling the column is a courtesy so the control does not appear to
 * work; it is not the gate.
 */
import { memo, useMemo } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import TableCell from '@mui/material/TableCell';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { t } from '@/shared/i18n';

import type { PermissionMatrixRow } from './api/adminRolesApi';
import { isLockedGrant, type DeprecatedGrants } from './deprecatedGrants';

/** The one role the server will not write, whatever the body says. */
const READ_ONLY_ROLE = 'system';

/** A role column's aggregate state across a group's permissions. */
type Aggregate = 'none' | 'some' | 'all';

export function humaniseRole(role: string): string {
  return role.replace(/_/g, ' ');
}

/** `models.admin.audit_trail.view` → `audit_trail.view`; the group carries the rest. */
function shortPermissionName(permission: string): string {
  const parts = permission.split('.');
  return parts.length > 2 ? parts.slice(2).join('.') : permission;
}

export interface PermissionGroupRowProps {
  readonly groupName: string;
  readonly permissions: readonly PermissionMatrixRow[];
  readonly roles: readonly string[];
  readonly expanded: boolean;
  readonly onToggleExpand: (groupName: string) => void;
  readonly onToggleGroupRole: (groupName: string, role: string, value: boolean) => void;
  /** Deprecated names and their holders: a cell a role cannot be given is left out of its aggregate. */
  readonly deprecated?: DeprecatedGrants | undefined;
  readonly readOnly: boolean;
}

export const PermissionGroupRow = memo(function PermissionGroupRow({
  groupName,
  permissions,
  roles,
  expanded,
  onToggleExpand,
  onToggleGroupRole,
  deprecated,
  readOnly,
}: PermissionGroupRowProps) {
  const aggregates = useMemo(() => {
    const result: Record<string, Aggregate> = {};
    for (const role of roles) {
      const open = permissions.filter((permission) => !isLockedGrant(deprecated, permission.name, role));
      const checked = open.filter((permission) => permission[role] === true).length;
      result[role] = checked === 0 ? 'none' : checked === open.length ? 'all' : 'some';
    }
    return result;
  }, [roles, permissions, deprecated]);

  const expandLabel = expanded
    ? t('pages.admin.roles.action.collapseGroup', 'Collapse permission group')
    : t('pages.admin.roles.action.expandGroup', 'Expand permission group');

  return (
    <TableRow data-testid="admin-roles-group-row" sx={{ backgroundColor: 'background.paper' }}>
      <TableCell sx={{ paddingY: '0.25rem' }}>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.25rem' }}>
          <IconButton
            size="small"
            aria-label={`${expandLabel}: ${groupName}`}
            aria-expanded={expanded}
            onClick={() => onToggleExpand(groupName)}
          >
            {expanded ? <ExpandMoreIcon fontSize="small" /> : <ChevronRightIcon fontSize="small" />}
          </IconButton>
          <Typography variant="headingSmall" component="span">
            {groupName}
          </Typography>
          <Typography variant="bodySmall" color="text.secondary">
            {`(${permissions.length})`}
          </Typography>
        </Box>
      </TableCell>
      {roles.map((role) => (
        <TableCell key={role} align="center" sx={{ paddingY: '0.25rem' }}>
          <BaseCheckbox
            size="small"
            checked={aggregates[role] === 'all'}
            indeterminate={aggregates[role] === 'some'}
            disabled={readOnly || role === READ_ONLY_ROLE}
            aria-label={`${humaniseRole(role)}: ${groupName}`}
            onChange={() => onToggleGroupRole(groupName, role, aggregates[role] !== 'all')}
          />
        </TableCell>
      ))}
    </TableRow>
  );
});

export interface PermissionCellRowProps {
  readonly permission: PermissionMatrixRow;
  readonly roles: readonly string[];
  /**
   * Set when the server marks the name deprecated (ADR-0059 R0.14a): the roles
   * that hold it on the server. It keeps working where it is granted until its
   * sunset date, and a save that adds it to any other role is refused with
   * `permission_deprecated`, so that role's checkbox is disabled.
   */
  readonly deprecatedHolders?: ReadonlySet<string> | undefined;
  readonly onToggle: (permissionName: string, role: string, value: boolean) => void;
  readonly readOnly: boolean;
}

export const PermissionCellRow = memo(function PermissionCellRow({
  permission,
  roles,
  deprecatedHolders,
  onToggle,
  readOnly,
}: PermissionCellRowProps) {
  return (
    <TableRow data-testid="admin-roles-permission-row" hover>
      <TableCell sx={{ paddingY: '0.125rem', paddingLeft: '2.5rem' }}>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
          <Typography variant="bodySmall" color="text.secondary">
            {shortPermissionName(permission.name)}
          </Typography>
          {deprecatedHolders ? (
            <Chip
              size="small"
              variant="outlined"
              color="warning"
              data-testid="admin-roles-deprecated-mark"
              label={t('pages.admin.roles.deprecated', 'Deprecated')}
              title={t(
                'pages.admin.roles.deprecatedHint',
                'Ends on a sunset date. Existing grants keep working; it can no longer be added to a role.',
              )}
            />
          ) : null}
        </Box>
      </TableCell>
      {roles.map((role) => (
        <TableCell key={role} align="center" sx={{ paddingY: '0.125rem' }}>
          <BaseCheckbox
            size="small"
            checked={permission[role] === true}
            disabled={
              readOnly || role === READ_ONLY_ROLE || (deprecatedHolders !== undefined && !deprecatedHolders.has(role))
            }
            aria-label={`${humaniseRole(role)}: ${permission.name}`}
            onChange={() => onToggle(permission.name, role, permission[role] !== true)}
          />
        </TableCell>
      ))}
    </TableRow>
  );
});
