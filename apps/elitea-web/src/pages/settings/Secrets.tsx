/**
 * Secrets settings content — composes `DrawerPageHeader` +
 * `SecretsTable` + data fetching.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/secrets/
 * SecretsContent.jsx`.  This component owns:
 *
 *  - Fetching secrets via `useListSecretsQuery`
 *  - Search filtering (client-side, matches old-app pattern)
 *  - Creating new secret rows (button + `?createSecret=1` flag)
 *  - Wiring API mutations to the actions hook
 *  - Error toasts on API failures
 *  - Permission gating (`PERMISSIONS.secrets.list` / `.unsecret`)
 *
 * Deviations from the baseline:
 *  - No Redux (sidebar state → dropped)
 *  - No tour IDs
 *  - Uses `DrawerPageHeader` from shared UI
 *  - Uses `useSelectedProjectStore` for project ID
 *  - Pagination state is lifted into SecretsTable (self-contained)
 *  - Permission check is inlined here (local `usePermissionList` read)
 *    rather than a shared `useCheckPermission()` hook — this codebase's
 *    established convention is a per-slice local copy of that hook (see
 *    `features/chat-input/lib/hooks/useCheckPermission.hooks.ts`'s own doc
 *    comment on `no-sideways-features`); inlining here avoids adding a new
 *    file outside this unit's file scope for a two-line computation.
 */
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import Snackbar from '@mui/material/Snackbar';
import type { SxProps, Theme } from '@mui/material/styles';

import {
  useSecretsActions,
  useListSecretsQuery,
  useCreateSecretMutation,
  useUpdateSecretMutation,
  useDeleteSecretMutation,
  useHideSecretMutation,
  showSecret,
} from '@/entities/secret';
import type { SecretRow, SecretMutations, Secret } from '@/entities/secret';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import { secretsFeature } from '@/features/settings';
import { handleCopy } from '@/shared/lib/clipboard';
import { EliteaApiError } from '@/shared/api/generated/mutator';

const { SecretsTable, SecretsLoadError, useSecretPermissions } = secretsFeature;
import { t } from '@/shared/i18n';

/**
 * A stable empty-array fallback for `useListSecretsQuery`'s `data`.
 *
 * BUG FIX: a destructuring default (`const { data: secrets = [] } = …`)
 * allocates a BRAND NEW `[]` literal on every render in which `data` is
 * `undefined` — which is exactly the state the query sits in for as long
 * as it stays disabled (no project selected) or never resolves (API
 * client unconfigured / erroring). That fresh reference then feeds the
 * `useEffect` below as a dependency: React sees a "changed" dep on every
 * render, re-runs the effect, calls `setRows` with a new array reference
 * every time (even though the effective contents never change), which
 * triggers a re-render — which allocates a fresh `[]` again — forever.
 * Reading `data` directly and falling back to this module-level constant
 * keeps the reference identical across renders whenever `data` is
 * genuinely absent, so the effect's dependency is only ever "changed"
 * when the query actually produces a new result.
 */
const EMPTY_SECRETS: Secret[] = [];

export interface SecretsContentProps {
  /** Whether `?createSecret=1` is in the URL. */
  shouldCreate: boolean;
  /** Search query from the header's search input. */
  search: string;
  /** Update the search query. */
  onSearchChange: (value: string) => void;
}

interface ToastState {
  readonly severity: 'success' | 'error';
  readonly message: string;
}

/** `EliteaApiError`'s 403 case — mirrors the baseline's `error?.status === 403` special-casing (`SecretsContent.jsx:105`, `SecretsTable.jsx:328`). */
function isForbiddenError(error: unknown): boolean {
  if (!(error instanceof EliteaApiError)) return false;
  const { failure } = error;
  return (failure.kind === 'http' || failure.kind === 'auth') && failure.status === 403;
}

/** A blank, editable row for a secret not saved yet. */
function newSecretRow(): SecretRow {
  return { id: `new-${Date.now()}`, name: '', secretName: '', isDefault: false, secretValue: '', isNew: true };
}

/** Split out purely to keep `SecretsContent`'s own `useEffect` count within the §3.5 budget (3) — syncs the memoized mutations wrapper into the actions hook whenever either reference changes. */
function useSyncSecretMutations(setMutations: (m: SecretMutations) => void, mutationsWrapper: SecretMutations): void {
  useEffect(() => {
    setMutations(mutationsWrapper);
  }, [setMutations, mutationsWrapper]);
}

export const SecretsContent = memo(function SecretsContent({
  shouldCreate,
  search,
  onSearchChange,
}: SecretsContentProps) {
  /* ── project context ──────────────────────────────────────────────── */
  const projectId = useSelectedProjectStore((s) => s.project?.id ?? '');

  /* ── permissions ───────────────────────────────────────────────────── */
  // `canList` gates the QUERY; the rest gate controls. #402 grants the list to
  // the viewer, which is the first role that reads this page and writes nothing
  // on it, so every write control below is gated too.
  const { canList, controls: secretPermissions } = useSecretPermissions(projectId);
  const canCreate = secretPermissions.canCreate;

  /* ── API query ────────────────────────────────────────────────────── */
  const { data, isFetching, isError, error, refetch } = useListSecretsQuery(projectId, {
    enabled: !!projectId && canList,
  });
  const secrets = data ?? EMPTY_SECRETS;
  // A FAILED list offers no create: its error state renders no table.
  const canAddSecret = canCreate && !isError;

  /* ── API mutations ────────────────────────────────────────────────── */
  const createMutation = useCreateSecretMutation(projectId);
  const updateMutation = useUpdateSecretMutation(projectId);
  const deleteMutation = useDeleteSecretMutation(projectId);
  const hideMutation = useHideSecretMutation(projectId);

  /* ── toast ─────────────────────────────────────────────────────────── */
  const [toast, setToast] = useState<ToastState | null>(null);
  const closeToast = useCallback(() => setToast(null), []);

  useEffect(() => {
    if (isError) {
      setToast({
        severity: 'error',
        message: isForbiddenError(error)
          ? t('entities.secret.error.forbidden', 'The access is not allowed')
          : t('entities.secret.error.listFailed', 'Failed to load secrets'),
      });
    }
  }, [isError, error]);

  /* ── row state — the actions hook's own `rows`/`setRows` is the single
   * source of truth, since its handlers (onSave/onEdit/onShowSecret/…)
   * read/write via an internal `rowsRef` seeded ONLY by this state. A
   * separate local `useState` here (as a prior version of this file had)
   * left that ref permanently empty — every handler wired through
   * `actions` would silently no-op on a row lookup that always misses. ── */
  const actions = useSecretsActions();
  const { rows, setRows } = actions;

  // Sync API data into rows (preserve temporary new-secret rows)
  useEffect(() => {
    if (!isFetching) {
      setRows((prev) => {
        const newRows = prev.filter((r) => r.isNew);
        const mapped: SecretRow[] = secrets.map((s) => ({
          id: `existing-${s.name}`,
          name: s.name,
          secretName: s.secretName,
          isDefault: s.isDefault,
          secretValue: s.secretName,
          isNew: false,
        }));
        return [...newRows, ...mapped];
      });
    }
  }, [secrets, isFetching, setRows]);

  // Handle ?createSecret=1 URL flag. Gated on `canCreate` (#402): the flag is
  // reachable from a bookmark and from the global create menu, and the row it
  // opens ends in a POST. Ungated, a viewer types a name and a value, presses
  // save, and watches the row vanish while the POST answers 403. It waits for
  // a settled, non-failed list too: a row opened in the error state is
  // invisible until recovery. `flagOpenedFor`: one row per project.
  const flagOpenedFor = useRef<string | null>(null);
  useEffect(() => {
    if (!shouldCreate) {
      flagOpenedFor.current = null;
      return;
    }
    if (projectId && canAddSecret && !isFetching && flagOpenedFor.current !== projectId) {
      flagOpenedFor.current = projectId;
      setRows((prev) => [newSecretRow(), ...prev]);
    }
  }, [shouldCreate, projectId, canAddSecret, isFetching, setRows]);

  // Filtered rows — client-side search filter (matches old-app pattern)
  const filteredRows = useMemo(() => {
    if (!search.trim()) return rows;
    const needle = search.trim().toLowerCase();
    return rows.filter((r) => r.name.toLowerCase().includes(needle));
  }, [rows, search]);

  // Fall back to an empty row set on a list-fetch error rather than
  // showing stale data (matches the baseline's `rows={isError ? [] :
  // secretRows}`, `SecretsContent.jsx:167`).
  const tableRows = isError ? [] : filteredRows;
  // A settled FAILURE is not an empty project (UI-UX-1(b)): the grid's "No
  // secrets" overlay invites the create the backend refuses. A retry that is
  // in flight shows the table's loading skeletons instead.
  const showLoadError = isError && !isFetching;
  const retryList = useCallback(() => void refetch(), [refetch]);

  // Wire mutations to the hook
  const setMutations = actions.setMutations;

  // Memoize the mutations wrapper so the hook doesn't get a new ref every render
  const mutationsWrapper = useMemo(
    () => ({
      createSecret: (name: string, value: string) => {
        createMutation.mutate(
          { name, value },
          {
            onError: (err: unknown) => {
              setToast({
                severity: 'error',
                message: isForbiddenError(err)
                  ? t('entities.secret.error.forbidden', 'The access is not allowed')
                  : t('entities.secret.error.createFailed', 'Failed to create secret'),
              });
            },
          },
        );
      },
      updateSecret: (oldName: string, name: string, value: string) => {
        updateMutation.mutate(
          { name: oldName, params: { name, value } },
          {
            onError: (err: unknown) => {
              setToast({
                severity: 'error',
                message: isForbiddenError(err)
                  ? t('entities.secret.error.forbidden', 'The access is not allowed')
                  : t('entities.secret.error.updateFailed', 'Failed to update secret'),
              });
            },
          },
        );
      },
      deleteSecret: (name: string) => {
        deleteMutation.mutate(name);
      },
      hideSecret: (name: string) => {
        hideMutation.mutate(name);
      },
      showSecret: (name: string) => showSecret(projectId, name),
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [projectId, createMutation.mutate, updateMutation.mutate, deleteMutation.mutate, hideMutation.mutate],
  );

  useSyncSecretMutations(setMutations, mutationsWrapper);

  /* ── edit: fetch-then-blank wrapper ───────────────────────────────── *
   * Ported from the baseline's `handleEditClick` wrapper
   * (`SecretsTable.jsx:220-245`): `actions.onEdit` fetches the plaintext
   * and stores it on the row so `EditSecretInputGridTable` has something
   * to seed the edit session's diff from, but the edit *input* must start
   * empty, not pre-filled with the live secret. */
  const handleEditClick = useCallback(
    (rowId: string) => async () => {
      await actions.onEdit(rowId)();
      setRows((prev) => prev.map((r) => (r.id === rowId && !r.isNew ? { ...r, secretValue: '' } : r)));
    },
    [actions, setRows],
  );

  /* ── copy: always fetch the live plaintext ────────────────────────── *
   * Ported from the baseline's `handleDirectCopy`
   * (`SecretValueCell.jsx:15-31`): copy always re-fetches the secret via
   * the show endpoint and copies the real value, regardless of whether
   * the row is currently revealed in the UI. */
  const onCopySecretValue = useCallback(
    (rowId: string) => async () => {
      const row = rows.find((r) => r.id === rowId);
      if (!row?.name) return;
      try {
        const revealed = await showSecret(projectId, row.name);
        await handleCopy(revealed.value);
        setToast({ severity: 'success', message: t('entities.secret.copySuccess', 'The secret has been copied to the clipboard') });
      } catch {
        setToast({ severity: 'error', message: t('entities.secret.error.copyFailed', 'Failed to copy to clipboard') });
      }
    },
    [rows, projectId],
  );

  /* ── styles ───────────────────────────────────────────────────────── */
  const styles = getStyles();

  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader
        title={t('entities.secret.pageTitle', 'Secrets')}
        showSearchInput
        showAddButton={canAddSecret}
        slotProps={{
          searchInput: {
            search,
            onChangeSearch: onSearchChange,
            placeholder: t('entities.secret.searchPlaceholder', 'Search secrets'),
          },
          addButton: {
            onAdd: () => setRows((prev) => [newSecretRow(), ...prev]),
            disabled: isFetching,
            tooltip: t('entities.secret.addTooltip', 'Create new secret'),
          },
        }}
      />
      <Box sx={styles.content}>
        {showLoadError ? (
          <SecretsLoadError forbidden={isForbiddenError(error)} onRetry={retryList} />
        ) : (
          <SecretsTable
            rows={tableRows}
            setRows={setRows}
            rowModesModel={actions.rowModesModel}
            setRowModesModel={actions.setRowModesModel}
            isFetching={isFetching}
            isShowSecretMap={actions.isShowSecretMap}
            permissions={secretPermissions}
            validationErrors={actions.validationErrors}
            onValidationChange={actions.onValidationChange}
            actions={{
              onSave: actions.onSave,
              onCancel: actions.onCancel,
              onShowSecret: actions.onShowSecret,
              onHideSecret: actions.onHideSecret,
              onCopySecretValue,
              onActionsMenuClick: actions.onActionsMenuClick,
              onEdit: handleEditClick,
              onHide: actions.onHide,
              onDelete: actions.onDelete,
              onCloseAlert: actions.onCloseAlert,
              onConfirmAlert: actions.onConfirmAlert,
            }}
            menu={{
              anchorEl: actions.anchorEl,
              anchorRowId: actions.anchorRowId,
              onCloseMenu: actions.onActionsMenuClose,
            }}
            dialog={{
              openAlert: actions.openAlert,
              openAlertType: actions.openAlertType,
            }}
          />
        )}
      </Box>
      <Snackbar
        open={toast !== null}
        autoHideDuration={3000}
        onClose={closeToast}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        {toast ? (
          <Alert onClose={closeToast} severity={toast.severity} variant="filled">
            {toast.message}
          </Alert>
        ) : undefined}
      </Snackbar>
    </Paper>
  );
});

const getStyles = (): Record<string, SxProps<Theme>> => ({
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
    borderRadius: 'var(--el-shape-radiusSm, 0px)',
  },
  content: {
    flex: 1,
    minHeight: 0,
    padding: '0 1.5rem 1.5rem',
    overflow: 'auto',
  },
});
