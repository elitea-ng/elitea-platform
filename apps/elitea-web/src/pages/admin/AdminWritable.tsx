/**
 * The read-only wrapper and its two parts (ADR-0059 decision 6; R3.4a,
 * issue 2534). `./adminWriteAccess.ts` holds the level and the reasons.
 *
 *  - `AdminWritable itemId` wraps a page. Every control under it reads the
 *    item's level through `useAdminWriteControl`.
 *  - `AdminViewOnlyNotice` is the "View only" notice, placed by the page
 *    under its title. It renders at `read` only, and names the roles that
 *    can change the page, from `GET /admin/console/items/{item_id}` (R3.1a):
 *    the server's own answer, by role, never by person.
 *  - `AdminWriteControl` is one icon control: disabled at `read`, with the
 *    reason as its tooltip.
 */
import type { ReactElement, ReactNode } from 'react';
import { useContext, useMemo } from 'react';

import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Tooltip from '@mui/material/Tooltip';

import { useGetAdminConsoleItem } from '@/shared/api/generated/admin/admin';
import { t } from '@/shared/i18n';

import {
  AdminWriteScopeContext,
  consoleRoleName,
  useAdminItemAccess,
  useAdminWriteControl,
  type AdminWriteScope,
} from './adminWriteAccess';

export interface AdminWritableProps {
  /** The server's console item id the page belongs to. */
  readonly itemId: string;
  readonly children: ReactNode;
}

export function AdminWritable({ itemId, children }: AdminWritableProps): ReactNode {
  const access = useAdminItemAccess(itemId);
  const scope = useMemo((): AdminWriteScope => ({ itemId, access }), [itemId, access]);
  return <AdminWriteScopeContext value={scope}>{children}</AdminWriteScopeContext>;
}

/** The sentence that names who can change the page. */
function WritingRoles({ itemId }: { readonly itemId: string }): ReactNode {
  const query = useGetAdminConsoleItem(itemId, {
    query: {
      retry: false,
      staleTime: 60_000,
      select: (response) =>
        response.status === 200
          ? response.data.granting_roles.filter((role) => role.level === 'write').map(consoleRoleName)
          : undefined,
    },
  });
  const roles = query.data;
  // Pending or failed: say what the page is, not who can change it. A failed
  // read is never "no role can change it".
  if (roles === undefined) {
    return t('pages.admin.viewOnly.unnamed', 'You can see this page but not change it.');
  }
  if (roles.length === 0) {
    return t(
      'pages.admin.viewOnly.noRole',
      'You can see this page but not change it. No role on this deployment can change it.',
    );
  }
  return (
    <span data-testid="admin-view-only-roles">
      {t(
        'pages.admin.viewOnly.roles',
        'You can see this page but not change it. These roles can change it: {{roles}}.',
        {
          roles: roles.join(', '),
        },
      )}
    </span>
  );
}

/** The "View only" notice; nothing unless the page's item is at `read`. */
export function AdminViewOnlyNotice(): ReactNode {
  const scope = useContext(AdminWriteScopeContext);
  if (scope?.access !== 'read') return null;
  return (
    <Alert severity="info" data-testid="admin-view-only" data-item-id={scope.itemId}>
      <AlertTitle>{t('pages.admin.viewOnly.title', 'View only')}</AlertTitle>
      <WritingRoles itemId={scope.itemId} />
    </Alert>
  );
}

export interface AdminWriteControlProps {
  /** The control's name, its tooltip while enabled. */
  readonly label: string;
  /** The control, given whether it is disabled. Give it `aria-label={label}`. */
  readonly children: (disabled: boolean) => ReactElement;
}

/**
 * One mutation control. A disabled button fires no pointer events, so the
 * tooltip sits on a wrapping span, in both states: one element tree means a
 * level that changes under a focused control (a refetch on focus, after a
 * 403) does not remount it. The disabled control is not focusable, so the
 * tooltip is a pointer convenience; the page's "View only" notice is the
 * explanation every reader gets.
 */
export function AdminWriteControl({ label, children }: AdminWriteControlProps): ReactNode {
  const { disabled, reason } = useAdminWriteControl();
  return (
    <Tooltip title={disabled ? reason : label}>
      <span>{children(disabled)}</span>
    </Tooltip>
  );
}
