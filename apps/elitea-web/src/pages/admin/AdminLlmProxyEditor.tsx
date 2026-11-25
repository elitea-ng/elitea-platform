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
import { useState } from 'react';

import Box from '@mui/material/Box';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';

import { t } from '@/shared/i18n';

import { AdminWritableSection } from './AdminWritable';
import { ADMIN_ITEM } from './adminItemIds';
import { LlmProxyAlertsPanel } from './LlmProxyAlertsPanel';
import { LlmProxyLogsPanel } from './LlmProxyLogsPanel';
import { ModelsTab } from './LlmProxyModelsTab';
import { LlmProxyProvidersPanel } from './LlmProxyProvidersPanel';
import { LlmProxyStatusPanel } from './LlmProxyStatusPanel';
import { LlmProxyToolPricesPanel } from './LlmProxyToolPricesPanel';
import { LlmProxyUsagePanel } from './LlmProxyUsagePanel';

type TabId = 'status' | 'providers' | 'models' | 'tools' | 'usage' | 'logs' | 'alerts';

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
      {/* Each tab that writes is its own console item (R3.4b, issue 2613). */}
      {tab === 'providers' ? (
        <AdminWritableSection itemId={ADMIN_ITEM.providersModels}>
          <LlmProxyProvidersPanel />
        </AdminWritableSection>
      ) : null}
      {tab === 'models' ? (
        <AdminWritableSection itemId={ADMIN_ITEM.modelsPricing}>
          <ModelsTab />
        </AdminWritableSection>
      ) : null}
      {tab === 'tools' ? (
        <AdminWritableSection itemId={ADMIN_ITEM.toolPrices}>
          <LlmProxyToolPricesPanel />
        </AdminWritableSection>
      ) : null}
      {tab === 'usage' ? <LlmProxyUsagePanel /> : null}
      {tab === 'logs' ? <LlmProxyLogsPanel /> : null}
      {tab === 'alerts' ? (
        <AdminWritableSection itemId={ADMIN_ITEM.budgetsAlerts}>
          <LlmProxyAlertsPanel />
        </AdminWritableSection>
      ) : null}
    </Box>
  );
}
