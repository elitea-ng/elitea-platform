/**
 * Create / rename / delete state for the admin Roles page (gap G9).
 *
 * Split out of `useAdminRolesPage` rather than folded into it, for the reason
 * that hook was split out of `Roles.tsx`: these three writes change WHICH
 * COLUMNS the matrix has, while everything in the parent hook is about the
 * cells. They are different surfaces that share a target, and keeping them
 * apart is what stops one from being changed while reasoning about the other.
 *
 * ## What is authorisation and what is not
 *
 * Every gate here is PRESENTATION. `internal/api/v2/admin/roles_crud.go`
 * validates the name, refuses every built-in role as a rename or delete target,
 * and refuses a delete while the role still holds members — each of those
 * independently of anything in this file. `window.admin_ui_config.permissions`
 * decides what is SHOWN and never what is allowed (`./adminUiConfig`).
 *
 * ## `onWritten` is called after the refetch, not after the request
 *
 * The three mutations return their `invalidateQueries` promise from `onSuccess`
 * (`./api/adminRolesApi`), so TanStack Query does not settle the mutation until
 * the matrix has been re-read. `onWritten` therefore runs when `serverRows`
 * already carries the new column set, which is what lets the parent re-seed its
 * draft from fresh data instead of from the matrix that still lacks it.
 */
import { useCallback, useState } from 'react';

import { adminUiShowsControlFor } from './adminUiConfig';
import { isBuiltInRole, type RoleNameDialogMode } from './AdminRoleDialogs';
import {
  permissionMatrixFailureReason,
  useCreateRole,
  useDeleteRole,
  useRenameRole,
  type PermissionMatrixTarget,
} from './api/adminRolesApi';

/**
 * The three permissions the role-definition routes carry
 * (`internal/api/router.go`, migrations/shared/0111). They are separate from
 * `configuration.roles.permissions.edit`, which governs the matrix: moving a
 * checkbox and deleting the role the checkbox belongs to are different
 * privileges, and pylon separates them too.
 */
const PERMISSION_ROLE_CREATE = 'configuration.roles.roles.create';
const PERMISSION_ROLE_EDIT = 'configuration.roles.roles.edit';
const PERMISSION_ROLE_DELETE = 'configuration.roles.roles.delete';

interface RoleDialogState {
  readonly mode: RoleNameDialogMode;
  /** The role being renamed. Empty for a create. */
  readonly role: string;
}

export interface AdminRoleDefinitionsState {
  /** `undefined` ⇒ the "New role" control is not offered. */
  readonly onCreateRole: (() => void) | undefined;
  /**
   * `undefined` ⇒ no rename control for this role — either the operator is not
   * shown the permission, or the role is built in and the SERVER refuses it.
   */
  readonly renameHandlerFor: (role: string) => (() => void) | undefined;
  readonly deleteHandlerFor: (role: string) => (() => void) | undefined;

  readonly roleDialog: RoleDialogState | null;
  /** The role the delete dialog is armed for, or `null`. */
  readonly roleToDelete: string | null;
  readonly isWritingRole: boolean;
  /** The server's own words for the last refused write. */
  readonly roleWriteFailure: string | undefined;

  readonly onCloseRoleDialog: () => void;
  readonly onSubmitRoleDialog: (name: string) => void;
  readonly onConfirmDeleteRole: () => void;
}

export function useAdminRoleDefinitions(
  target: PermissionMatrixTarget,
  /** Called once per successful write, AFTER the matrix refetch has settled. */
  onWritten: (message: string) => void,
): AdminRoleDefinitionsState {
  const [roleDialog, setRoleDialog] = useState<RoleDialogState | null>(null);
  const [roleToDelete, setRoleToDelete] = useState<string | null>(null);
  const [roleWriteFailure, setRoleWriteFailure] = useState<string | undefined>(undefined);

  const createRole = useCreateRole();
  const renameRole = useRenameRole();
  const deleteRole = useDeleteRole();

  const onCloseRoleDialog = useCallback(() => {
    setRoleDialog(null);
    setRoleToDelete(null);
    setRoleWriteFailure(undefined);
  }, []);

  const succeeded = useCallback(
    (message: string) => {
      onWritten(message);
      setRoleDialog(null);
      setRoleToDelete(null);
      setRoleWriteFailure(undefined);
    },
    [onWritten],
  );

  // The failure stays INSIDE the dialog rather than being raised to the page
  // banner: a 409 here names the role and, for a delete, how many members still
  // hold it, and the operator needs that beside the control that produced it.
  const failed = useCallback((fallback: string, error: unknown) => {
    setRoleWriteFailure(permissionMatrixFailureReason(error) ?? fallback);
  }, []);

  const onSubmitRoleDialog = useCallback(
    (name: string) => {
      if (!roleDialog) return;
      setRoleWriteFailure(undefined);
      if (roleDialog.mode === 'create') {
        createRole.mutate(
          { target, name },
          {
            onSuccess: () => succeeded('roleCreated'),
            onError: (error) => failed('roleCreate', error),
          },
        );
        return;
      }
      renameRole.mutate(
        { target, name: roleDialog.role, newName: name },
        {
          onSuccess: () => succeeded('roleRenamed'),
          onError: (error) => failed('roleRename', error),
        },
      );
    },
    [roleDialog, target, createRole, renameRole, succeeded, failed],
  );

  const onConfirmDeleteRole = useCallback(() => {
    if (roleToDelete === null) return;
    setRoleWriteFailure(undefined);
    deleteRole.mutate(
      { target, name: roleToDelete },
      {
        onSuccess: () => succeeded('roleDeleted'),
        onError: (error) => failed('roleDelete', error),
      },
    );
  }, [roleToDelete, target, deleteRole, succeeded, failed]);

  const canCreateRole = adminUiShowsControlFor(PERMISSION_ROLE_CREATE);
  const canRenameRole = adminUiShowsControlFor(PERMISSION_ROLE_EDIT);
  const canDeleteRole = adminUiShowsControlFor(PERMISSION_ROLE_DELETE);

  // A built-in role gets NO rename or delete control, because the server refuses
  // both. Rendering one anyway would be a button whose only outcome is a 409.
  const renameHandlerFor = useCallback(
    (role: string) =>
      canRenameRole && !isBuiltInRole(role)
        ? () => {
            setRoleWriteFailure(undefined);
            setRoleDialog({ mode: 'rename', role });
          }
        : undefined,
    [canRenameRole],
  );
  const deleteHandlerFor = useCallback(
    (role: string) =>
      canDeleteRole && !isBuiltInRole(role)
        ? () => {
            setRoleWriteFailure(undefined);
            setRoleToDelete(role);
          }
        : undefined,
    [canDeleteRole],
  );

  const onCreateRole = useCallback(() => {
    setRoleWriteFailure(undefined);
    setRoleDialog({ mode: 'create', role: '' });
  }, []);

  return {
    onCreateRole: canCreateRole ? onCreateRole : undefined,
    renameHandlerFor,
    deleteHandlerFor,
    roleDialog,
    roleToDelete,
    isWritingRole: createRole.isPending || renameRole.isPending || deleteRole.isPending,
    roleWriteFailure,
    onCloseRoleDialog,
    onSubmitRoleDialog,
    onConfirmDeleteRole,
  };
}
