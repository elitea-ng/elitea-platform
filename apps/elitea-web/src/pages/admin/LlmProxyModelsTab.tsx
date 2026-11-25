/**
 * Admin › Configuration › LLM Proxy › Models & pricing: the catalogue tab,
 * moved out of `./AdminLlmProxyEditor.tsx` unchanged so the editor stays
 * under the 400-line cap (R3.4b, issue 2613).
 */
import { useDeferredValue, useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { ADMIN_ITEM } from './adminItemIds';
import { useAdminWritable } from './adminWriteAccess';
import { ModelCatalogueTable, UnpricedModelsAlert, UsageWindowSelect } from './LlmProxyModelsPanel';
import { LlmProxyPriceDialog } from './LlmProxyPriceDialog';
import { configFailureReason } from './api/adminConfigurationApi';
import {
  useAdminLlmModels,
  useClearAdminLlmModelOverride,
  useSaveAdminLlmModelPrice,
  type LlmModelPriceDraft,
  type LlmModelRow,
  type UnpricedLlmModel,
  type UsageWindow,
} from './api/adminLlmProxyApi';

/** What the price dialog is currently open on, if anything. */
interface PriceEditorState {
  readonly row: LlmModelRow | undefined;
  readonly unpriced: UnpricedLlmModel | undefined;
}

/**
 * The three ways the catalogue tab can be in trouble at once.
 *
 * Extracted so `ModelsTab` stays its own state machine rather than a stack of
 * conditionals — the same complexity-gate split `AdminMcpServersEditor` made for
 * `McpCatalogueAlerts`.
 *
 * Each renders the SERVER's own sentence where it gave one. On this surface the
 * refusals are specific and actionable ("a price cannot be negative", "an
 * override that prices nothing"), and collapsing them into a generic failure
 * would discard the only words that say what to change.
 */
function ModelCatalogueAlerts({
  readError,
  loadError,
  clearError,
  unpricedError,
}: {
  /** The reason the server attached to an otherwise-200 catalogue read. */
  readonly readError: string | undefined;
  readonly loadError: unknown;
  readonly clearError: unknown;
  /** Why the unpriced check could not run. */
  readonly unpricedError: string | undefined;
}) {
  return (
    <>
      {/* Stated explicitly, because an unpriced report that FAILED renders the
          same as one that found nothing — no alert at all — and "no unpriced
          models" is the reassuring reading an operator would take from it. */}
      {unpricedError !== undefined ? (
        <Alert severity="warning" data-testid="llm-proxy-unpriced-error">
          {t(
            'pages.admin.llmProxy.models.unpricedError',
            'The check for called-but-unpriced models could not be run, so this page cannot say whether any exist: {{reason}}',
            { reason: unpricedError },
          )}
        </Alert>
      ) : null}
      {readError !== undefined ? (
        <Alert severity="warning" data-testid="llm-proxy-models-error">
          {readError}
        </Alert>
      ) : null}

      {loadError != null ? (
        <Alert severity="warning" data-testid="llm-proxy-models-load-error">
          {configFailureReason(loadError) ??
            t('pages.admin.llmProxy.models.loadError', 'Failed to load the model catalogue.')}
        </Alert>
      ) : null}

      {clearError != null ? (
        <Alert severity="error" data-testid="llm-proxy-clear-error">
          {configFailureReason(clearError) ??
            t('pages.admin.llmProxy.models.clearError', 'Failed to resume the price sync.')}
        </Alert>
      ) : null}
    </>
  );
}

/**
 * The catalogue's three terminal states: loading, nothing to show, or a page of
 * rows that may have been capped.
 *
 * Extracted so ModelsTab stays under the complexity gate and remains its own
 * state machine rather than a chain of nested ternaries.
 */
function ModelCatalogueResults({
  isPending,
  items,
  truncated,
  searching,
  onEdit,
  onClearOverride,
}: {
  readonly isPending: boolean;
  readonly items: readonly LlmModelRow[];
  readonly truncated: boolean;
  /** Whether a search term is active, which changes what "empty" means. */
  readonly searching: boolean;
  readonly onEdit: (row: LlmModelRow) => void;
  readonly onClearOverride: (row: LlmModelRow) => void;
}) {
  if (isPending) {
    return (
      <LinearProgress aria-label={t('pages.admin.llmProxy.models.loading', 'Loading models')} />
    );
  }

  if (items.length === 0) {
    // "No results for this search" and "the catalogue is empty" are different
    // facts, and the second is alarming while the first is routine. Conflating
    // them would tell an operator who mistyped a model name that their price
    // catalogue had vanished.
    return (
      <Typography variant="bodyMedium" color="text.secondary" data-testid="llm-proxy-models-empty">
        {searching
          ? t('pages.admin.llmProxy.models.noMatches', 'No models match that search.')
          : t(
              'pages.admin.llmProxy.models.empty',
              "The price catalogue is empty. It is normally filled by the automatic price sync; until it is, every recorded cost comes from the gateway's fallback rates rather than from real prices.",
            )}
      </Typography>
    );
  }

  return (
    <>
      {/* A capped page says so. A short list that looks complete is how an
          operator concludes a model is absent from the catalogue when it is
          merely past the cap — and then prices a duplicate. */}
      {truncated ? (
        <Alert severity="info" data-testid="llm-proxy-truncated">
          {t(
            'pages.admin.llmProxy.models.truncated',
            'Showing the first {{count}} models. Search to narrow the list.',
            { count: items.length },
          )}
        </Alert>
      ) : null}
      <ModelCatalogueTable items={items} onEdit={onEdit} onClearOverride={onClearOverride} />
    </>
  );
}

/**
 * The Models tab.
 *
 * Extracted so the editor stays a tab shell and this stays the catalogue's own
 * state machine — the split the complexity gate forced on the MCP editor, for
 * the same reason.
 */
export function ModelsTab() {
  // `usageWindow`, not `window`: the latter would shadow the DOM global for the
  // whole of this function.
  const [usageWindow, setUsageWindow] = useState<UsageWindow>('24h');
  const [search, setSearch] = useState('');
  const [editor, setEditor] = useState<PriceEditorState | undefined>(undefined);

  // The search term reaches the SERVER, because the catalogue is capped there:
  // filtering only what was already returned would silently exclude every model
  // past the cap, which is the failure the cap itself has to avoid.
  const deferredSearch = useDeferredValue(search);
  const { data, isPending, error } = useAdminLlmModels(usageWindow, deferredSearch);
  const access = useAdminWritable(ADMIN_ITEM.modelsPricing);
  const savePrice = access.guard(useSaveAdminLlmModelPrice());
  const clearOverride = access.guard(useClearAdminLlmModelOverride());

  const items = data?.items ?? [];
  const unpriced = data?.unpriced ?? [];

  // The server's own sentence when it gave one. These refusals are specific
  // ("a price cannot be negative"), and a generic "failed to save" would discard
  // the only words that say what to change.
  const saveError = useMemo(
    () =>
      savePrice.error != null
        ? (configFailureReason(savePrice.error) ??
          t('pages.admin.llmProxy.models.saveError', 'Failed to save the model price.'))
        : undefined,
    [savePrice.error],
  );

  const onSave = (draft: LlmModelPriceDraft) => {
    savePrice.mutate(draft, { onSuccess: () => setEditor(undefined) });
  };

  // Closing DISCARDS the previous failure. `savePrice.error` survives until the
  // next mutate(), so without this a refusal from one row ("a price cannot be
  // negative") would still be sitting at the top of the dialog when the operator
  // opened a different row — an error about a save that is not the one in front
  // of them.
  const onCloseEditor = () => {
    setEditor(undefined);
    savePrice.reset();
  };

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.llmProxy.models.intro',
          'Every billed request is costed against this catalogue. A model with no row here is not refused: text requests are billed at a rate the gateway invents, and audio requests are billed at nothing.',
        )}
      </Typography>

      <Box
        sx={{
          display: 'flex',
          gap: '1rem',
          alignItems: 'center',
          flexWrap: 'wrap',
        }}
      >
        <UsageWindowSelect usageWindow={usageWindow} onChange={setUsageWindow} />
        <TextField
          size="small"
          label={t('pages.admin.llmProxy.models.search', 'Search models')}
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          sx={{ minWidth: '16rem' }}
          slotProps={{ htmlInput: { 'data-testid': 'llm-proxy-search' } }}
        />
        <Button
          variant="elitea" color="primary"
          size="small"
          disabled={!access.writable}
          onClick={() => setEditor({ row: undefined, unpriced: undefined })}
          data-testid="llm-proxy-add-price"
        >
          {t('pages.admin.llmProxy.models.add', 'Add a model price')}
        </Button>
      </Box>

      <ModelCatalogueAlerts
        readError={data?.error}
        loadError={error}
        clearError={clearOverride.error}
        unpricedError={data?.unpriced_error}
      />

      <UnpricedModelsAlert
        unpriced={unpriced}
        onPrice={(model) => setEditor({ row: undefined, unpriced: model })}
      />

      <ModelCatalogueResults
        isPending={isPending}
        items={items}
        truncated={data?.truncated === true}
        searching={deferredSearch !== ''}
        onEdit={(row) => setEditor({ row, unpriced: undefined })}
        onClearOverride={(row) => clearOverride.mutate(row.id)}
      />

      <LlmProxyPriceDialog
        open={editor !== undefined}
        row={editor?.row}
        unpriced={editor?.unpriced}
        saving={savePrice.isPending}
        error={saveError}
        onClose={onCloseEditor}
        onSave={onSave}
      />
    </Box>
  );
}
