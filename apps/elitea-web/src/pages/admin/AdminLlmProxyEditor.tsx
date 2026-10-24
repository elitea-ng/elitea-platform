/**
 * Admin › Configuration › LLM Proxy — the section editor.
 *
 * ## What this section is
 *
 * The screen that stands where **LiteLLM** stood. LiteLLM is gone: ADR-0015
 * replaced it with `services/elitea-llm-gateway`, a standalone service built on
 * maximhq/bifrost's core. The old section's fields all described that removed
 * subsystem — which LiteLLM to talk to, its master key, its database, and three
 * buttons that reconciled teams and keys inside it — so none of them is carried
 * over. A control whose subject no longer exists is not a control worth porting.
 *
 * What replaces them is the subset of **Bifrost's own admin UI** that this
 * platform can back with real data:
 *
 *   | Bifrost | Here | Backed by |
 *   | --- | --- | --- |
 *   | Observability / health | **Status** | the gateway's `GET /governance/status` |
 *   | Model Catalog + Pricing Overrides | **Models** | `gateway.gateway_models` + `llm_usage_events` |
 *   | Usage | **Usage** | `gateway.llm_usage_events` |
 *   | Logs / request inspection | **Logs** | `gateway.llm_request_logs` |
 *   | Providers & keys | **Providers & models** | the public project's shared rows |
 *   | Alerting | **Alerts** | `gateway.governance_config` soft-alert row |
 *
 * ## What is deliberately not here, and why
 *
 * Bifrost's UI has thirty-odd pages. Most of the rest cannot be served honestly
 * from this platform, and a screen that renders a control over nothing is worse
 * than its absence — it reports success for a setting nothing reads:
 *
 *   - **Logs / request inspection.** STALE CLAIM, corrected: this said no
 *     request-log store exists, and that building one "means a new table and a
 *     gateway writer before any screen is worth drawing". That was the right
 *     diagnosis and both now exist — shared migration 0099 and the gateway's
 *     `internal/requestlog`, which records one row per request off the request
 *     path. The Logs tab reads it.
 *
 *     What the tab still cannot show is the PAYLOAD, and that is structural
 *     rather than pending: the table has no column a prompt, a completion or an
 *     upstream error string could reach. The tab says so rather than leaving an
 *     operator to discover it.
 *   - **Providers.** STALE CLAIM, corrected. This said provider credentials
 *     are per-project rows "not global gateway config, so they do not belong on
 *     a platform screen". The premise was half right and the conclusion was
 *     wrong: the gateway has resolved a PLATFORM scope since issue #316 — the
 *     public project's `shared = true` rows, usable by every project — and the
 *     only thing missing was a surface to author them. The Providers tab is
 *     that surface; a project's own credentials stay where they are, and both
 *     still resolve.
 *   - **Virtual keys.** Bifrost's virtual-key slot carries the Elitea project
 *     id. There is no key to mint, rotate or revoke.
 *   - **Retries, timeouts, fallback chains, key weights, proxy, semantic cache.**
 *     Not configurable anywhere: the gateway leaves them at Bifrost's defaults
 *     and its `Account` implementation cannot vary them per project. Exposing
 *     them would mean inventing storage and a read path first.
 *
 * ## Governance is authored next door, not here
 *
 * Budgets, rate limits, allowlists, credential policy and CEL routing rules are
 * rows, and `/admin/app/governance` is their editor. This section reports
 * whether the gateway ACCEPTED them, which is the half that page cannot show.
 *
 * The page reaches this component through a server-declared `managed_surface`,
 * never through a hardcoded section id — see `./Configuration.tsx`.
 */
import { useDeferredValue, useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { LlmProxyAlertsPanel } from './LlmProxyAlertsPanel';
import { ModelCatalogueTable, UnpricedModelsAlert, UsageWindowSelect } from './LlmProxyModelsPanel';
import { LlmProxyPriceDialog } from './LlmProxyPriceDialog';
import { LlmProxyLogsPanel } from './LlmProxyLogsPanel';
import { LlmProxyProvidersPanel } from './LlmProxyProvidersPanel';
import { LlmProxyStatusPanel } from './LlmProxyStatusPanel';
import { LlmProxyToolPricesPanel } from './LlmProxyToolPricesPanel';
import { LlmProxyUsagePanel } from './LlmProxyUsagePanel';
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

type TabId = 'status' | 'providers' | 'models' | 'tools' | 'usage' | 'logs' | 'alerts';

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
function ModelsTab() {
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
  const savePrice = useSaveAdminLlmModelPrice();
  const clearOverride = useClearAdminLlmModelOverride();

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

export function AdminLlmProxyEditor() {
  const [tab, setTab] = useState<TabId>('status');

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }}>
      <Tabs
        value={tab}
        onChange={(_event, next: TabId) => setTab(next)}
        data-testid="llm-proxy-tabs"
      >
        <Tab value="status" label={t('pages.admin.llmProxy.tab.status', 'Status')} />
        <Tab
          value="providers"
          label={t('pages.admin.llmProxy.tab.providers', 'Providers & models')}
        />
        <Tab value="models" label={t('pages.admin.llmProxy.tab.models', 'Models & pricing')} />
        <Tab value="tools" label={t('pages.admin.llmProxy.tab.tools', 'Tools')} />
        <Tab value="usage" label={t('pages.admin.llmProxy.tab.usage', 'Usage')} />
        <Tab value="logs" label={t('pages.admin.llmProxy.tab.logs', 'Logs')} />
        <Tab value="alerts" label={t('pages.admin.llmProxy.tab.alerts', 'Budget alerts')} />
      </Tabs>

      {tab === 'status' ? <LlmProxyStatusPanel /> : null}
      {tab === 'providers' ? <LlmProxyProvidersPanel /> : null}
      {tab === 'models' ? <ModelsTab /> : null}
      {tab === 'tools' ? <LlmProxyToolPricesPanel /> : null}
      {tab === 'usage' ? <LlmProxyUsagePanel /> : null}
      {tab === 'logs' ? <LlmProxyLogsPanel /> : null}
      {tab === 'alerts' ? <LlmProxyAlertsPanel /> : null}
    </Box>
  );
}
