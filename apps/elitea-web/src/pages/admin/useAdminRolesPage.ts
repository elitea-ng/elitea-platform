/**
 * State, data and write handlers for `pages/admin/Roles.tsx` (unit A14).
 *
 * Split out of the page for the same reason `useAdminUsersPage` is: the page
 * component stays a render, and the branching that decides WHICH controls exist
 * lives in one place.
 *
 * ## The draft is not re-seeded from a background refetch
 *
 * The matrix is an editable draft over a server document, so it has the exact
 * shape #191 hit on the Settings › Users dialog: a refetch that runs while the
 * operator is mid-edit must not discard what they typed. The draft is therefore
 * seeded ONCE per tab — when the tab changes, or when its data first arrives —
 * and never again. Discard re-seeds it explicitly.
 *
 * ## …except when the COLUMNS change, which is a different event (gap G9)
 *
 * Creating, renaming or deleting a role changes which columns the matrix has.
 * A draft seeded before that change has the old column set, so leaving it in
 * place would hide the role the operator just created — the control would look
 * like it did nothing. Those three writes therefore bump `seedToken`, and the
 * draft is re-seeded when the token moves.
 *
 * The token is bumped AFTER the refetch has settled, never before: the three
 * mutations return their `invalidateQueries` promise from `onSuccess`
 * (`./api/adminRolesApi`), so by the time the call-site `onSuccess` below runs,
 * `serverRows` already carries the new column. Bumping first would re-seed from
 * the matrix that still lacks it, and the effect would not fire a second time.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { adminUiShowsControlFor } from './adminUiConfig';
import {
  permissionMatrixFailureReason,
  usePermissionMatrix,
  useSavePermissionMatrix,
  useSyncPermissionMatrix,
  type PermissionMatrixRow,
  type PermissionMatrixTarget,
} from './api/adminRolesApi';
import { useAdminRoleDefinitions, type AdminRoleDefinitionsState } from './useAdminRoleDefinitions';

/** The four tabs, in render order, with the endpoint each addresses. */
const ROLE_TABS = [
  { key: 'admin', scope: 'administration', targetMode: 'administration' },
  { key: 'standard', scope: 'administration', targetMode: 'default' },
  { key: 'public', scope: 'public', targetMode: 'default' },
  { key: 'support', scope: 'support', targetMode: 'default' },
] as const satisfies readonly ({ key: string } & PermissionMatrixTarget)[];

type RoleTabKey = (typeof ROLE_TABS)[number]['key'];

/**
 * Column order, matching the reference page. Roles the response does not carry
 * are dropped, and any role the deployment defines beyond these is appended
 * rather than hidden — an invisible role column is an invisible privilege.
 */
const ROLE_ORDER: readonly string[] = ['system', 'super_admin', 'admin', 'editor', 'viewer'];

/** The permission the (hardcoded) admin-panel config advertises for editing. */
const PERMISSION_ROLES_EDIT = 'configuration.roles.permissions.edit';

interface AdminRolesMatrixState {
  readonly activeTab: number;
  readonly search: string;
  readonly rows: readonly PermissionMatrixRow[] | undefined;
  readonly roles: readonly string[];
  readonly isFetching: boolean;
  readonly isDirty: boolean;
  readonly isSaving: boolean;
  readonly isSyncing: boolean;
  /** The server's own words when a tab is refused or unavailable. */
  readonly unavailableReason: string | undefined;
  readonly isError: boolean;
  readonly errorMessage: string;
  readonly savedMessage: string;

  /** Presentation only — the server authorises every write on its own. */
  readonly canEdit: boolean;

  readonly onTabChange: (event: unknown, next: number) => void;
  readonly onSearchChange: (value: string) => void;
  readonly onDismissError: () => void;
  readonly onDismissSaved: () => void;
  readonly onChange: (updater: (rows: readonly PermissionMatrixRow[]) => PermissionMatrixRow[]) => void;
  readonly onDiscard: () => void;
  readonly onSave: () => void;
  /** `undefined` ⇒ "Apply to Projects" is not offered on this tab. */
  readonly onApplyToProjects: (() => void) | undefined;

}

/**
 * What the page renders. The role-DEFINITION half comes from
 * `./useAdminRoleDefinitions`: create, rename and delete change WHICH COLUMNS
 * the matrix has, while everything above is about the cells (gap G9).
 */
export type AdminRolesPageState = AdminRolesMatrixState & AdminRoleDefinitionsState;

interface Draft {
  readonly tabKey: RoleTabKey;
  /**
   * The `seedToken` the draft was seeded at. A role create, rename or delete
   * changes the matrix COLUMNS, which a draft seeded before it cannot show —
   * see this module's header.
   */
  readonly seedToken: number;
  readonly rows: PermissionMatrixRow[];
}

/**
 * An order-independent signature of the matrix, used for the dirty check.
 *
 * `JSON.stringify` would work only as long as every row keeps its key order,
 * which a spread-based edit happens to preserve today and is not a property
 * worth depending on — a false "not dirty" hides the Save button entirely.
 */
function matrixSignature(rows: readonly PermissionMatrixRow[] | undefined): string {
  if (!rows) return '';
  return rows
    .map((row) => {
      const cells = Object.keys(row)
        .filter((key) => key !== 'name')
        .sort()
        .map((key) => `${key}=${row[key] === true ? '1' : '0'}`)
        .join(',');
      return `${row.name}|${cells}`;
    })
    .sort()
    .join(';');
}

function orderRoles(rows: readonly PermissionMatrixRow[] | undefined): string[] {
  const first = rows?.[0];
  if (!first) return [];
  const present = Object.keys(first).filter((key) => key !== 'name');
  const known = ROLE_ORDER.filter((role) => present.includes(role));
  const extra = present.filter((role) => !ROLE_ORDER.includes(role)).sort();
  return [...known, ...extra];
}

export function useAdminRolesPage(): AdminRolesPageState {
  const [activeTab, setActiveTab] = useState(0);
  const [search, setSearch] = useState('');
  const [draft, setDraft] = useState<Draft | null>(null);
  const [errorMessage, setErrorMessage] = useState('');
  const [savedMessage, setSavedMessage] = useState('');
  // Bumped once per successful role-definition write, after its refetch has
  // settled. See this module's header for why the order matters.
  const [seedToken, setSeedToken] = useState(0);

  const tab = ROLE_TABS[activeTab] ?? ROLE_TABS[0];
  const target = useMemo(
    () => ({ scope: tab.scope, targetMode: tab.targetMode }),
    [tab.scope, tab.targetMode],
  );

  const matrixQuery = usePermissionMatrix(target);
  const saveMatrix = useSavePermissionMatrix();
  const syncMatrix = useSyncPermissionMatrix();

  const serverRows = matrixQuery.data;

  // Seed once per tab, and once more per role-definition write. See this
  // module's header: a background refetch must not discard an edit in progress,
  // but a change to the COLUMNS is not a background refetch.
  useEffect(() => {
    if (!serverRows) return;
    setDraft((previous) =>
      previous?.tabKey === tab.key && previous.seedToken === seedToken
        ? previous
        : { tabKey: tab.key, seedToken, rows: [...serverRows] },
    );
  }, [serverRows, tab.key, seedToken]);

  const rows = draft?.tabKey === tab.key ? draft.rows : undefined;
  const isDirty = useMemo(
    () => rows !== undefined && matrixSignature(rows) !== matrixSignature(serverRows),
    [rows, serverRows],
  );

  const onChange = useCallback(
    (updater: (current: readonly PermissionMatrixRow[]) => PermissionMatrixRow[]) => {
      setDraft((previous) => (previous ? { ...previous, rows: updater(previous.rows) } : previous));
    },
    [],
  );

  const onDiscard = useCallback(() => {
    if (serverRows) setDraft({ tabKey: tab.key, seedToken, rows: [...serverRows] });
  }, [serverRows, tab.key, seedToken]);

  const reportFailure = useCallback((fallback: string, error: unknown) => {
    setSavedMessage('');
    setErrorMessage(permissionMatrixFailureReason(error) ?? fallback);
  }, []);

  const onSave = useCallback(() => {
    if (!rows) return;
    setErrorMessage('');
    setSavedMessage('');
    saveMatrix.mutate(
      { target, rows },
      {
        onSuccess: () => setSavedMessage('saved'),
        onError: (error) => reportFailure('save', error),
      },
    );
  }, [rows, target, saveMatrix, reportFailure]);

  const onApply = useCallback(() => {
    setErrorMessage('');
    setSavedMessage('');
    syncMatrix.mutate(undefined, {
      onSuccess: () => setSavedMessage('synced'),
      onError: (error) => reportFailure('sync', error),
    });
  }, [syncMatrix, reportFailure]);

  // The seed token moves ONLY here, and only after a role write has settled —
  // which, because the three mutations return their invalidation promise from
  // `onSuccess`, is after the matrix has been re-read with its new columns.
  const onRoleWritten = useCallback((message: string) => {
    setErrorMessage('');
    setSeedToken((previous) => previous + 1);
    setSavedMessage(message);
  }, []);
  const definitions = useAdminRoleDefinitions(target, onRoleWritten);
  const { onCloseRoleDialog } = definitions;

  const onTabChange = useCallback(
    (_event: unknown, next: number) => {
      setActiveTab(next);
      setSearch('');
      setErrorMessage('');
      setSavedMessage('');
      // A dialog left open across a tab change would write to the tab the
      // operator moved away from: the target is read at submit time.
      onCloseRoleDialog();
    },
    [onCloseRoleDialog],
  );

  const canEdit = adminUiShowsControlFor(PERMISSION_ROLES_EDIT);

  return {
    activeTab,
    search,
    rows,
    roles: orderRoles(rows),
    isFetching: matrixQuery.isFetching,
    isDirty,
    isSaving: saveMatrix.isPending,
    isSyncing: syncMatrix.isPending,
    unavailableReason: permissionMatrixFailureReason(matrixQuery.error),
    isError: matrixQuery.isError,
    errorMessage,
    savedMessage,

    canEdit,

    onTabChange,
    onSearchChange: useCallback((value: string) => setSearch(value), []),
    onDismissError: useCallback(() => setErrorMessage(''), []),
    onDismissSaved: useCallback(() => setSavedMessage(''), []),
    onChange,
    onDiscard,
    onSave,
    // "Apply to Projects" pushes the STANDARD matrix onto shared projects. The
    // server defines it for `administration/default` alone, so offering it on
    // any other tab would be a control with nothing behind it.
    //
    // It is ALSO how a newly created standard role reaches projects that
    // already exist: `AdminPermissionsSync` cross-joins every central
    // default-mode role into every in-scope project, so a create followed by
    // this button is the whole propagation path (gap G9). No second control
    // was invented for it.
    onApplyToProjects: canEdit && tab.key === 'standard' ? onApply : undefined,

    ...definitions,
  };
}
