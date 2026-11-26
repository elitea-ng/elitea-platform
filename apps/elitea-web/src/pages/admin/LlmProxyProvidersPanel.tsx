/**
 * Admin › Configuration › LLM Proxy — **Providers**.
 *
 * ## What this screen makes possible
 *
 * Publishing an LLM provider credential once, for the whole platform, instead of
 * once per project. That was always what the gateway resolved — the caller's
 * project plus the public project's `shared = true` rows (issue #316) — but the
 * only way to author one was to open the public project's ordinary settings
 * screen, know that it was the global scope, and be a member of it.
 *
 * ## `status_ok` is the column that decides whether anything happens
 *
 * The gateway admits `status_ok = true` and nothing else. Provider admission
 * runs on every write, so a credential whose endpoint or key does not resolve is
 * stored, listed, and completely inert — a state that has no other display
 * anywhere in the product. It is therefore the first column, not a detail.
 *
 * ## Unsealed secrets are reported as findings
 *
 * A write through this platform seals the key into the vault and stores a
 * reference. A row imported from a legacy deployment can still hold a literal,
 * and that value is readable by every holder of the project-scoped configuration
 * permissions on the public project. The server reports `sealed: false` for such
 * a row without ever sending the value, and this panel surfaces it — re-saving
 * the credential is the fix.
 */
import type { ReactNode } from 'react';
import { useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Divider from '@mui/material/Divider';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { LlmProviderAdoptModelsDialog } from './LlmProviderAdoptModelsDialog';
import { LlmProviderDialog } from './LlmProviderDialog';
import {
  ProviderAlerts,
  ProviderResults,
  type ProviderRowActions,
} from './LlmProviderTableParts';
import { PlatformModelsPanel } from './PlatformModelsPanel';
import { providerDeleteRequest, providerSaveRequest } from './llmProviderDangerous';
import { useAdminDangerousConfirm } from './useAdminDangerousConfirm';
import { ADMIN_ITEM } from './adminItemIds';
import { useAdminWritable } from './adminWriteAccess';
import { configFailureReason } from './api/adminConfigurationApi';
import { SharedScopeWarning } from './LlmProviderScopeWarning';
import {
  useAdminLlmProviders,
  useCheckAdminLlmProvider,
  useCreateAdminLlmProvider,
  useDeleteAdminLlmProvider,
  useRevalidateAdminLlmProvider,
  useUpdateAdminLlmProvider,
  type LlmProvider,
  type LlmProviderCheckResult,
  type LlmProviderDraft,
} from './api/adminLlmProvidersApi';

/** A stable empty list, so a pending query does not invalidate every memo. */
const EMPTY_PROVIDERS: readonly LlmProvider[] = [];

/** The rows holding a secret this platform did not seal. */
function unsealedProviders(items: readonly LlmProvider[]): readonly LlmProvider[] {
  return items.filter((item) => item.secrets.some((secret) => !secret.sealed));
}

type CheckResultsUpdater = (
  updater: (prev: Record<number, LlmProviderCheckResult>) => Record<number, LlmProviderCheckResult>,
) => void;

/**
 * Starts one row's live check and routes both outcomes into the panel's local
 * `checkResults` map. A free function — not inlined into the component — so
 * `LlmProxyProvidersPanel` itself stays within the §3.5 complexity budget as
 * the row grew a second action.
 *
 * `onError` fires only for what `useCheckAdminLlmProvider` could not resolve
 * into `{success,message}` itself — a genuine transport failure, not the
 * check route's own documented 400/404 (that module's own header explains why
 * those resolve rather than throw). Rendering it the same way as a real
 * failure, rather than a separate top-level alert, keeps every check verdict
 * in the one place an operator is already looking: this row.
 */
function runProviderCheck(
  check: { mutate: (id: number, options: { onSuccess: (result: LlmProviderCheckResult) => void; onError: (error: unknown) => void }) => void },
  setCheckResults: CheckResultsUpdater,
  row: LlmProvider,
): void {
  setCheckResults((prev) => {
    // Cleared, not left stale, while the new round trip is in flight — a
    // holdover "Connected" from the last press must not sit under a request
    // that has not answered yet.
    if (!(row.id in prev)) return prev;
    const next = { ...prev };
    delete next[row.id];
    return next;
  });
  check.mutate(row.id, {
    onSuccess: (result) => setCheckResults((prev) => ({ ...prev, [row.id]: result })),
    onError: (checkError) =>
      setCheckResults((prev) => ({
        ...prev,
        [row.id]: {
          success: false,
          message: configFailureReason(checkError) ?? t('pages.admin.llmProviders.checkFailed', 'Live check failed'),
        },
      })),
  });
}

/** The row a mutation is currently running against, or `undefined` when it is idle — the `pendingIds` pattern `useAdminAppRequestsPage.ts` uses, narrowed to one id since only one row can be mid-flight per action here. */
function pendingRowId(mutation: { readonly isPending: boolean; readonly variables: number | undefined }): number | undefined {
  return mutation.isPending ? mutation.variables : undefined;
}

export function LlmProxyProvidersPanel(): ReactNode {
  const [editor, setEditor] = useState<{ readonly open: boolean; readonly row: LlmProvider | undefined }>({
    open: false,
    row: undefined,
  });
  // The provider whose catalogue is being read, or undefined when no adoption
  // is open. It is the row itself rather than an id because the dialog needs
  // the credential's TITLE: a platform model names its provider by title, and
  // the server refuses a link naming anything else.
  const [adoptFrom, setAdoptFrom] = useState<LlmProvider | undefined>(undefined);

  const { data, isPending, error } = useAdminLlmProviders();
  const access = useAdminWritable(ADMIN_ITEM.providersModels);
  const createProvider = access.guard(useCreateAdminLlmProvider());
  const updateProvider = access.guard(useUpdateAdminLlmProvider());
  const deleteProvider = access.guard(useDeleteAdminLlmProvider());
  const checkProvider = access.guard(useCheckAdminLlmProvider());
  const revalidateProvider = access.guard(useRevalidateAdminLlmProvider());
  const confirm = useAdminDangerousConfirm();

  // This session's own live-check verdicts, kept locally rather than in the
  // query cache: a check writes NOTHING server-side (see the api module's own
  // header), so there is no row to invalidate or refetch, and a react-query
  // mutation only ever remembers its LAST call — one entry per row is what
  // lets a Test on row B leave row A's result on screen.
  const [checkResults, setCheckResults] = useState<Record<number, LlmProviderCheckResult>>({});

  const onCheck = (row: LlmProvider) => runProviderCheck(checkProvider, setCheckResults, row);
  const onRevalidate = (row: LlmProvider) => revalidateProvider.mutate(row.id);
  const rowActions: ProviderRowActions = {
    onCheck,
    onRevalidate,
    onAdopt: setAdoptFrom,
    checkResults,
    checkingId: pendingRowId(checkProvider),
    revalidatingId: pendingRowId(revalidateProvider),
  };

  // Both derived from `data.items`, which is stable between refetches, rather
  // than from a `?? []` fallback that is a fresh array on every render — the
  // dependency that made `useMemo` here recompute unconditionally.
  const items = useMemo(() => data?.items ?? EMPTY_PROVIDERS, [data?.items]);
  const unsealed = useMemo(() => unsealedProviders(items), [items]);

  // The server's own sentence when it gave one — its refusals here name the
  // problem ("not an LLM provider credential type the gateway can dispatch to",
  // a self-referential endpoint), and a generic "failed to save" would discard
  // exactly the words that say what to change.
  const saveFailure = createProvider.error ?? updateProvider.error;
  const saveError =
    saveFailure != null
      ? (configFailureReason(saveFailure) ??
        t('pages.admin.llmProviders.saveError', 'Failed to save the provider.'))
      : undefined;

  const onSubmit = (draft: LlmProviderDraft) => {
    const onSuccess = () => {
      setEditor({ open: false, row: undefined });
    };
    const editing = editor.row;
    confirm.ask({
      ...providerSaveRequest(draft.elitea_title, editing !== undefined),
      run: (reason) => {
        if (editing !== undefined) {
          // The TYPE is not resent on an edit: it cannot change (see the dialog),
          // and restating it would make a partial update carry a field the operator
          // did not touch.
          updateProvider.mutate(
            { id: editing.id, draft: { elitea_title: draft.elitea_title, data: draft.data }, reason },
            { onSuccess },
          );
          return;
        }
        createProvider.mutate({ draft, reason }, { onSuccess });
      },
    });
  };

  const onDelete = (row: LlmProvider) => {
    confirm.ask({
      ...providerDeleteRequest(row.elitea_title),
      run: (reason) => deleteProvider.mutate({ id: row.id, reason }),
    });
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.llmProviders.intro',
          'Provider credentials published here are available to every project on this deployment. A project can still configure its own; both resolve, and a project’s own credential is not replaced by these.',
        )}
      </Typography>

      <SharedScopeWarning publicProjectID={data?.public_project_id ?? 0} />

      <ProviderAlerts
        loadError={error}
        saveError={saveError}
        deleteError={deleteProvider.error}
        revalidateError={revalidateProvider.error}
        unsealed={unsealed}
      />

      <Box sx={{ display: 'flex', gap: '1rem', alignItems: 'center', flexWrap: 'wrap' }}>
        <Button
          variant="elitea" color="primary"
          size="small"
          data-testid="llm-providers-add"
          disabled={!access.writable}
          onClick={() => setEditor({ open: true, row: undefined })}
        >
          {t('pages.admin.llmProviders.add', 'Add a platform provider')}
        </Button>
        {data !== undefined && data.public_project_id > 0 ? (
          // Echoed because getting it wrong is the failure where every
          // credential is published correctly into a schema the gateway never
          // reads, and every other signal on this screen still looks right.
          <Typography variant="bodySmall" color="text.secondary">
            {t('pages.admin.llmProviders.publicProject', 'Shared project: {{id}}', {
              id: data.public_project_id,
            })}
          </Typography>
        ) : null}
      </Box>

      <ProviderResults
        isPending={isPending}
        failed={error != null}
        items={items}
        onEdit={(row) => setEditor({ open: true, row })}
        onDelete={onDelete}
        rowActions={rowActions}
      />

      {confirm.dialog}

      {/* Models sit under the providers on the SAME tab. A model names a
          credential, and the commonest mistake is a model naming one that is
          not there — two screens would put the cause and the effect a click
          apart. */}
      <Divider sx={{ marginTop: '0.5rem' }} />
      <PlatformModelsPanel />

      {/* The successor to legacy's `import_llm_models`: the provider's own
          catalogue, read on demand with the stored credential, adopted one
          model at a time by an operator rather than by a cron job. */}
      <LlmProviderAdoptModelsDialog
        provider={adoptFrom}
        onClose={() => setAdoptFrom(undefined)}
      />

      <LlmProviderDialog
        open={editor.open}
        providerTypes={data?.provider_types ?? []}
        editing={editor.row}
        isSaving={createProvider.isPending || updateProvider.isPending}
        serverError={saveError}
        onClose={() => {
          setEditor({ open: false, row: undefined });
          // The previous refusal is DISCARDED on close. Both mutations keep
          // their error until the next mutate(), so without this a failure from
          // one row would still be at the top of the dialog when the operator
          // opened a different one.
          createProvider.reset();
          updateProvider.reset();
        }}
        onSubmit={onSubmit}
      />
    </Box>
  );
}
