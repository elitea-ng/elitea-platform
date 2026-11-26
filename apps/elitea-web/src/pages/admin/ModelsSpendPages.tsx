/**
 * Admin › Models and spend: the seven tabs of the LLM gateway editor as
 * pages of their own (ADR-0059 decision 6, "the seven gateway tabs ...
 * become items"; R3.5a, issue 2535; finding C5).
 *
 * The panels are the ones `./AdminLlmProxyEditor.tsx` mounts as tabs of the
 * Configuration page, unchanged: this module changes where they mount, not
 * what they do. Each page that writes sits under the read-only wrapper of
 * its own console item, as its tab did (R3.4b, issue 2613).
 *
 * Five of the seven are navigation items. The other two follow decision 6,
 * which lists seven items for the area and no Status item:
 *
 *  - Status (`/models/status`) is a tab at the head of Usage, the Usage
 *    item's page;
 *  - Budget alerts (`/budgets/alerts`) is a tab of "Budgets and alerts",
 *    beside the Budgets page.
 *
 * `./AdminItemTabs.tsx` draws those two strips. The server still declares
 * the `llm_proxy` managed surface until R4.9 (issue 2623): with the
 * `admin_console_areas` switch off the Configuration page keeps mounting the
 * editor, and with it on its section row links here.
 */
import type { ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminWritableSection } from './AdminWritable';
import { ADMIN_ITEM } from './adminItemIds';
import { LlmProxyAlertsPanel } from './LlmProxyAlertsPanel';
import { LlmProxyLogsPanel } from './LlmProxyLogsPanel';
import { ModelsTab } from './LlmProxyModelsTab';
import { LlmProxyProvidersPanel } from './LlmProxyProvidersPanel';
import { LlmProxyStatusPanel } from './LlmProxyStatusPanel';
import { LlmProxyToolPricesPanel } from './LlmProxyToolPricesPanel';
import { LlmProxyUsagePanel } from './LlmProxyUsagePanel';

interface ModelsSpendPageProps {
  readonly testId: string;
  readonly title: string;
  /** The console item whose level the page's controls read; absent for a page that only reads. */
  readonly itemId?: string;
  readonly children: ReactNode;
}

function ModelsSpendPage({ testId, title, itemId, children }: ModelsSpendPageProps): ReactNode {
  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }} data-testid={testId}>
      <Typography variant="headingLarge" component="h1">
        {title}
      </Typography>
      {itemId === undefined ? children : <AdminWritableSection itemId={itemId}>{children}</AdminWritableSection>}
    </DrawerPage>
  );
}

export function AdminLlmProviders(): ReactNode {
  return (
    <ModelsSpendPage
      testId="admin-llm-providers-page"
      title={t('pages.admin.console.item.providers-models', 'Providers and models')}
      itemId={ADMIN_ITEM.providersModels}
    >
      <LlmProxyProvidersPanel />
    </ModelsSpendPage>
  );
}

export function AdminLlmModelsPricing(): ReactNode {
  return (
    <ModelsSpendPage
      testId="admin-llm-models-pricing-page"
      title={t('pages.admin.console.item.models-pricing', 'Models and pricing')}
      itemId={ADMIN_ITEM.modelsPricing}
    >
      <ModelsTab />
    </ModelsSpendPage>
  );
}

export function AdminLlmToolPrices(): ReactNode {
  return (
    <ModelsSpendPage
      testId="admin-llm-tool-prices-page"
      title={t('pages.admin.console.item.tool-prices', 'Tool prices')}
      itemId={ADMIN_ITEM.toolPrices}
    >
      <LlmProxyToolPricesPanel />
    </ModelsSpendPage>
  );
}

export function AdminLlmUsage(): ReactNode {
  return (
    <ModelsSpendPage testId="admin-llm-usage-page" title={t('pages.admin.console.item.usage', 'Usage')}>
      <LlmProxyUsagePanel />
    </ModelsSpendPage>
  );
}

export function AdminLlmStatus(): ReactNode {
  return (
    <ModelsSpendPage testId="admin-llm-status-page" title={t('pages.admin.llmProxy.page.status', 'Gateway status')}>
      <LlmProxyStatusPanel />
    </ModelsSpendPage>
  );
}

export function AdminLlmLogs(): ReactNode {
  return (
    <ModelsSpendPage testId="admin-llm-logs-page" title={t('pages.admin.console.item.logs', 'Logs')}>
      <LlmProxyLogsPanel />
    </ModelsSpendPage>
  );
}

export function AdminBudgetAlerts(): ReactNode {
  return (
    <ModelsSpendPage
      testId="admin-budget-alerts-page"
      title={t('pages.admin.llmProxy.tab.alerts', 'Budget alerts')}
      itemId={ADMIN_ITEM.budgetsAlerts}
    >
      <LlmProxyAlertsPanel />
    </ModelsSpendPage>
  );
}
