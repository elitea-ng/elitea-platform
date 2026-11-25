/**
 * Admin › People and access › Access (ADR-0059 decision 6, the Access item;
 * R3.6 and R3.9a, issues 2537 and 2540).
 *
 * What each administration role can do, read from
 * `GET /admin/access/role-definitions`: the console items its set opens, each
 * at read or write, its seed status, its permission count, and whether the
 * caller may give it to another person. All of it is the server's answer; the
 * page re-implements no role rule. Every control is read-only here: the
 * assignment, the administrator listing, the comparison and the tokens are
 * the later Access stories (R3.9b, issue 2615; R3.9c, issue 2616).
 *
 * The raw permission matrix stays at `/roles`, linked for a holder of the
 * matrix permission only (ADR-0059 decision 6: "the raw matrix as a tab").
 * The security auditor reads this page through `access.roles.view` and has
 * no matrix.
 */
import type { ReactNode } from 'react';

import { Link } from '@tanstack/react-router';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import LinearProgress from '@mui/material/LinearProgress';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { useGetAccessRoleDefinitions } from '@/shared/api/generated/admin/admin';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { AccessRoleDefinition } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { consoleItemLabel, refusalLabel, roleStatusLabel } from './accessCopy';
import { adminRoleLabel } from './adminRoleCopy';
import { adminUiPermissionsKnown, adminUiShowsControlFor } from './adminUiConfig';
import { AdminViewOnlyNotice, AdminWritable } from './AdminWritable';
import { ADMIN_ITEM } from './adminItemIds';

/** The permission that reads the matrix of `/roles`. */
const MATRIX_VIEW = 'configuration.roles.permissions.view';

/**
 * Whether to link the matrix: presentation only, as the server gates the
 * matrix itself. When the injected list is not the caller's, the link shows
 * and the server decides.
 */
function showsMatrixLink(): boolean {
  return !adminUiPermissionsKnown() || adminUiShowsControlFor(MATRIX_VIEW);
}

function RoleOpens({ role }: { readonly role: AccessRoleDefinition }): ReactNode {
  if (role.areas.length === 0) {
    return (
      <Typography variant="bodySmall" color="text.secondary">
        {t('pages.admin.access.opensNothing', 'Opens nothing in the console')}
      </Typography>
    );
  }
  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: '0.25rem' }}>
      {role.areas.flatMap((area) =>
        area.items.map((entry) => (
          <Chip
            key={entry.id}
            size="small"
            variant={entry.level === 'write' ? 'filled' : 'outlined'}
            label={
              entry.level === 'write'
                ? t('pages.admin.access.levelWrite', '{{item}}: change', { item: consoleItemLabel(entry.id) })
                : t('pages.admin.access.levelRead', '{{item}}: view', { item: consoleItemLabel(entry.id) })
            }
            data-testid={`admin-access-item-${role.name}-${entry.id}`}
            data-level={entry.level}
          />
        )),
      )}
    </Box>
  );
}

function RoleRow({ role }: { readonly role: AccessRoleDefinition }): ReactNode {
  const label = adminRoleLabel(role.name, {
    name: role.name,
    builtIn: role.built_in,
    marker: null,
    displayName: role.display_name,
    holdsPermissions: role.permission_count > 0,
  });
  return (
    <TableRow data-testid={`admin-access-role-${role.name}`}>
      <TableCell>
        <Typography variant="bodyMedium">{label}</Typography>
        {role.held_by_caller ? (
          <Typography variant="bodySmall" color="text.secondary">
            {t('pages.admin.access.yours', 'You hold this role')}
          </Typography>
        ) : null}
      </TableCell>
      <TableCell>{roleStatusLabel(role.status)}</TableCell>
      <TableCell>{role.permission_count}</TableCell>
      <TableCell>
        <RoleOpens role={role} />
      </TableCell>
      <TableCell data-testid={`admin-access-assignable-${role.name}`} data-assignable={role.assignable_by_caller}>
        {role.assignable_by_caller
          ? t('pages.admin.access.assignable', 'You can assign it')
          : refusalLabel(role.refusal_code)}
      </TableCell>
    </TableRow>
  );
}

function RolesTable({ roles }: { readonly roles: readonly AccessRoleDefinition[] }): ReactNode {
  return (
    <Table size="small" aria-label={t('pages.admin.access.table.label', 'Administration roles')}>
      <TableHead>
        <TableRow>
          <TableCell>{t('pages.admin.access.table.role', 'Role')}</TableCell>
          <TableCell>{t('pages.admin.access.table.status', 'Kind')}</TableCell>
          <TableCell>{t('pages.admin.access.table.permissions', 'Permissions')}</TableCell>
          <TableCell>{t('pages.admin.access.table.opens', 'What it opens')}</TableCell>
          <TableCell>{t('pages.admin.access.table.assign', 'Assignment')}</TableCell>
        </TableRow>
      </TableHead>
      <TableBody>
        {roles.map((role) => (
          <RoleRow key={role.name} role={role} />
        ))}
      </TableBody>
    </Table>
  );
}

function isRefusal(error: unknown): boolean {
  return error instanceof EliteaApiError && error.failure.kind === 'http' && error.failure.status === 403;
}

function RoleDefinitions(): ReactNode {
  const query = useGetAccessRoleDefinitions({
    query: { select: (response) => (response.status === 200 ? response.data.roles : undefined) },
  });
  if (query.isPending) return <LinearProgress />;
  if (isRefusal(query.error)) {
    // The Access item opens for the matrix names too, while the definitions
    // read needs access.roles.view (or the names it narrows): the page then
    // offers the matrix alone (issue 2540).
    return (
      <Alert severity="info" data-testid="admin-access-definitions-refused">
        {t(
          'pages.admin.access.refused',
          'Your roles do not include reading the role definitions. The permission matrix is above.',
        )}
      </Alert>
    );
  }
  if (query.data === undefined) {
    return (
      <Alert severity="warning" data-testid="admin-access-error">
        {t('pages.admin.access.error', 'The administration roles could not be read.')}
      </Alert>
    );
  }
  return <RolesTable roles={query.data} />;
}

export function AdminAccess(): ReactNode {
  return (
    <AdminWritable itemId={ADMIN_ITEM.access}>
      <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }} data-testid="admin-access-page">
        <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '1rem' }}>
          <Typography variant="headingLarge" component="h1">
            {t('pages.admin.nav.access', 'Access')}
          </Typography>
          {showsMatrixLink() ? (
            <Button
              component={Link}
              to="/roles"
              size="small"
              sx={{ textTransform: 'none' }}
              data-testid="admin-access-matrix-link"
            >
              {t('pages.admin.access.matrix', 'Permission matrix')}
            </Button>
          ) : null}
        </Box>
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.admin.access.subtitle',
            'What each administration role can do, by console page, and whether you can give it to another person.',
          )}
        </Typography>
        <AdminViewOnlyNotice />
        <RoleDefinitions />
      </DrawerPage>
    </AdminWritable>
  );
}
