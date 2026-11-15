/**
 * FINDING A PUBLISHED AGENT (#939 group 10).
 *
 * `agents.publishing.spec.ts` proves that a publish puts the row in the
 * catalogue — it reads `GET /elitea_core/public_applications/prompt_lib`
 * directly. That is the right assertion for the publish plane and it says
 * nothing about the screen a CONSUMER uses to find the agent afterwards.
 *
 * This file is that screen: `/app/marketplace` (ADR-0051; it was the Catalog's
 * Agents tab, "Agent HUB"), which mounts `Marketplace`
 * (`src/pages/marketplace/Marketplace.tsx`) — a search field in the right
 * sidebar's Browse tab over the server's own `?query=`, category checkboxes
 * there too, and a card per agent that is a link to the listing page. The
 * source case asks for exactly those four things and nothing else: searchable
 * by name, filterable by its category, a card that carries the agent's
 * identity, and a card that opens.
 *
 * WHAT IS DELIBERATELY NOT HERE. The source case's last step ("start a
 * conversation; the agent responds") needs a model turn, which this stack
 * composes no worker for. That half rides the chat-stream lane with the rest
 * of the version-switch family (#939 group 10), and the case is recorded as
 * covered here for its discovery half alone.
 *
 * SHARED-PROJECT DISCIPLINE. Publishing writes into the catalogue every other
 * worker reads, so the agent is named uniquely per run and every assertion is
 * scoped to that name — never "the first card" or "the only section".
 */
import { expect, test } from '@playwright/test';

import type { APIRequestContext, Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  PUBLISHABLE_TAGS,
  unpublishAllVersions,
  deleteAgent,
} from '../../fixtures/api';

/**
 * The CATEGORY this agent publishes into — the Browse sidebar's checkbox.
 *
 * MEASURED, and not what the source case's word "tag" suggests: the checkboxes
 * are the deployment's agent CATEGORIES (`useGetAgentCategories`), and a row is
 * filed by `meta.category` (`getCategoryForApplication`,
 * `src/pages/marketplace/helpers.ts`, whose own note records that reading a
 * flat `category` put every agent in "Other"). A version's `tags` are the
 * publish GATE's requirement (#913) and are not what the Marketplace groups
 * by, so the fixture below sets both and this constant names the one the
 * checkbox carries.
 *
 * `Development` is the category the publish wizard's own journey picks
 * (`agents.publishing.spec.ts::chooseCategory`), so it is one this deployment
 * really serves rather than a name invented here.
 */
const MARKETPLACE_CATEGORY = 'Development';

/** The description the card must render, distinctive enough that only this agent's card can carry it. */
function descriptionFor(name: string): string {
  return `${name} briefs a meeting from notes.`;
}

interface PublishedAgent {
  readonly id: string;
  readonly versionId: string;
  readonly name: string;
}

/**
 * Create an agent that passes the pre-publish check and publish it, through
 * the same two routes the wizard calls.
 *
 * The version carries `PUBLISHABLE_TAGS` because an untagged version is a
 * Critical the publish route refuses inline (#913) — and because the tag is
 * ALSO what the Marketplace groups by, so the one fixture serves both the gate and the
 * assertion.
 */
async function publishAgent(request: APIRequestContext, name: string): Promise<PublishedAgent> {
  const created = await request.post(`${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name,
      description: descriptionFor(name),
      versions: [
        {
          name: 'base',
          agent_type: 'openai',
          instructions:
            'You are a meeting preparation assistant. Turn the notes the user gives you into a short briefing.',
          welcome_message: 'Send me your notes and I will prepare a briefing.',
          conversation_starters: ['Summarise these notes.'],
          // The gate's requirement and the Marketplace's grouping key are DIFFERENT
          // fields; a fixture that set only one of them publishes an agent
          // that is either refused or invisible. See MARKETPLACE_CATEGORY.
          tags: [...PUBLISHABLE_TAGS],
          meta: { category: MARKETPLACE_CATEGORY },
        },
      ],
    },
  });
  expect(created.ok(), `the agent was not created: ${(await created.text()).slice(0, 300)}`).toBe(true);
  const body = (await created.json()) as { id?: unknown; version_details?: { id?: unknown } };
  const agent: PublishedAgent = {
    id: String(body.id ?? ''),
    versionId: String(body.version_details?.id ?? ''),
    name,
  };
  expect(agent.id).not.toBe('');

  const published = await request.post(
    `${API_BASE}/elitea_core/publish/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.versionId}`,
    { data: { version_name: `rel${String(Date.now()).slice(-6)}` } },
  );
  expect(published.status(), `the publish was refused: ${(await published.text()).slice(0, 300)}`).toBe(200);

  // The catalogue route the Marketplace reads, polled BEFORE the browser is pointed
  // at it: a page that renders no card because the publish has not landed yet is a
  // race, and it would read as "the search box is broken".
  await expect
    .poll(
      async () => {
        const catalogue = await request.get(`${API_BASE}/elitea_core/public_applications/prompt_lib?query=${name}`);
        if (!catalogue.ok()) return false;
        const rows = ((await catalogue.json()) as { rows?: readonly { name?: string }[] }).rows ?? [];
        return rows.some((row) => row.name === name);
      },
      { timeout: 30_000, message: 'the published agent never reached the catalogue route the Marketplace reads' },
    )
    .toBe(true);
  return agent;
}

/**
 * Open the Marketplace's agents and open its Browse sidebar.
 *
 * The sidebar is open by default only on a window of 1440px or more; below
 * that it is a drawer, opened from the summary bar's Browse button. The bar
 * is drawn only while the sidebar is closed, so the helper opens it only when
 * the search is not already on screen.
 */
async function openMarketplace(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/marketplace?kind=agents`);
  await expect(page.getByTestId('marketplace')).toBeVisible({ timeout: 30_000 });
  await showBrowse(page);
}

/** Opens the Browse sidebar when it is closed. */
async function showBrowse(page: Page): Promise<void> {
  const open = page.getByTestId('marketplace-open-browse');
  // One of the two is on screen once the page has settled: the button (sidebar closed) or the search (open).
  await expect(open.or(browseSearch(page))).toBeVisible({ timeout: 30_000 });
  if (await open.isVisible()) await open.click();
  await expect(browseSearch(page)).toBeVisible({ timeout: 30_000 });
}

/** The Browse sidebar's search field (it carries a test id: the label is shared with the skills kind). */
function browseSearch(page: Page): ReturnType<Page['getByTestId']> {
  return page.getByTestId('marketplace-search');
}

const created: PublishedAgent[] = [];

test.afterEach(async ({ request }) => {
  while (created.length > 0) {
    const agent = created.pop();
    if (agent === undefined) continue;
    // Withdraw first: a published version refuses the delete with a 400.
    await unpublishAllVersions(request, agent.id).catch(() => undefined);
    await deleteAgent(request, agent.id).catch(() => undefined);
  }
});

/*
 * onetest: ELITEA-0201 — a published agent is discoverable in the Marketplace:
 * found by typing its exact name, found again through its category checkbox
 * with the search cleared, and its card is a link that opens the agent's own
 * listing page, which carries the agent's name and description. (The case's
 * final "and it answers a prompt" step is a model turn and rides the
 * chat-stream lane.)
 */
test('a published agent is findable in the Marketplace by name and by category, and its card opens', async ({
  page,
  request,
}) => {
  // A create, a publish, a catalogue poll and three Marketplace renders.
  test.setTimeout(120_000);

  const name = `${AUTOTEST_PREFIX}hub-${String(Date.now()).slice(-7)}`;
  const agent = await publishAgent(request, name);
  created.push(agent);

  await openMarketplace(page);

  // ── found by its exact name ───────────────────────────────────────────
  // The rows come back already filtered by the server's `?query=`
  // (`useMarketplaceListings`' own note), so this is a round trip and not a
  // client-side filter over whatever page happened to be loaded.
  await browseSearch(page).fill(name);
  // `.first()`, and the reason is worth keeping: a freshly published agent is
  // rendered in MORE THAN ONE section at once — the full list and the
  // "Featured" row (newest first) — so an unqualified locator is a
  // strict-mode violation rather than a missing card. Being in two sections is
  // correct behaviour; the assertion is that it is findable, not that it
  // appears once. With a search typed the shelves are hidden, so in practice
  // the list holds the one card.
  const card = page.getByTestId(`marketplace-card-link-agents-${agent.id}`).first();
  await expect(card, 'the published agent must be findable by its exact name').toBeVisible({ timeout: 30_000 });
  await expect(card, 'the card must name the agent').toContainText(name);

  // ── the card is a link, and the listing page carries the identity ─────
  //
  // MEASURED: the card is a compact tile (icon, name, one-line description,
  // like count, version) and a real link to `/marketplace/agents/{id}`. The
  // full description lives on the listing page, together with Try in chat, so
  // the identity assertion is made where the product puts it.
  await card.click();
  await expect(page, 'clicking the card must open the listing page, not a dialog').toHaveURL(
    new RegExp(`/marketplace/agents/${agent.id}$`),
    { timeout: 20_000 },
  );
  await expect(page.getByTestId('marketplace-listing-name'), 'the listing page must name the agent').toHaveText(name, {
    timeout: 20_000,
  });
  await expect(
    page.getByText(descriptionFor(name), { exact: false }),
    'the listing page must carry the agent’s description',
  ).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId('marketplace-try-in-chat'), 'Try in chat must stay').toBeVisible();
  await page.goBack();
  await expect(page.getByTestId('marketplace')).toBeVisible({ timeout: 20_000 });

  // ── found by its category, with the search cleared ───────────────────
  // The half the name search cannot answer: a consumer who does not know the
  // name browses the categories instead.
  await showBrowse(page);
  await browseSearch(page).fill('');
  const category = page.getByRole('checkbox', { name: MARKETPLACE_CATEGORY, exact: true });
  await expect(category, `the Marketplace must offer the '${MARKETPLACE_CATEGORY}' category`).toBeVisible({ timeout: 20_000 });
  await category.check();
  await expect(category, 'the checkbox must report itself checked').toBeChecked();
  // The summary bar's chips are for a CLOSED sidebar (unit-tested); with Browse open the checkbox is the filter's state.
  await expect(
    page.getByTestId(`marketplace-card-link-agents-${agent.id}`).first(),
    'the published agent must be reachable through its category with no search term',
  ).toBeVisible({ timeout: 30_000 });
});
