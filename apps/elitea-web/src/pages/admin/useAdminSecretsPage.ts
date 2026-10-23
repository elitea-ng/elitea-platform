/**
 * State, data and write handlers for `pages/admin/Secrets.tsx` (unit A14).
 *
 * Split out of the page for the same reason `useAdminRolesPage` is: the page
 * component stays a render, and the branching that decides WHICH controls exist
 * lives in one place.
 *
 * ## The Internal tab is a classification, not a server concept
 *
 * The global vault has no "internal" flag. The reference page derives the split
 * from the NAME (`SecretsPage/constants.js`), and this reproduces that rule
 * exactly, because the classification is about which secrets a human put there:
 *
 *   - `auth_token` — written by the legacy runtime's `auth_init` RPC at
 *     startup. It is the only entry in the reference deployment's global vault.
 *   - `project_llm_key` — the per-project LLM key the LiteLLM runtime managed.
 *   - a bare 32-character hex name — `SecretString.store_secret()` mints these
 *     as opaque references for configuration credential fields.
 *
 * Those rows are shown, and shown read-only. Hiding them would make a vault
 * whose contents the operator cannot fully account for; offering Edit on them
 * would offer to break a platform invariant from a UI with no idea what the
 * value is for. The reference makes the same call and this keeps it.
 *
 * ## Search, sort and pagination are client-side
 *
 * As in the reference — and necessarily so: the listing endpoint is
 * `GET /secrets/secrets/administration/0` with no parameters at all, in the
 * legacy runtime and in `internal/api/v2/secrets/admin.go` alike. A global
 * vault holds tens of entries, not thousands, so the whole set is one response.
 */
import { useCallback, useMemo, useState } from 'react';

import { t } from '@/shared/i18n';

import { adminUiShowsControlFor } from './adminUiConfig';
import {
  adminSecretFailureReason,
  useAdminSecrets,
  useCreateAdminSecret,
  useDeleteAdminSecret,
  useRevealAdminSecret,
  useUpdateAdminSecret,
  type AdminSecret,
} from './api/adminSecretsApi';

/** Names the platform writes for itself. See this module's header. */
const INTERNAL_SECRET_NAMES: ReadonlySet<string> = new Set(['auth_token', 'project_llm_key']);

/** `SecretString.store_secret()` mints a bare uuid4 hex as the reference name. */
const HEX_REFERENCE_NAME = /^[0-9a-f]{32}$/;

function isInternalSecretName(name: string): boolean {
  return INTERNAL_SECRET_NAMES.has(name) || HEX_REFERENCE_NAME.test(name);
}

/**
 * The permissions the (hardcoded) admin-panel config advertises. Presentation
 * only — every one of them is re-resolved server-side per request.
 *
 * Note `list` and `view` are both here and they are NOT the same grant: the
 * admin SPA's sidebar has always gated this page on `.list`, while the legacy
 * admin READ handlers declare `.view` — and on the reference deployment the
 * administration-mode `editor` role holds the first and not the second. The page
 * therefore renders for an editor and its reads are refused, which is why the
 * server's reason is surfaced rather than a generic failure.
 */
const PERMISSION_SECRETS_LIST = 'configuration.secrets.secret.list';
const PERMISSION_SECRETS_CREATE = 'configuration.secrets.secret.create';
const PERMISSION_SECRETS_EDIT = 'configuration.secrets.secret.edit';
const PERMISSION_SECRETS_DELETE = 'configuration.secrets.secret.delete';

export interface AdminSecretsPageState {
  readonly activeTab: number;
  readonly counts: { readonly user: number; readonly internal: number };
  readonly search: string;
  readonly rows: readonly AdminSecret[];
  readonly allNames: ReadonlySet<string>;
  readonly isFetching: boolean;
  readonly isError: boolean;
  /** The server's own words when the listing was refused or unavailable. */
  readonly unavailableReason: string | undefined;

  readonly dialogOpen: boolean;
  readonly editingName: string | undefined;
  readonly isSaving: boolean;
  readonly saveError: string | undefined;

  readonly deleteName: string | undefined;
  readonly isDeleting: boolean;

  readonly errorMessage: string;
  readonly savedMessage: string;

  /** Presentation only — the server authorises every one of these on its own. */
  readonly canReveal: boolean;
  readonly onCreate: (() => void) | undefined;
  readonly onEdit: ((name: string) => void) | undefined;
  readonly onDelete: ((name: string) => void) | undefined;

  readonly onTabChange: (event: unknown, next: number) => void;
  readonly onSearchChange: (value: string) => void;
  readonly onReveal: (name: string) => Promise<string | null>;
  readonly onDialogClose: () => void;
  readonly onDialogSubmit: (name: string, value: string) => void;
  readonly onDeleteCancel: () => void;
  readonly onDeleteConfirm: () => void;
  readonly onDismissError: () => void;
  readonly onDismissSaved: () => void;
}

export function useAdminSecretsPage(): AdminSecretsPageState {
  const [activeTab, setActiveTab] = useState(0);
  const [search, setSearch] = useState('');
  const [dialogOpen, setDialogOpen] = useState(false);
  const [editingName, setEditingName] = useState<string | undefined>(undefined);
  const [deleteName, setDeleteName] = useState<string | undefined>(undefined);
  const [errorMessage, setErrorMessage] = useState('');
  const [saveError, setSaveError] = useState<string | undefined>(undefined);
  const [savedMessage, setSavedMessage] = useState('');

  const listQuery = useAdminSecrets();
  const reveal = useRevealAdminSecret();
  const create = useCreateAdminSecret();
  const update = useUpdateAdminSecret();
  const remove = useDeleteAdminSecret();

  const secrets = useMemo(() => listQuery.data ?? [], [listQuery.data]);

  const { user, internal } = useMemo(() => {
    const userSecrets: AdminSecret[] = [];
    const internalSecrets: AdminSecret[] = [];
    for (const secret of secrets) {
      (isInternalSecretName(secret.name) ? internalSecrets : userSecrets).push(secret);
    }
    return { user: userSecrets, internal: internalSecrets };
  }, [secrets]);

  const active = activeTab === 0 ? user : internal;
  const rows = useMemo(() => {
    const needle = search.trim().toLowerCase();
    if (needle === '') return active;
    return active.filter((secret) => secret.name.toLowerCase().includes(needle));
  }, [active, search]);

  const allNames = useMemo(() => new Set(secrets.map((secret) => secret.name)), [secrets]);

  const onReveal = useCallback(
    async (name: string): Promise<string | null> => {
      try {
        return await reveal.mutateAsync(name);
      } catch (error) {
        setErrorMessage(adminSecretFailureReason(error) ?? 'reveal');
        return null;
      }
    },
    [reveal],
  );

  const onDialogSubmit = useCallback(
    (name: string, value: string) => {
      setSaveError(undefined);
      const mutation = editingName === undefined ? create : update;
      mutation.mutate(
        { name, value },
        {
          onSuccess: () => {
            setDialogOpen(false);
            setEditingName(undefined);
            setSavedMessage(editingName === undefined ? 'created' : 'updated');
          },
          onError: (error) =>
            setSaveError(
              adminSecretFailureReason(error) ??
                t('pages.admin.secrets.error.save', 'Failed to save the secret.'),
            ),
        },
      );
    },
    [editingName, create, update],
  );

  const onDeleteConfirm = useCallback(() => {
    if (deleteName === undefined) return;
    remove.mutate(deleteName, {
      onSuccess: () => {
        setDeleteName(undefined);
        setSavedMessage('deleted');
      },
      onError: (error) => {
        setDeleteName(undefined);
        setErrorMessage(adminSecretFailureReason(error) ?? 'delete');
      },
    });
  }, [deleteName, remove]);

  // Presentation flags. The Internal tab is read-only for everyone regardless
  // of what these say — see this module's header.
  const isUserTab = activeTab === 0;
  const canCreate = isUserTab && adminUiShowsControlFor(PERMISSION_SECRETS_CREATE);
  const canEdit = isUserTab && adminUiShowsControlFor(PERMISSION_SECRETS_EDIT);
  const canDelete = isUserTab && adminUiShowsControlFor(PERMISSION_SECRETS_DELETE);

  return {
    activeTab,
    counts: { user: user.length, internal: internal.length },
    search,
    rows,
    allNames,
    isFetching: listQuery.isFetching,
    isError: listQuery.isError,
    unavailableReason: adminSecretFailureReason(listQuery.error),

    dialogOpen,
    editingName,
    isSaving: create.isPending || update.isPending,
    saveError,

    deleteName,
    isDeleting: remove.isPending,

    errorMessage,
    savedMessage,

    // The reveal is gated server-side on `…secret.view`; the SPA config
    // advertises `…secret.list`, and the two are different grants. Rendering the
    // toggle on the strictly weaker one and letting the server refuse is the
    // honest ordering — hiding it on a permission the config does not carry
    // would hide the affordance from callers who do hold `.view`.
    canReveal: adminUiShowsControlFor(PERMISSION_SECRETS_LIST),
    onCreate: canCreate
      ? () => {
          setEditingName(undefined);
          setSaveError(undefined);
          setDialogOpen(true);
        }
      : undefined,
    onEdit: canEdit
      ? (name: string) => {
          setEditingName(name);
          setSaveError(undefined);
          setDialogOpen(true);
        }
      : undefined,
    onDelete: canDelete ? (name: string) => setDeleteName(name) : undefined,

    onTabChange: useCallback((_event: unknown, next: number) => {
      setActiveTab(next);
      setSearch('');
      setErrorMessage('');
      setSavedMessage('');
    }, []),
    onSearchChange: useCallback((value: string) => setSearch(value), []),
    onReveal,
    onDialogClose: useCallback(() => {
      setDialogOpen(false);
      setEditingName(undefined);
      setSaveError(undefined);
    }, []),
    onDialogSubmit,
    onDeleteCancel: useCallback(() => setDeleteName(undefined), []),
    onDeleteConfirm,
    onDismissError: useCallback(() => setErrorMessage(''), []),
    onDismissSaved: useCallback(() => setSavedMessage(''), []),
  };
}
