/**
 * The dangerous actions of Admin › Users (ADR-0059 decision 6; R3.8b, issue
 * 2614): delete, suspend and an administration role change wait in one
 * pending slot for `AdminDangerousActionDialog`, which `Users.tsx` renders,
 * and go out with its reason. An unsuspend is not dangerous and is sent at
 * once.
 */
import { useCallback, useState } from 'react';

import { t } from '@/shared/i18n';

import type { AdminDangerousActionDialogProps } from './AdminDangerousActionDialog';
import type { AdminReason } from './adminDangerousAction';
import { ADMIN_ITEM } from './adminItemIds';
import { useAdminWritable } from './adminWriteAccess';
import { permissionMatrixFailureReason } from './api/adminRolesApi';
import {
  useDeleteAdminUsers,
  useSetAdminRoles,
  useSuspendAdminUser,
  type AdminUserRow,
} from './api/adminUsersApi';

type PendingUserAction =
  | { readonly kind: 'delete'; readonly ids: readonly number[] }
  | { readonly kind: 'roles'; readonly userId: number; readonly roleNames: readonly string[] }
  | { readonly kind: 'suspend'; readonly user: AdminUserRow };

/** The page-specific props of the dialog for a pending action. */
export type AdminUserDangerousDialog = Pick<
  AdminDangerousActionDialogProps,
  'action' | 'title' | 'change' | 'reach' | 'target' | 'confirmLabel'
>;

function emailOf(rows: readonly AdminUserRow[], id: number): string {
  return rows.find((row) => row.id === id)?.email ?? String(id);
}

function dialogFor(pending: PendingUserAction, rows: readonly AdminUserRow[]): AdminUserDangerousDialog {
  switch (pending.kind) {
    case 'delete':
      return {
        action: 'POST /api/v2/admin/auth_users/{mode} delete',
        title: t('pages.admin.users.deleteModal.title', 'Delete confirmation'),
        change: t(
          'pages.admin.users.dangerous.deleteChange',
          'The selected people are removed from the platform with their memberships. There is no restore.',
        ),
        reach: { count: pending.ids.length, unit: 'people' },
        target: pending.ids.length === 1 ? emailOf(rows, pending.ids[0] ?? 0) : `${pending.ids.length} users`,
        confirmLabel: t('pages.admin.users.dangerous.deleteConfirm', 'Delete'),
      };
    case 'roles':
      return {
        action: 'POST /api/v2/admin/auth_users/{mode} set_admin_role',
        title: t('pages.admin.users.dangerous.rolesTitle', 'Change administration roles'),
        change: t(
          'pages.admin.users.dangerous.rolesChange',
          'The person holds exactly these administration roles afterwards: {{roles}}.',
          { roles: pending.roleNames.length === 0 ? '—' : pending.roleNames.join(', ') },
        ),
        reach: { count: 1, unit: 'people' },
        target: emailOf(rows, pending.userId),
        confirmLabel: t('pages.admin.users.dangerous.rolesConfirm', 'Change roles'),
      };
    case 'suspend':
      return {
        action: 'PUT /api/v2/admin/user_suspend/{mode}/{userID} suspend',
        title: t('pages.admin.users.dangerous.suspendTitle', 'Suspend user'),
        change: t(
          'pages.admin.users.dangerous.suspendChange',
          'The person can no longer sign in, and their sessions and tokens stop working until they are unsuspended.',
        ),
        reach: { count: 1, unit: 'people' },
        target: pending.user.email,
        confirmLabel: t('pages.admin.users.dangerous.suspendConfirm', 'Suspend'),
      };
  }
}

interface Callbacks {
  readonly rows: readonly AdminUserRow[];
  readonly onError: (message: string) => void;
  readonly onDeleted: (ids: readonly number[]) => void;
}

function message(error: unknown, fallback: string): string {
  return error instanceof Error && error.message ? error.message : fallback;
}

export function useAdminUserDangerousActions({ rows, onError, onDeleted }: Callbacks) {
  const access = useAdminWritable(ADMIN_ITEM.users);
  const deleteUsers = access.guard(useDeleteAdminUsers());
  const setAdminRole = access.guard(useSetAdminRoles());
  const suspendUser = access.guard(useSuspendAdminUser());
  const [pending, setPending] = useState<PendingUserAction | undefined>(undefined);

  const onConfirm = useCallback(
    (reason: AdminReason) => {
      const action = pending;
      setPending(undefined);
      if (action?.kind === 'delete') {
        deleteUsers.mutate(
          { userIds: action.ids, reason },
          {
            onSuccess: () => onDeleted(action.ids),
            onError: (error) =>
              onError(message(error, t('pages.admin.users.error.delete', 'Failed to delete the selected users.'))),
          },
        );
      } else if (action?.kind === 'roles') {
        // The no-escalation refusals carry a stable code (role_self_assignment,
        // role_permission_not_held, …); the page says what it means.
        setAdminRole.mutate(
          { userId: action.userId, roleNames: action.roleNames, reason },
          {
            onError: (error) =>
              onError(
                permissionMatrixFailureReason(error) ??
                  t('pages.admin.users.error.roles', 'Failed to change the administration roles.'),
              ),
          },
        );
      } else if (action?.kind === 'suspend') {
        suspendUser.mutate(
          { userId: action.user.id, suspended: true, reason },
          {
            onError: (error) =>
              onError(message(error, t('pages.admin.users.error.suspend', 'Failed to change the suspended state.'))),
          },
        );
      }
    },
    [pending, deleteUsers, setAdminRole, suspendUser, onError, onDeleted],
  );

  const onToggleSuspended = useCallback(
    (user: AdminUserRow) => {
      if (!user.suspended) {
        setPending({ kind: 'suspend', user });
        return;
      }
      suspendUser.mutate(
        { userId: user.id, suspended: false },
        {
          onError: (error) =>
            onError(message(error, t('pages.admin.users.error.suspend', 'Failed to change the suspended state.'))),
        },
      );
    },
    [suspendUser, onError],
  );

  return {
    deleteUsers,
    setAdminRole,
    suspendUser,
    dialog: pending === undefined ? undefined : dialogFor(pending, rows),
    onRequestDelete: useCallback((ids: readonly number[]) => setPending({ kind: 'delete', ids }), []),
    onRequestRoles: useCallback(
      (userId: number, roleNames: readonly string[]) => setPending({ kind: 'roles', userId, roleNames }),
      [],
    ),
    onToggleSuspended,
    onCancel: useCallback(() => setPending(undefined), []),
    onConfirm,
  };
}
