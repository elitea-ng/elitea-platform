/**
 * Journey: the LLM manager does its job in Models and spend, and never opens
 * Configuration (ADR-0059 decision 3, the `llm_manager` set and owner
 * decision 13; R3.5b, issue 2536).
 *
 * The persona is the R3.13a one (`STORAGE_STATE.personas.llm_manager`): it
 * holds exactly the built-in `llm_manager` role. Through the navigation of
 * the six areas it adds a platform provider, adopts one of the provider's
 * models, sets a project budget and saves the budget alert, each on the page
 * that was a tab of Configuration › LLM Proxy until R3.5a (issue 2535).
 *
 * Then the refusal the ADR names: the role writes governance rows but not
 * guardrails. The LLM Governance page shows the guardrail control disabled,
 * and the same session's direct API save of a guardrail row answers 403. The
 * 403 is the security assertion; the disabled control is presentation only
 * (decision 6, "Hiding is not enforcement").
 *
 * The provider listing. A provider's model list is an outbound call through
 * the LLM gateway, and the journeys stack runs no gateway and no mock model
 * server (`deploy/docker-compose.e2e-standalone.yml`). The provider here
 * points at an address nothing answers, and its listing
 * (`POST /admin/gateway/providers/{id}/models`) is answered by this test with
 * the one model id it adopts. The adoption itself is the server's: each
 * adopted model is `POST /admin/gateway/platform_models`, admitted on
 * `llm.catalogue.manage`.
 *
 * The budget alert is one row for the whole deployment and the budget is a
 * seeded project's. No other journey reads either, and each CI leg (one
 * engine) has a stack of its own, so the journey writes them directly and
 * puts back what it read.
 */
import { randomBytes } from 'node:crypto';

import { expect, request as playwrightRequest, test, type APIRequestContext, type Page } from '@playwright/test';

import { API_BASE, AUTOTEST_PREFIX } from '../../fixtures/api';
import { confirmDangerousAction } from '../../fixtures/dangerousAction';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

const PROVIDERS_URL = `${API_BASE}/admin/gateway/providers`;
const PLATFORM_MODELS_URL = `${API_BASE}/admin/gateway/platform_models`;
const BUDGET_ALERTS_URL = `${API_BASE}/admin/gateway/budget-alerts`;
const GOVERNANCE_URL = `${API_BASE}/admin/gateway/governance`;
/** A seeded team project (`scripts/e2e-stack.sh seed`). */
const BUDGET_PROJECT = 'e2e-team-active';

function budgetUrl(projectId: number): string {
  return `${API_BASE}/elitea_core/project_budget/administration/${String(projectId)}/budget`;
}

interface BudgetAlerts {
  readonly enabled: boolean;
  readonly threshold_pct: number;
}

interface ProjectBudget {
  readonly monthly_limit: number | null;
  readonly enabled: boolean;
  readonly soft_alert_pct?: number | null;
}

/** Deletes the provider and the platform models this run created, whatever state the test ended in. */
async function sweep(request: APIRequestContext, provider: string, model: string): Promise<void> {
  const models = await request.get(PLATFORM_MODELS_URL);
  if (models.status() === 200) {
    const items = ((await models.json()) as { items?: { id: number; elitea_title?: string }[] }).items ?? [];
    for (const row of items) {
      if (row.elitea_title === model) await request.delete(`${PLATFORM_MODELS_URL}/${String(row.id)}`);
    }
  }
  const providers = await request.get(PROVIDERS_URL);
  if (providers.status() !== 200) return;
  const items = ((await providers.json()) as { items?: { id: number; elitea_title?: string }[] }).items ?? [];
  for (const row of items) {
    if (row.elitea_title === provider) await request.delete(`${PROVIDERS_URL}/${String(row.id)}`);
  }
}

/** Puts a project budget back as it was read: deleted when the project had none. */
async function restoreBudget(
  request: APIRequestContext,
  projectId: number,
  before: ProjectBudget | undefined,
): Promise<void> {
  if (before === undefined || before.monthly_limit === null) {
    await request.delete(budgetUrl(projectId));
    return;
  }
  await request.put(budgetUrl(projectId), {
    data: {
      monthly_limit: before.monthly_limit,
      enabled: before.enabled,
      ...(before.soft_alert_pct == null ? {} : { soft_alert_pct: before.soft_alert_pct }),
    },
  });
}

async function readBudget(request: APIRequestContext, projectId: number): Promise<ProjectBudget | undefined> {
  const response = await request.get(budgetUrl(projectId));
  if (response.status() === 404) return undefined;
  expect(response.status(), await response.text()).toBe(200);
  return (await response.json()) as ProjectBudget;
}

/** Opens a page of the console by clicking its navigation item, and waits for its address. */
async function openItem(page: Page, itemId: string, path: string): Promise<void> {
  await page.getByTestId(`admin-nav-item-${itemId}`).click();
  await page.waitForURL((url) => url.pathname === `/admin/app${path}`, { timeout: 20_000 });
}

test('the LLM manager adds a provider, adopts a model, sets a budget and saves the alert, without Configuration', async ({
  browser,
}, testInfo) => {
  test.setTimeout(180_000);
  const tag = `${testInfo.project.name}_${randomBytes(4).toString('hex')}`;
  const providerName = `${AUTOTEST_PREFIX}llm_manager_provider_${tag}`;
  const modelId = `${AUTOTEST_PREFIX}llm-manager-model-${tag}`;

  const context = await browser.newContext({ storageState: STORAGE_STATE.personas.llm_manager });
  const request = await playwrightRequest.newContext({
    baseURL: BASE_URL,
    storageState: STORAGE_STATE.personas.llm_manager,
  });
  const visited: string[] = [];
  try {
    const page = await context.newPage();
    page.on('framenavigated', (frame) => {
      if (frame === page.mainFrame()) visited.push(new URL(frame.url()).pathname);
    });
    // The provider's listing, which needs the gateway this stack does not run.
    await page.route('**/admin/gateway/providers/*/models', (route) =>
      route.request().method() === 'POST'
        ? route.fulfill({ json: { models: [modelId], total: 1, truncated: false, type: 'open_ai' } })
        : route.fallback(),
    );

    await page.goto(`${BASE_URL}/admin/app/`, { waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('admin-nav')).toBeVisible({ timeout: 30_000 });
    // The role opens no Configuration section: the item is not offered.
    await expect(page.getByTestId('admin-nav-item-providers-models')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('admin-nav-item-configuration')).toHaveCount(0);

    // 1. Add a platform provider.
    await openItem(page, 'providers-models', '/models/providers');
    await expect(page.getByTestId('llm-providers-table').or(page.getByTestId('llm-providers-empty'))).toBeVisible({
      timeout: 20_000,
    });
    await page.getByTestId('llm-providers-add').click();
    await page.getByTestId('llm-provider-name').fill(providerName);
    await page.getByTestId('llm-provider-api_key').fill('sk-autotest-not-a-real-key');
    await page.getByTestId('llm-provider-api_base').fill('http://autotest.invalid/v1');
    const created = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname.endsWith('/admin/gateway/providers') && response.request().method() === 'POST',
    );
    await page.getByTestId('llm-provider-save').click();
    // A provider create is a dangerous action: the one dialog asks for a reason (R3.8b, issue 2614).
    await confirmDangerousAction(page, providerName);
    const createdResponse = await created;
    expect(createdResponse.status(), 'the server admits the create on llm.catalogue.manage').toBe(201);
    const providerId = ((await createdResponse.json()) as { id: number }).id;
    await expect(page.getByRole('row').filter({ hasText: providerName })).toBeVisible({ timeout: 20_000 });

    // 2. Adopt one of its models as a platform model.
    await page.getByTestId(`admin-provider-adopt-${String(providerId)}`).click();
    await page.getByRole('checkbox', { name: modelId, exact: true }).check();
    const adopted = page.waitForResponse(
      (response) =>
        new URL(response.url()).pathname.endsWith('/admin/gateway/platform_models') &&
        response.request().method() === 'POST',
    );
    await page.getByTestId('adopt-models-submit').click();
    expect((await adopted).status(), 'the server admits the adoption on llm.catalogue.manage').toBe(201);
    await expect(page.getByTestId('platform-models-table').getByText(modelId)).toBeVisible({ timeout: 20_000 });

    {
      // 3. Set a project budget.
      await openItem(page, 'budgets-alerts', '/budgets');
      await page.getByTestId('admin-budgets-search').fill(BUDGET_PROJECT);
      const edit = page
        .getByRole('row')
        .filter({ hasText: BUDGET_PROJECT })
        .getByTestId(/^admin-budgets-edit-/);
      await expect(edit).toBeVisible({ timeout: 20_000 });
      const projectId = Number((await edit.getAttribute('data-testid'))?.replace('admin-budgets-edit-', ''));
      const budgetBefore = await readBudget(request, projectId);
      try {
        await edit.click();
        await page.getByTestId('admin-budget-limit').fill('4321');
        const saved = page.waitForResponse(
          (response) =>
            new URL(response.url()).pathname.endsWith(`/${String(projectId)}/budget`) &&
            response.request().method() === 'PUT',
        );
        await page.getByTestId('admin-budget-save').click();
        expect((await saved).status(), 'the server admits the budget on the budgets edit permission').toBe(200);
        expect((await readBudget(request, projectId))?.monthly_limit).toBe(4321);
      } finally {
        await restoreBudget(request, projectId, budgetBefore);
      }

      // 4. Save the budget alert, a tab of Budgets and alerts.
      const alertsRead = await request.get(BUDGET_ALERTS_URL);
      expect(alertsRead.status(), await alertsRead.text()).toBe(200);
      const alertsBefore = (await alertsRead.json()) as BudgetAlerts;
      const threshold = alertsBefore.threshold_pct === 77 ? 78 : 77;
      try {
        await page.getByTestId('admin-budgets-alerts-tab-alerts').click();
        await page.waitForURL((url) => url.pathname === '/admin/app/budgets/alerts', { timeout: 20_000 });
        await expect(page.getByTestId('admin-nav-item-budgets-alerts')).toHaveAttribute('aria-current', 'page');
        const field = page.getByTestId('llm-proxy-alerts-threshold');
        await expect(field).toHaveValue(String(alertsBefore.threshold_pct), { timeout: 20_000 });
        await field.fill(String(threshold));
        const saved = page.waitForResponse(
          (response) =>
            new URL(response.url()).pathname.endsWith('/admin/gateway/budget-alerts') &&
            response.request().method() === 'PUT',
        );
        await page.getByTestId('llm-proxy-alerts-save').click();
        expect((await saved).status(), 'the server admits the alert on llm.alerts.manage').toBe(200);
        expect(((await (await request.get(BUDGET_ALERTS_URL)).json()) as BudgetAlerts).threshold_pct).toBe(threshold);
      } finally {
        await request.put(BUDGET_ALERTS_URL, { data: alertsBefore });
      }
    }

    // 5. Guardrails are not the role's: the control is disabled, and the server refuses.
    await openItem(page, 'llm-governance', '/governance');
    await expect(page.getByTestId('governance-create')).toBeEnabled({ timeout: 20_000 });
    await expect(page.getByTestId('governance-guardrail-preset')).toBeDisabled();
    const refused = await request.post(GOVERNANCE_URL, {
      data: { type: 'guardrail', name: `${AUTOTEST_PREFIX}llm_manager_guardrail_${tag}`, data: {} },
    });
    expect(refused.status(), 'a guardrail save needs llm.guardrails.manage').toBe(403);

    // Never through Configuration.
    expect(visited.filter((path) => path.startsWith('/admin/app/configuration'))).toEqual([]);
  } finally {
    await context.close();
    await sweep(request, providerName, modelId);
    await request.dispose();
  }
});
