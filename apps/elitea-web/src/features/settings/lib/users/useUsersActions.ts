// @ts-nocheck
/**
 * useUsersActions — mutations, callbacks, and action configs for UsersPage.
 * Extracted to keep UsersPage ≤ 400 lines (spec §3.5).
 */
import { useMemo, useCallback } from 'react';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import type { EditUsersButtonProps } from '@/shared/ui/settings/EditUsersButton';
import type { DeleteUserButtonProps } from '@/shared/ui/settings/DeleteUserButton';
import { useEditUser, useBatchEditUsers, useDeleteUsers } from '@/entities/user';
import { userCreate, getUserListQueryKey } from '@/shared/api/generated/admin/admin';
import { unwrapBody } from '@/shared/api/unwrap';
import {
  readInviteRows,
  readInviteRowsFromError,
  summariseInviteRows,
  type InviteAddressResult,
  type InviteSummary,
} from '@/shared/ui/settings/inviteResults';

export interface UseUsersActionsArgs {
  projectId: string;
  selectedUsers: { id: string; name: string; email: string; roles: string[] }[];
  rolesOptions: { label: string; value: string }[];
  onDeleteSuccess: () => void;
  onDeleteError: (error: unknown) => void;
  onInviteSuccess: (outcome: InviteOutcome) => void;
  onInviteError: (error: unknown, outcome: InviteOutcome) => void;
  onEditSuccess: () => void;
  onEditError: (error: unknown) => void;
}

export interface UseUsersActionsResult {
  deleteUserMutation: ReturnType<typeof useDeleteUsers>;
  editHook: ReturnType<typeof useEditUser>;
  batchEditHook: ReturnType<typeof useBatchEditUsers>;
  inviteHook: { inviteUsers: (emails: string[], roles: string[]) => void; isLoading: boolean };
  handleDelete: () => void;
  handleBatchRoleSave: (roles: string[]) => void;
  handleInviteConfirm: (data: { emails: string[]; roles: string[] }) => void;
  singleAction: { edit: EditUsersButtonProps; delete: Record<string, unknown> } | null;
  batchAction: { edit: EditUsersButtonProps; delete: Record<string, unknown> } | null;
  actions: { edit: EditUsersButtonProps | null; delete: Record<string, unknown> } | null;
}

/**
 * Invite-users mutation.
 *
 * `entities/user` (spec §3.3, ≤20-export budget) does not curate a create/
 * invite hook. `userCreate` used to be a live no-op on the Go router — the
 * router mounted POST on the listing handler, so this mutation's `onSuccess`
 * fired and nothing was written (#130). It now POSTs to a real handler that
 * takes exactly the `{emails, roles}` body built below, so the success toast
 * finally means what it says. Firing the real request and reacting to the
 * real result (old-app parity: `Users.jsx`'s
 * `useUserCreateMutation()`) is what made that fixable at all — a faked
 * success would have hidden it. Wrapping the generated `userCreate` fetcher
 * with `useMutation` locally — rather than adding a new curated export to
 * `entities/user` — mirrors the same "local, feature-owned hook" call this
 * cluster already makes for `useHasPermission` (`features/agents/lib/
 * useHasPermission.ts`'s doc comment): the duplication is a dozen lines,
 * not worth threading a new entities/user primitive through for from this
 * fix's scope.
 */
/**
 * What the invite mutation reports to the page: the server's answer for EVERY
 * address, plus the counts derived from it.
 *
 * `delivered` (ADR-0024 WP7) says whether an invitation e-mail went out for
 * every invited address. The server says so per row (`invitation_delivered`);
 * a deployment with no SMTP configured answers false, and the page must not
 * then tell the operator "invited" as if a message had been sent.
 *
 * `rows` is the bulk half. A batch of twelve addresses can end in four
 * different states at once, and the page used to see only the aggregate — so
 * "eleven invited, one already a member" and "all twelve failed" were the same
 * red toast. The rows are carried through so the dialog can render them.
 */
interface InviteOutcome {
  readonly delivered: boolean;
  readonly rows: readonly InviteAddressResult[];
  readonly summary: InviteSummary;
}

function inviteOutcome(rows: readonly InviteAddressResult[]): InviteOutcome {
  const summary = summariseInviteRows(rows);
  return { delivered: summary.delivered, rows, summary };
}

function useInviteUsers(
  projectId: string,
  onSuccess: (outcome: InviteOutcome) => void,
  onError: (error: unknown, outcome: InviteOutcome) => void,
) {
  const queryClient = useQueryClient();

  const mutation = useMutation({
    mutationFn: ({ emails, roles }: { emails: string[]; roles: string[] }) =>
      userCreate(projectId, { emails, roles }),
    onSuccess: (results) => {
      void queryClient.invalidateQueries({ queryKey: getUserListQueryKey(projectId) });
      onSuccess(inviteOutcome(readInviteRows(unwrapBody(results))));
    },
    onError: (error: unknown) => {
      // A 400 here is a PARTIAL failure, not a rejected request: each address is
      // written in its own transaction, so the `ok` rows in that body have
      // already landed. Invalidating the member list on this path is what makes
      // them appear; skipping it left the table stale until a manual reload.
      void queryClient.invalidateQueries({ queryKey: getUserListQueryKey(projectId) });
      onError(error, inviteOutcome(readInviteRowsFromError(error)));
    },
  });

  const { mutate } = mutation;
  const inviteUsers = useCallback(
    (emails: string[], roles: string[]) => {
      mutate({ emails, roles });
    },
    [mutate],
  );

  return { inviteUsers, isLoading: mutation.isPending };
}

export function useUsersActions({
  projectId,
  selectedUsers,
  rolesOptions,
  onDeleteSuccess,
  onDeleteError,
  onInviteSuccess,
  onInviteError,
  onEditSuccess,
  onEditError,
}: UseUsersActionsArgs): UseUsersActionsResult {
  const editHook = useEditUser({
    projectId,
    onSuccess: onEditSuccess,
    onError: onEditError,
  });

  const batchEditHook = useBatchEditUsers({
    userIds: selectedUsers.map((u) => u.id),
    projectId,
    onSuccess: onEditSuccess,
    onError: onEditError,
  });

  const deleteUserMutation = useDeleteUsers({
    userIds: selectedUsers.map((u) => u.id),
    projectId,
    onSuccess: onDeleteSuccess,
    onError: onDeleteError,
  });

  const inviteHook = useInviteUsers(projectId, onInviteSuccess, onInviteError);

  /* ── callbacks ─────────────────────────────────────────────────────── */
  // Destructure the stable callbacks out of each hook result. The hook results
  // themselves are fresh object literals every render, so closing over them
  // rebuilt `singleAction`/`batchAction` — and therefore the `userRoles` array
  // handed to `EditUserRolesDialog` — on every render of the Users page,
  // including background refetches while the Edit-roles dialog is open.
  const { saveUser } = editHook;
  const { saveUsers } = batchEditHook;
  const { deleteUserIds } = deleteUserMutation;
  const { inviteUsers } = inviteHook;

  const handleDelete = useCallback(() => {
    const ids = selectedUsers.map((u) => parseInt(u.id, 10));
    deleteUserIds(ids);
  }, [selectedUsers, deleteUserIds]);

  const handleBatchRoleSave = useCallback(
    (roles: string[]) => {
      saveUsers(roles);
    },
    [saveUsers],
  );

  const handleInviteConfirm = useCallback(
    (data: { emails: string[]; roles: string[] }) => {
      inviteUsers(data.emails, data.roles);
    },
    [inviteUsers],
  );

  /* ── action configs ────────────────────────────────────────────────── */
  const singleAction = useMemo(() => {
    if (selectedUsers.length !== 1) return null;
    const user = selectedUsers[0]!;
    const editProps: Record<string, unknown> = {
      userIds: [user.id],
      userRoles: Array.from(user.roles),
      rolesOptions,
      onConfirm: (roles: string[]) => {
        saveUser(user.id, roles);
      },
    };
    if (editHook.isLoading !== undefined) editProps.isLoading = editHook.isLoading;
    const deleteProps: Record<string, unknown> = {
      userIds: [user.id],
      onConfirm: () => {
        const ids = [parseInt(user.id, 10)];
        deleteUserIds(ids);
      },
    };

    return {
      edit: editProps as unknown as EditUsersButtonProps,
      delete: deleteProps as unknown as DeleteUserButtonProps,
    };
  }, [selectedUsers, rolesOptions, saveUser, editHook.isLoading, deleteUserIds]);

  const batchAction = useMemo(() => {
    if (selectedUsers.length < 2) return null;
    const editProps: Record<string, unknown> = {
      userIds: selectedUsers.map((u) => u.id),
      rolesOptions,
      onConfirm: handleBatchRoleSave,
    };
    if (batchEditHook.isLoading !== undefined) editProps.isLoading = batchEditHook.isLoading;
    const deleteProps: Record<string, unknown> = {
      userIds: selectedUsers.map((u) => u.id),
      onConfirm: () => { handleDelete(); },
    };

    return {
      edit: editProps as unknown as EditUsersButtonProps,
      delete: deleteProps as unknown as DeleteUserButtonProps,
    };
  }, [selectedUsers, rolesOptions, batchEditHook.isLoading, handleBatchRoleSave, handleDelete]);

  const actions = batchAction ?? singleAction;

  return {
    deleteUserMutation,
    editHook,
    batchEditHook,
    inviteHook,
    handleDelete,
    handleBatchRoleSave,
    handleInviteConfirm,
    singleAction,
    batchAction,
    actions,
  };
}
