/**
 * Row shaping for the Admin › Users export. Quoting, the formula guard, the
 * BOM and the paged walk all live in `./adminCsv` — shared with the Projects
 * export so the two files cannot drift into two different CSV dialects.
 *
 * The columns and their values are the reference page's `EXPORT_COLUMNS`
 * (`frontends/admin_ui/.../UsersPage.jsx`); only the container format differs.
 */
import { t } from '@/shared/i18n';

import { toCsvDocument } from './adminCsv';
import { adminRoleLabel } from './adminRoleCopy';
import type { AdminUserRow } from './api/adminUsersApi';

/** Every administration role held (ADR-0059 R0.8c), by display name. */
function roleLabel(row: AdminUserRow): string {
  if (row.admin_roles.length === 0) return t('pages.admin.users.role.none', 'None');
  return row.admin_roles.map((name) => adminRoleLabel(name)).join('; ');
}

/**
 * `rows` → a CSV document, header included.
 *
 * `Status` and `Administration roles` are the readings the table makes. `Last login` is
 * NOT: the table renders `new Date(value).toLocaleString()` in the viewer's
 * timezone (`AdminUsersTable.formatLastLogin`), while this writes the server's
 * own string unchanged. That is deliberate — a spreadsheet column wants a
 * sortable, unambiguous timestamp rather than one already rewritten into
 * whichever locale happened to click the button — but it does mean the file
 * and the screen show that one column differently.
 */
export function buildAdminUsersCsv(rows: readonly AdminUserRow[]): string {
  const header = [
    t('pages.admin.users.column.name', 'Name'),
    t('pages.admin.users.column.email', 'Email'),
    t('pages.admin.users.column.lastLogin', 'Last login'),
    t('pages.admin.users.column.status', 'Status'),
    t('pages.admin.users.column.adminRoles', 'Administration roles'),
  ];
  return toCsvDocument(
    header,
    rows.map((row) => [
      row.name,
      row.email,
      row.last_login ?? t('pages.admin.users.neverLoggedIn', 'Never'),
      row.suspended
        ? t('pages.admin.users.status.suspended', 'Suspended')
        : t('pages.admin.users.status.active', 'Active'),
      roleLabel(row),
    ]),
  );
}
