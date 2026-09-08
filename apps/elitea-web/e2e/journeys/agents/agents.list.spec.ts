/**
 * Journey 14e: the agents LIST page's own header — its search box and its
 * table/card switch.
 *
 * Ported BY USE CASE from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/agents/
 * test_agent_management.py::TestAgentList`). Each test below names the legacy
 * test it answers:
 *
 *  - `test_agents_dashboard_loads` — the dashboard renders a header and a
 *    functional search input;
 *  - `test_agent_search` — searching by name finds an agent;
 *  - `test_agent_search_no_results` — a search that matches nothing shows no
 *    results;
 *  - `test_view_toggle_table_and_card` — the two view buttons switch the list
 *    between the table and the cards.
 *
 * `agents.editor.spec.ts` used to record all four as unportable product gaps:
 * the list carried no search control and `ViewToggle` had no port. Both exist
 * now — `widgets/page-header`'s `ListSearchField` and `ListViewToggle`,
 * mounted by `pages/agents/Applications.tsx` — so the use cases are journeys
 * rather than disclosures.
 *
 * The agents are created through the API, not the form: every claim below is
 * about a LIST, and `agents.lifecycle.spec.ts` already covers authoring.
 */
import { expect, test, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgent, deleteAgent } from '../../fixtures/api';
import { DEFAULT_PROJECT_NAME, ensureProjectSelected } from '../../fixtures/project';

/** Agents minted below, removed in `afterEach` whatever happened. */
const created: string[] = [];

test.afterEach(async ({ request }) => {
  while (created.length > 0) {
    const id = created.pop();
    if (id === undefined) continue;
    await deleteAgent(request, id);
  }
});

/** A name unique per run, inside the 32-character ceiling the create form imposes on the same column. */
function uniqueName(stem: string): string {
  return `${AUTOTEST_PREFIX}${stem}-${String(Date.now()).slice(-7)}`;
}

/** The header's search field — `ListSearchField`, mounted with this test id by `Applications.tsx`. */
function searchBox(page: Page) {
  return page.getByTestId('agent-search-input');
}

/** Every card currently drawn by `EntityCardList`. */
function cards(page: Page) {
  return page.getByTestId('entity-card');
}

/** Every row drawn by the TABLE rendering of the same list. */
function tableRows(page: Page) {
  return page.getByTestId('entity-list-row');
}

/**
 * Open the private agents list and wait for it to be a LIST, not a spinner.
 *
 * `ensureProjectSelected` first: the tab set comes from the selected project
 * (`useApplicationTabs`), and the public project renders Latest/My liked/
 * Trending instead of the private status tabs this route needs.
 */
async function openAgentsList(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/agents/all`);
  await ensureProjectSelected(page, DEFAULT_PROJECT_NAME);
  await page.waitForURL('**/app/agents/all**', { timeout: 20_000 });
  await expect(searchBox(page), 'the agents list must render its search box').toBeVisible({ timeout: 30_000 });
}

/*
 * Legacy: `test_agents_dashboard_loads` and `test_agent_search`.
 *
 * One journey rather than two: the legacy "the dashboard loads" test asserts a
 * header plus a search box that accepts text, which a blank page with a tab bar
 * and an inert input would also satisfy. What makes the box real is that typing
 * in it changes the list, so both claims are made against one screen.
 */
test('J14e: the agents list renders its header search box, and typing in it narrows the list', async ({
  page,
  request,
}) => {
  const name = uniqueName('search');
  const agent = await createAgent(request, name);
  created.push(agent.id);

  await openAgentsList(page);
  await expect(page.getByRole('tablist', { name: 'Agents' })).toBeVisible();
  await expect(searchBox(page)).toBeEditable();

  await expect(cards(page).filter({ hasText: name }), 'the new agent must be on the dashboard').toHaveCount(1, {
    timeout: 30_000,
  });

  // At least one OTHER card before the search — otherwise "narrowed to one"
  // below is satisfied by a project that only ever held this agent.
  const before = await cards(page).count();
  expect(before, 'the seeded project must hold more than the agent this test created').toBeGreaterThan(1);

  await searchBox(page).fill(name);

  // `toHaveCount` retries, which covers `SimpleSearchBar`'s 300 ms debounce
  // and the request the list then issues, without a `waitForTimeout`.
  await expect(cards(page), 'the search must narrow the list to the one matching agent').toHaveCount(1, {
    timeout: 20_000,
  });
  await expect(cards(page).first()).toContainText(name);

  // The query is in the URL, which is what makes a filtered list linkable and
  // reload-proof. A box that only held the text in component state would pass
  // every assertion above and fail this one.
  await expect(page).toHaveURL(new RegExp(`query=${name}`));
});

/*
 * Legacy: `test_agent_search_no_results`.
 *
 * The legacy assertion is an absence ("the made-up name is not in the
 * results"), which a search box that ignored its input entirely would also
 * satisfy on a project that never held that name. This asserts the state the
 * app really enters: zero cards AND the empty state whose copy is reserved for
 * a query that matched nothing.
 */
test('J14e: a search that matches nothing empties the list and says so', async ({ page }) => {
  await openAgentsList(page);

  await searchBox(page).fill('zzzz_nonexistent_agent_12345');

  // `.count()` for the absence — it does not wait-for-attach the way a
  // visibility assertion does.
  await expect(cards(page)).toHaveCount(0, { timeout: 20_000 });
  await expect(page.getByText('Nothing found.')).toBeVisible({ timeout: 10_000 });

  // …and it is the SEARCH empty state, not the "this project has no agents"
  // one. Without this the test passes against a list that dropped its rows for
  // any other reason.
  await expect(page.getByText('No agents yet')).toHaveCount(0);
});

/*
 * Legacy: `test_view_toggle_table_and_card`.
 *
 * The legacy test asserts only that the two buttons exist and take
 * `aria-pressed`. That is satisfied by a pair of buttons wired to nothing,
 * which is exactly the failure mode this app has produced before, so each
 * click here is checked against the list it is supposed to change: the table
 * rendering draws `entity-list-row`s and no `entity-card`s, and the card
 * rendering the reverse.
 *
 * The remembered choice is asserted across a RELOAD, which the baseline's own
 * toggle does not survive — `ListViewToggle` persists it per page.
 */
test('J14e: the view toggle switches the list between table and cards, and the choice survives a reload', async ({
  page,
  request,
}) => {
  const name = uniqueName('view');
  const agent = await createAgent(request, name);
  created.push(agent.id);

  await openAgentsList(page);
  const tableButton = page.getByTestId('agent-table-view-button');
  const cardButton = page.getByTestId('agent-card-view-button');
  await expect(tableButton).toBeVisible();
  await expect(cardButton).toBeVisible();
  await expect(cardButton, 'the list starts on cards').toHaveAttribute('aria-pressed', 'true');
  await expect(cards(page).filter({ hasText: name })).toHaveCount(1, { timeout: 30_000 });

  await tableButton.click();
  await expect(tableButton).toHaveAttribute('aria-pressed', 'true');
  await expect(tableRows(page).filter({ hasText: name }), 'the table rendering must draw the same agent').toHaveCount(
    1,
    { timeout: 20_000 },
  );
  await expect(cards(page), 'no card grid while the table is showing').toHaveCount(0);

  // The reload proves the choice is remembered, not merely held in memory.
  await page.reload();
  await expect(page.getByTestId('agent-table-view-button')).toHaveAttribute('aria-pressed', 'true', {
    timeout: 30_000,
  });
  await expect(tableRows(page).filter({ hasText: name })).toHaveCount(1, { timeout: 30_000 });

  await page.getByTestId('agent-card-view-button').click();
  await expect(page.getByTestId('agent-card-view-button')).toHaveAttribute('aria-pressed', 'true');
  await expect(cards(page).filter({ hasText: name })).toHaveCount(1, { timeout: 20_000 });
  await expect(tableRows(page)).toHaveCount(0);
});
