/**
 * Journey 24: Settings: analytics loads (JRNY-024)
 *
 * Spec §8.5 acceptance (from parity/manifest/analytics.json JRNY-024).
 * Acceptance: the dashboards render with project data;
 * loading failures show an error state instead of empty charts.
 *
 * ── WHAT CHANGED, AND WHY ────────────────────────────────────────────────
 * The previous revision of this file asserted
 *
 *     expect(hasError || hasCharts || hasHeading || hasDatePicker || hasMainContent)
 *
 * over five `.catch(() => false)`-guarded probes, plus a three-deep `.or()`
 * chain that whitelisted `getByRole('main')`. Every one of those disjuncts is
 * satisfied by a page that renders nothing but a `<main>` with the word
 * "Analytics" in it — i.e. by a stub. The route is not a stub
 * (`src/routes/_shell/settings/analytics.tsx` mounts the real
 * `AnalyticsContainer`, and `AnalyticsRepo` is now wired at
 * `services/elitea-main/internal/api/router.go:832-840`), so there was
 * nothing to hedge against; the hedges only ever hid regressions.
 *
 * Every assertion below is on something a stub cannot produce: a KPI value
 * read back out of the very HTTP response the app consumed, a six-column
 * table header, a formatter-transformed number (`1234` → `1.2K`), or an
 * error string that only appears on the `isError` branch.
 *
 * No `test.skip`, no early `return`, no `.catch(() => false)`, no `.or()`.
 */
import { test, expect } from '@playwright/test';
import type { Page, Locator } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { API_BASE, DEFAULT_PROJECT_ID } from '../../fixtures/api';

/** `GET /elitea_core/analytics/prompt_lib/{projectID}` — the Overview/Health fetch. */
const USAGE_RE = /\/api\/v2\/elitea_core\/analytics\/prompt_lib\/\d+(\?|$)/;
const USAGE_GLOB = '**/api/v2/elitea_core/analytics/prompt_lib/*';

/** `GET /elitea_core/analytics_users/prompt_lib/{projectID}` — the Users tab fetch. */
const USERS_RE = /\/api\/v2\/elitea_core\/analytics_users\/prompt_lib\/\d+(\?|$)/;

/**
 * A verbatim copy of `src/features/analytics/lib/format.ts`'s `fmtNum`.
 * Copied, not imported: `e2e/` is compiled by Playwright without the app's
 * `@/` path aliases, and an assertion that re-used the app's own formatter
 * would pass for free if that formatter regressed. This is the independent
 * oracle the KPI values are checked against.
 */
function fmtNum(n: number | null | undefined): string {
  if (n == null) return '0';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return String(n);
}

/**
 * The `KpiCard` root Box (`ui/components/KpiCard.tsx:67`) is the parent of
 * the label `<Typography>`; value/suffix/badge/subtitle are its siblings.
 */
function kpi(page: Page, label: string): Locator {
  return page.getByText(label, { exact: true }).locator('..');
}

/**
 * The KPI block, as the server sends it.
 *
 * EVERY FIELD IS OPTIONAL, and that is the contract rather than convenience.
 * The endpoint publishes a figure only when something in the deployment
 * produces it: `llm_calls`/`total_tokens`/`ai_active_users` come from the
 * gateway request log (shared migration 0099), `total_project_users` and
 * `adoption_rate` need the identity tables this service does not own, and
 * `tool_runs`/`chat_msgs`/`agent_runs`/`unique_users` have no producer at all
 * and are never sent. A required field here would let this journey assert a
 * number the server does not claim to have measured, which is exactly the
 * defect (#303) that made the previous revision of this file pass against a
 * dashboard of fabricated zeros.
 */
interface Kpis {
  readonly total_project_users?: number;
  readonly active_project_members?: number;
  readonly ai_active_users?: number;
  readonly adoption_rate?: number;
  readonly llm_calls?: number;
  readonly total_tokens?: number;
}

/** Every tile label the row can render, for absence checks. */
const ALL_KPI_LABELS = ['AI ACTIVE', 'LLM CALLS', 'TOKENS', 'COST', 'TOOL RUNS', 'CHAT MSG', 'AGENT RUNS'] as const;

/**
 * Asserts the KPI row against one `kpis` object: a tile per figure the server
 * sent, and NO tile for a figure it did not.
 *
 * The second half is the load-bearing one. A row that printed 0 for an absent
 * figure would satisfy any "the dashboard rendered" check while asserting a
 * count nothing measured — which is the same defect one layer up from the
 * handler that used to hardcode those zeros.
 */
async function expectKpiRow(page: Page, k: Kpis): Promise<void> {
  const aiActive = kpi(page, 'AI ACTIVE');
  if (k.ai_active_users !== undefined) {
    await expect(aiActive.getByText(fmtNum(k.ai_active_users), { exact: true })).toBeVisible();
  }
  if (k.total_project_users !== undefined) {
    await expect(aiActive.getByText(`of ${fmtNum(k.total_project_users)}`, { exact: true })).toBeVisible();
  }
  if (k.adoption_rate !== undefined && k.adoption_rate > 0) {
    await expect(aiActive.getByText(`${k.adoption_rate}% adoption`, { exact: true })).toBeVisible();
    // The numerator is a SUBSET of the denominator by construction — members
    // who called, over members — so the rate cannot exceed 100. The earlier
    // form divided every CALLER by the member count, and callers include
    // identities the membership table does not contain; a project with one
    // member and three non-member callers reported 300.
    expect(k.adoption_rate, 'adoption_rate is a percentage of a subset').toBeLessThanOrEqual(100);
  }
  if (k.llm_calls !== undefined) {
    await expect(kpi(page, 'LLM CALLS').getByText(fmtNum(k.llm_calls), { exact: true })).toBeVisible();
  }
  if (k.total_tokens !== undefined) {
    await expect(kpi(page, 'TOKENS').getByText(fmtNum(k.total_tokens), { exact: true })).toBeVisible();
  }

  for (const label of ['TOOL RUNS', 'CHAT MSG', 'AGENT RUNS', 'TEAM'] as const) {
    await expect(page.getByText(label, { exact: true })).toHaveCount(0);
  }
}

/* ────────────────────────────────────────────────────────────────────────
 * J24 — the acceptance clause "the dashboards render with project data",
 * against the REAL backend. The oracle is the response body the app itself
 * received, captured off the wire; the assertion is that every one of the
 * six KPI tiles displays that response's corresponding field.
 * ──────────────────────────────────────────────────────────────────────── */
test('J24: settings: analytics renders the live backend\'s own usage figures', async ({ page }) => {
  const usage = page.waitForResponse(
    (r) => USAGE_RE.test(r.url()) && r.request().method() === 'GET',
    { timeout: 20_000 },
  );

  await page.goto(BASE_URL + '/app/settings/analytics');
  const response = await usage;

  // Chrome that only the real container renders (AnalyticsContainer.tsx:159-236).
  // 15s, not the 5s default: the header's name comes from the selected-project
  // store, which is populated by a mount effect reading persisted storage
  // (widgets/app-shell/model/useSelectedProject.hooks.ts:22-29) — so this text
  // lands a render after the route body, and webkit on a loaded runner is the
  // slow case. Headroom for a slow boot, NOT a correctness race: which NAME
  // gets stored is no longer order-dependent (issue #161 — AppShell and
  // ProjectSwitcher both resolve it from the project list now).
  await expect(page.getByText('Project: Default Project', { exact: true })).toBeVisible({ timeout: 15_000 });
  for (const preset of ['Last 24h', 'Last 7d', 'Last 30d', 'Last 90d']) {
    await expect(page.getByRole('button', { name: preset, exact: true })).toBeVisible();
  }
  // The two `DateRangeField` MUI DateTimePickers (AnalyticsContainer.tsx:186-205)
  // render five segmented spinbuttons each (MM/DD/YYYY hh:mm) — ten in total.
  await expect(page.getByText('From:', { exact: true })).toBeVisible();
  await expect(page.getByText('To:', { exact: true })).toBeVisible();
  await expect(page.getByRole('spinbutton')).toHaveCount(10);
  // The tab list, in the reference deployment's order. Costs and Tokens sit
  // second and third there, so they were inserted rather than appended, and
  // this list is the assertion that catches an insertion that renumbered the
  // switch without renumbering the labels.
  for (const tab of ['Overview', 'Costs', 'Tokens', 'Agents', 'Tools', 'Users', 'Health', 'Guide']) {
    await expect(page.getByRole('tab', { name: tab, exact: true })).toBeVisible();
  }

  // ── The acceptance clause, first half: "the dashboards render with project
  //    data."
  //
  // THIS ASSERTION HAS BEEN WRONG IN BOTH DIRECTIONS, and the history is the
  // reason it is written the way it is now.
  //
  // It began as `expectKpiRow(page, payload.kpis)` against a 200. It passed,
  // and what it proved was that six tiles displayed six numbers the backend had
  // never computed: `Usage()` hardcoded `unique_users: 0`, `tool_runs: 0`,
  // `chat_msgs: 0`, `adoption_rate: 0` and discarded the error from a
  // repository whose every query named a table no migration creates (#303). The
  // oracle was the response body, so a body of pure fabrication satisfied it
  // exactly as well as real data would have.
  //
  // It then became `expect(response.status()).toBe(500)`, pinning the refusal
  // that replaced the fabrication. That was true when written and is no longer:
  // the gateway request log (shared migration 0099) gives this endpoint a real
  // producer, and the live stack runs that migration.
  //
  // So what it pins now is the property that survives both: EVERY FIGURE THE
  // RESPONSE CARRIES IS ON THE SCREEN, AND NO FIGURE IT OMITS IS. A journey
  // against a stack with no gateway traffic sees `llm_calls: 0` — a real,
  // measured zero — and that is a different claim from the hardcoded one, which
  // is why `expectKpiRow` checks the absences too.
  expect(response.status()).toBe(200);
  const payload = (await response.json()) as {
    kpis: Kpis;
    models: readonly unknown[];
    daily_activity: readonly unknown[];
    top_ai_users: readonly unknown[];
  };

  // The figures with no producer must not appear even here, where a real
  // response is on the wire.
  for (const absent of ['unique_users', 'tool_runs', 'chat_msgs', 'agent_runs', 'total_cost'] as const) {
    expect(
      (payload.kpis as Record<string, unknown>)[absent],
      `kpis.${absent} has no producer and must be absent, not zero`,
    ).toBeUndefined();
  }
  // The counts the request log always produces, whatever the traffic was.
  expect(typeof payload.kpis.llm_calls).toBe('number');
  expect(typeof payload.kpis.total_tokens).toBe('number');

  await expectKpiRow(page, payload.kpis);

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * J24a — the tab that still has NO data source must say so, and must say it in
 * a way a client can act on.
 *
 * ONLY `analytics_tools` now. `analytics_agents` gained its producer: shared
 * migration 0100 put an execution_id on gateway.llm_request_logs, signed into
 * the identity header, and the agent is resolved at read time. It answers 200.
 *
 * Its 200 is NOT the fallback-body failure this suite exists to catch, and the
 * difference is the shape: when the window predates the migration the response
 * carries `agent_dimension_available: false` and NO `items` key at all, so a
 * client cannot map over a fabricated empty list and read "no agent ran" for a
 * month in which agents ran constantly. That is asserted below.
 *
 * `analytics_tools` still has no producer: nothing records a tool call made
 * outside a chat turn, so a table built from p_<id>.chat_message_trace_step
 * would under-report by an unknown factor — worse than the refusal. See
 * services/elitea-main/internal/infra/db/repos/analytics.go's header.
 *
 * The STATUS is the point. Answered 500, a permanent refusal is
 * indistinguishable from a blip to every client that retries 5xx — and this
 * app's TanStack Query default does, so each tab asked twice for an answer the
 * server had already finished giving. 501 is final.
 * ──────────────────────────────────────────────────────────────────────── */
test('J24a: the tabs with no data source refuse finally, and the UI says which', async ({ page }) => {
  await page.goto(BASE_URL + '/app/settings/analytics');
  await expect(page.getByRole('tab', { name: 'Overview', exact: true })).toBeVisible();

  // The Agents tab now HAS a producer. Assert the shape that keeps its 200
  // honest: the availability flag, and the absence of `items` when it is false.
  const agentsPath = `${API_BASE}/elitea_core/analytics_agents/prompt_lib/${DEFAULT_PROJECT_ID}`;
  const agentsResp = await page.request.get(agentsPath);
  expect(agentsResp.status(), `${agentsPath} answers now that it has a producer`).toBe(200);
  const agents = (await agentsResp.json()) as Record<string, unknown>;
  expect(
    agents,
    `${agentsPath} must state whether the dimension is available`,
  ).toHaveProperty('agent_dimension_available');
  expect(
    agents,
    'both call figures travel either way — the per-agent rows are not a partition of the overview total',
  ).toHaveProperty('attributed_llm_calls');
  if (agents['agent_dimension_available'] === false) {
    // `not.toHaveProperty`, not `'items' in agents`: R-A6 bans hand-rolled
    // shape sniffing, and the matcher says the same thing more directly — an
    // unavailable dimension must OMIT items, never answer [], which would
    // report "no agent ran" for a window in which agents ran constantly.
    expect(agents).not.toHaveProperty('items');
  }

  for (const tab of ['Tools'] as const) {
    const path = `${API_BASE}/elitea_core/analytics_${tab.toLowerCase()}/prompt_lib/${DEFAULT_PROJECT_ID}`;
    const resp = await page.request.get(path);
    expect(resp.status(), `${path} must refuse with a FINAL status`).toBe(501);
    const failure = (await resp.json()) as { code?: string; detail?: string };
    // Machine-readable, so the client is not parsing prose to decide what to
    // render or whether to retry.
    expect(failure.code, `${path} must carry a machine-readable code`).toBe('no_data_source');
    expect(failure.detail ?? '', `${path} must name the absent producer`).toContain(
      'analytics: no data source',
    );

    await page.getByRole('tab', { name: tab, exact: true }).click();

    // The UI must distinguish the two failures too. "Failed to load analytics
    // data." on a tab whose figures nothing produces sends the reader to file a
    // bug or reload forever; this says what is actually true.
    await expect(page.getByText('Not available on this deployment', { exact: true })).toBeVisible();
    await expect(page.getByText('Failed to load analytics data.', { exact: true })).toHaveCount(0);
    // The server's own reason, rendered rather than discarded.
    await expect(page.getByText(/analytics: no data source/)).toBeVisible();
  }

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * J24f — the Health tab, which could never render anything at all.
 *
 * Two defects made its whole body unreachable for the life of the component,
 * and each hid the other: `AnalyticsTabContent` never passed the `health` prop
 * it branches on (there was nothing to pass — `ProjectAnalytics` had no such
 * field), and the trend chart read `errors`/`events` off daily points that have
 * never carried either, so every point would have been 0 even had the chart
 * been reachable. A flat line at zero reads as "nothing failed".
 *
 * The gateway request log is what makes it answerable: it is the only table in
 * this platform that records a request that FAILED, because the billing ledger
 * is written from a delta and a delta rides only a BILLED request.
 * ──────────────────────────────────────────────────────────────────────── */
test('J24f: the Health tab renders the failure and latency view the backend returned', async ({ page }) => {
  const usage = page.waitForResponse(
    (r) => USAGE_RE.test(r.url()) && r.request().method() === 'GET',
    { timeout: 20_000 },
  );

  await page.goto(BASE_URL + '/app/settings/analytics');
  const payload = (await (await usage).json()) as {
    health?: { requests: number; errors: number; error_rate: number };
  };

  // The block must be on the wire. Absent means the repository could not build
  // it; a project with no traffic gets zeros, which is a different claim.
  expect(payload.health, 'the usage response must carry a health block').toBeDefined();
  const health = payload.health;
  if (health === undefined) return;
  expect(typeof health.requests).toBe('number');
  expect(typeof health.errors).toBe('number');

  await page.getByRole('tab', { name: 'Health', exact: true }).click();

  // The state this tab was stuck in for its whole life. Reaching anything else
  // is the assertion.
  await expect(page.getByText('No health data available.', { exact: true })).toHaveCount(0);

  // The totals, read back out of the response the app itself received.
  await expect(page.getByText('REQUESTS', { exact: true })).toBeVisible();
  await expect(page.getByText('ERRORS', { exact: true })).toBeVisible();
  await expect(page.getByText('ERROR RATE', { exact: true })).toBeVisible();
  await expect(
    page.getByText('ERROR RATE', { exact: true }).locator('..'),
  ).toContainText(`${health.error_rate.toFixed(1)}%`);

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * J24b — the rendering branches a real journey run cannot reach.
 *
 * The live stack answers this endpoint now (J24 above), but a journey run
 * generates no gateway traffic of its own, so every figure it returns is a real
 * zero and every collection is empty. The populated branches — the leaderboard
 * rows, the `fmtNum` K/M abbreviation, the adoption badge, the model table, the
 * chart's two axes — are therefore reachable only from a crafted response,
 * which is what this test serves.
 *
 * That division of labour is deliberate rather than a workaround. J24 proves
 * the numbers on screen came from the wire; this proves the components can
 * render numbers that are not zero. Neither claim implies the other, and the
 * revision of this file that had only the first passed for months against a
 * dashboard of six hardcoded zeros (#303).
 * ──────────────────────────────────────────────────────────────────────── */
test('J24b: analytics tiles, leaderboard and model table are bound to the response payload', async ({ page }) => {
  const kpis: Kpis = {
    total_project_users: 9,
    active_project_members: 4,
    ai_active_users: 2,
    adoption_rate: 42,
    llm_calls: 1234,
    total_tokens: 5678,
  };
  const body = {
    kpis,
    top_ai_users: [
      {
        user_id: 'u-ana-1',
        email: 'first-ana@example.com',
        run_count: 17,
        total_tokens: 900,
        last_active_at: '2026-08-02T09:00:00Z',
      },
      {
        user_id: 'u-ana-2',
        email: 'second-ana@example.com',
        run_count: 3,
        total_tokens: 120,
        last_active_at: '2026-08-01T09:00:00Z',
      },
    ],
    daily_activity: [
      { date: '2026-08-01', llm_calls: 12, total_tokens: 400, active_users: 3 },
      { date: '2026-08-02', llm_calls: 4, total_tokens: 100, active_users: 1 },
    ],
    models: [
      { model: 'model-alpha-ana', provider: 'provider-ana', prompt_tokens: 100, completion_tokens: 50, run_count: 40 },
    ],
  };
  // The cost tile comes from a DIFFERENT endpoint — /analytics_costs owns the
  // money, and /analytics does not publish it — so a payload-bound cost has to
  // be stubbed separately. That separation is the assertion: a COST tile
  // appearing from the usage stub alone would mean the two views of the same
  // dollars had been re-merged.
  const costBody = {
    kpis: { total_cost: 12.5, currency: 'USD', periods: 1, spend_available: true, window_days: 1 },
    periods: [],
    by_scope: [],
    periods_truncated: false,
    date_from: '2026-08-01T00:00:00Z',
    date_to: '2026-08-02T00:00:00Z',
  };

  await page.route(USAGE_GLOB, (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) }),
  );
  await page.route('**/api/v2/elitea_core/analytics_costs/prompt_lib/*', (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(costBody) }),
  );

  await page.goto(BASE_URL + '/app/settings/analytics');

  await expectKpiRow(page, kpis);
  // 1234 → "1.2K": the tile shows the FORMATTER'S output, so neither the raw
  // number nor a hardcoded zero satisfies this.
  await expect(kpi(page, 'LLM CALLS').getByText('1.2K', { exact: true })).toBeVisible();
  // The badge only renders when adoption_rate > 0.
  await expect(kpi(page, 'AI ACTIVE').getByText('↑42%', { exact: true })).toBeVisible();
  // Cost, bound to the SEPARATE endpoint's figure.
  await expect(kpi(page, 'COST').getByText('$12.50', { exact: true })).toBeVisible();

  // Leaderboard: populated rows replace the "No AI activity data." branch.
  await expect(page.getByText('No AI activity data.', { exact: true })).toHaveCount(0);
  await expect(page.getByText('first-ana@example.com', { exact: true })).toBeVisible();
  await expect(page.getByText('900 tokens', { exact: true })).toBeVisible();
  await expect(page.getByText('second-ana@example.com', { exact: true })).toBeVisible();
  await expect(page.getByText('120 tokens', { exact: true })).toBeVisible();

  // Model usage table: rendered only because `models` is non-empty. The
  // PROVIDER is asserted alongside the model because the rows are grouped by
  // the pair — two routes to the same model name would otherwise read as one
  // duplicated row.
  await expect(page.getByText('Model Usage Breakdown', { exact: true })).toBeVisible();
  await expect(page.getByText('model-alpha-ana', { exact: true })).toBeVisible();
  await expect(page.getByText('provider-ana', { exact: true })).toBeVisible();

  // Daily-activity series: two points ⇒ two x-axis ticks, labelled by
  // `value.slice(5)`, and a y-axis whose domain is computed from the series
  // maximum (`llm_calls: 12`).
  const xTicks = page.locator('.recharts-xAxis-tick-labels .recharts-cartesian-axis-tick-value');
  await expect(xTicks).toHaveCount(2);
  await expect(xTicks.nth(0)).toHaveText('08-01');
  await expect(xTicks.nth(1)).toHaveText('08-02');
  await expect(
    page.locator('.recharts-yAxis-tick-labels .recharts-cartesian-axis-tick-value', { hasText: /^12$/ }),
  ).toHaveCount(1);

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * J24c — the second acceptance clause, verbatim: "loading failures show an
 * error state instead of empty charts".
 * ──────────────────────────────────────────────────────────────────────── */
test('J24c: a failing analytics load shows the error state instead of empty charts', async ({ page }) => {
  await page.route(USAGE_GLOB, (route) =>
    route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"induced failure"}' }),
  );

  await page.goto(BASE_URL + '/app/settings/analytics');

  // AnalyticsTabContent.tsx:110-119 — the `needsOverview && isError` branch.
  await expect(page.getByText('Failed to load analytics data.', { exact: true })).toBeVisible({
    timeout: 20_000,
  });

  // A 500 is a TRANSIENT failure — the data exists and this attempt did not
  // reach it — so the generic, retryable wording is the right one and the
  // "not available on this deployment" wording would be a false claim about
  // the platform.
  await expect(page.getByText('Not available on this deployment', { exact: true })).toHaveCount(0);

  // "instead of empty charts": no KPI tiles, no recharts surface, no
  // leaderboard — the error branch returns EARLY, before renderTabBody.
  for (const label of ALL_KPI_LABELS) {
    await expect(page.getByText(label, { exact: true })).toHaveCount(0);
  }
  await expect(page.locator('.recharts-surface')).toHaveCount(0);
  await expect(page.getByText('Top AI Adopters', { exact: true })).toHaveCount(0);

  // The rest of the screen is still alive — the failure is scoped to the tab body.
  await expect(page.getByRole('tab', { name: 'Overview', exact: true })).toBeVisible();
  await expect(page.getByText('Project: Default Project', { exact: true })).toBeVisible({ timeout: 15_000 });

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * J24d — the three list tabs. Each one issues its own request; the row count
 * the UI reports is checked against the row count the backend actually
 * returned for the same project.
 * ──────────────────────────────────────────────────────────────────────── */
test('J24d: the Users tab renders the rows the backend returned, not a stub table', async ({ page }) => {
  await page.goto(BASE_URL + '/app/settings/analytics');
  await expect(page.getByRole('tab', { name: 'Overview', exact: true })).toBeVisible();

  // This test used to assert three tabs all refused with a 500 — and before
  // that, that all three rendered a table SIZED BY the endpoint (status 200,
  // `items.length` echoed in the count label). The first oracle was a
  // fabrication: the routes answered before consulting the repository at all,
  // over queries naming tables no migration creates (#303). The second was
  // accurate until the gateway request log gave the Users read a real producer.
  //
  // Agents and Tools still have none, and J24a above pins their refusal. What
  // is left here is the tab that now ANSWERS, and the claim is the same shape
  // as J24's: the count the UI reports is the count the endpoint returned.
  // THE ORACLE MUST ASK THE SAME QUESTION THE SCREEN DID. Sent bare, this
  // request carries no date_from/date_to, so the server falls back to its
  // 7-day default (defaultDateRangeDays) while the tab it is being compared
  // against is showing the "Last 24h" preset. Both counts are 0 on a stack with
  // no gateway traffic, so the mismatch is invisible today and would surface as
  // a failure that is not a defect the first time the seed plants a request-log
  // row aged between one and seven days.
  //
  // The window comes off the app's OWN request rather than being restated here,
  // for the same reason: a literal would be a second copy of the preset's
  // definition, free to drift from it.
  //
  // The waiter is armed BEFORE the click, because the Users request only fires
  // when the tab is opened — awaiting it first would deadlock.
  const uiRequest = page.waitForRequest((r) => USERS_RE.test(r.url()), { timeout: 20_000 });
  await page.getByRole('tab', { name: 'Users', exact: true }).click();
  const uiParams = new URL((await uiRequest).url()).searchParams;

  const path =
    `${API_BASE}/elitea_core/analytics_users/prompt_lib/${DEFAULT_PROJECT_ID}` +
    `?date_from=${encodeURIComponent(uiParams.get('date_from') ?? '')}` +
    `&date_to=${encodeURIComponent(uiParams.get('date_to') ?? '')}`;
  const resp = await page.request.get(path);
  expect(resp.status(), `${path} has a data source and must answer`).toBe(200);
  const { items, truncated } = (await resp.json()) as {
    items: readonly { user_id: string }[];
    truncated: boolean;
  };
  // The completeness signal must be on the wire, not inferred. The client
  // paginates and searches client-side over `items`, so a cut list without this
  // flag would be presented as the whole membership.
  expect(typeof truncated, 'the users list must state whether it was cut').toBe('boolean');

  // A table, not the error branch.
  await expect(page.getByText('User Activity', { exact: true })).toBeVisible();
  await expect(page.getByText('Failed to load analytics data.', { exact: true })).toHaveCount(0);
  await expect(page.getByText('Not available on this deployment', { exact: true })).toHaveCount(0);

  // The count label, against the row count the endpoint actually returned. A
  // journey run has no gateway traffic of its own, so this is normally 0 — but
  // it is a MEASURED zero read back off the wire, which a stub cannot fake and
  // a broken query cannot survive: a 500 would have failed the status check
  // above, and a fabricated table would report a count the response does not
  // carry.
  const label = truncated
    ? `Top ${items.length} users by LLM calls`
    : `${items.length} users`;
  await expect(page.getByText(label, { exact: true })).toBeVisible();

  await checkA11y(page);
});

/* ────────────────────────────────────────────────────────────────────────
 * J24e — the filter bar is wired to the query, not decorative. Selecting a
 * preset must produce a NEW backend request whose range is the selected width.
 * ──────────────────────────────────────────────────────────────────────── */
test('J24e: a date preset re-queries the backend with the selected range', async ({ page }) => {
  const first = page.waitForRequest((r) => USAGE_RE.test(r.url()), { timeout: 20_000 });
  await page.goto(BASE_URL + '/app/settings/analytics');
  const initialUrl = new URL((await first).url());
  // Default preset is "Last 24h" (AnalyticsContainer.tsx:120-122).
  const initialSpanDays = spanDays(initialUrl);
  expect(initialSpanDays).toBeGreaterThan(0.9);
  expect(initialSpanDays).toBeLessThan(1.1);

  const refetch = page.waitForRequest(
    (r) => USAGE_RE.test(r.url()) && spanDays(new URL(r.url())) > 6.9,
    { timeout: 20_000 },
  );
  await page.getByRole('button', { name: 'Last 7d', exact: true }).click();
  const sevenDayUrl = new URL((await refetch).url());
  expect(spanDays(sevenDayUrl)).toBeLessThan(7.1);

  // The re-query lands and the page is still alive after the range change.
  //
  // The liveness check has moved twice with the backend: a KPI tile first (when
  // every value was a hardcoded zero), then the error branch (when the endpoint
  // refused), and now a tile again — because the endpoint answers, and the tile
  // it renders carries a figure the request log measured rather than a literal.
  // The claim this journey makes is about the REQUEST, in the two span
  // assertions above; this is the liveness half.
  await expect(kpi(page, 'LLM CALLS')).toBeVisible();
  await expect(page.getByText('Failed to load analytics data.', { exact: true })).toHaveCount(0);

  await checkA11y(page);
});

/** `date_to - date_from`, in days, from a usage-endpoint URL. */
function spanDays(url: URL): number {
  const from = Date.parse(url.searchParams.get('date_from') ?? '');
  const to = Date.parse(url.searchParams.get('date_to') ?? '');
  expect(Number.isNaN(from), `date_from missing from ${url.pathname}${url.search}`).toBe(false);
  expect(Number.isNaN(to), `date_to missing from ${url.pathname}${url.search}`).toBe(false);
  return (to - from) / 86_400_000;
}
