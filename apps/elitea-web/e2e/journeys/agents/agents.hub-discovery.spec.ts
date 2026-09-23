/**
 * FINDING A PUBLISHED AGENT (#939 group 10).
 *
 * `agents.publishing.spec.ts` proves that a publish puts the row in the
 * catalogue — it reads `GET /elitea_core/public_applications/prompt_lib`
 * directly. That is the right assertion for the publish plane and it says
 * nothing about the screen a CONSUMER uses to find the agent afterwards.
 *
 * This file is that screen: `/app/elitea-catalog`'s Agents tab, which mounts
 * `AgentHub` (`src/pages/agents-hub/AgentHub.tsx`) — a search field over the
 * server's own `?query=`, one section per TAG, and a card per agent that opens
 * a detail modal. The source case asks for exactly those four things and
 * nothing else: searchable by name, filterable by its tag, a card that carries
 * the agent's identity, and a card that opens.
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
 * The CATEGORY this agent publishes into — the hub's section and its chip.
 *
 * MEASURED, and not what the source case's word "tag" suggests: the hub's
 * chips are the deployment's agent CATEGORIES (`useGetAgentCategories`), and a
 * row is filed by `meta.category` (`getCategoryForApplication`,
 * `src/pages/agents-hub/helpers.ts`, whose own note records that reading a
 * flat `category` put every agent in "Other"). A version's `tags` are the
 * publish GATE's requirement (#913) and are not what the hub groups by, so the
 * fixture below sets both and this constant names the one the chip carries.
 *
 * `Development` is the category the publish wizard's own journey picks
 * (`agents.publishing.spec.ts::chooseCategory`), so it is one this deployment
 * really serves rather than a name invented here.
 */
const HUB_CATEGORY = 'Development';

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
 * ALSO what the hub groups by, so the one fixture serves both the gate and the
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
          // The gate's requirement and the hub's grouping key are DIFFERENT
          // fields; a fixture that set only one of them publishes an agent
          // that is either refused or invisible. See HUB_CATEGORY.
          tags: [...PUBLISHABLE_TAGS],
          meta: { category: HUB_CATEGORY },
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

  // The catalogue route the hub reads, polled BEFORE the browser is pointed at
  // it: a hub that renders no card because the publish has not landed yet is a
  // race, and it would read as "the search box is broken".
  await expect
    .poll(
      async () => {
        const catalogue = await request.get(`${API_BASE}/elitea_core/public_applications/prompt_lib?query=${name}`);
        if (!catalogue.ok()) return false;
        const rows = ((await catalogue.json()) as { rows?: readonly { name?: string }[] }).rows ?? [];
        return rows.some((row) => row.name === name);
      },
      { timeout: 30_000, message: 'the published agent never reached the catalogue route the hub reads' },
    )
    .toBe(true);
  return agent;
}

/** Open the catalogue's Agents tab and wait for the hub's own search control. */
async function openAgentHub(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/elitea-catalog?tab=agents`);
  await expect(page.getByTestId('elitea-catalog')).toBeVisible({ timeout: 30_000 });
  await expect(hubSearch(page)).toBeVisible({ timeout: 30_000 });
}

/**
 * The hub's search field, by its accessible name.
 *
 * `CategoryFilter` gives the input an `aria-label` from its placeholder
 * precisely so it can be addressed this way; the control carries no test id of
 * its own and adding one would need an image rebuild for an assertion a role
 * query already makes.
 */
function hubSearch(page: Page): ReturnType<Page['getByRole']> {
  return page.getByRole('textbox', { name: 'Search for agents' });
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
 * onetest: ELITEA-0201 — a published agent is discoverable in the Agents
 * catalogue: found by typing its exact name, found again through its category
 * chip with the search cleared, and its card carries the agent's own name and description
 * and opens a detail view. (The case's final "and it answers a prompt" step is
 * a model turn and rides the chat-stream lane.)
 */
test('a published agent is findable in the catalogue by name and by category, and its card opens', async ({
  page,
  request,
}) => {
  // A create, a publish, a catalogue poll and three hub renders.
  test.setTimeout(120_000);

  const name = `${AUTOTEST_PREFIX}hub-${String(Date.now()).slice(-7)}`;
  const agent = await publishAgent(request, name);
  created.push(agent);

  await openAgentHub(page);

  // ── found by its exact name ───────────────────────────────────────────
  // The rows come back already filtered by the server's `?query=`
  // (AgentHub's own note), so this is a round trip and not a client-side
  // filter over whatever page happened to be loaded.
  await hubSearch(page).fill(name);
  // `.first()`, and the reason is worth keeping: a freshly published agent is
  // rendered in MORE THAN ONE section at once — its category's and the hub's
  // own "Trending" — so an unqualified locator is a strict-mode violation
  // rather than a missing card. Being in two sections is correct behaviour;
  // the assertion is that it is findable, not that it appears once.
  const card = page.getByRole('heading', { name, exact: true }).first();
  await expect(card, 'the published agent must be findable by its exact name').toBeVisible({ timeout: 30_000 });

  // ── the card opens, and the detail view carries the identity ──────────
  //
  // MEASURED, and not what the source case predicts: the hub CARD is a compact
  // 7rem tile carrying the agent's icon, name, author and like control
  // (`src/pages/agents-hub/ui/AgentCard.tsx`) — there is no description on it.
  // The description the case asks for lives one click away, in the detail
  // modal (`AgentModal.tsx`), together with the name and the author. So the
  // identity assertion is made where the product actually puts it rather than
  // where the case guessed, and the click that gets there is the case's own
  // next step.
  await card.click();
  const modal = page.getByTestId('agent-modal-copy-link');
  await expect(modal, 'clicking the card must open the agent’s detail view').toBeVisible({ timeout: 20_000 });
  await expect(
    page.getByText(descriptionFor(name), { exact: false }),
    'the detail view must carry the agent’s description',
  ).toBeVisible({ timeout: 15_000 });
  await expect(
    page.getByRole('heading', { name, exact: true }).first(),
    'the detail view must name the agent',
  ).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(modal, 'the detail view must close again').toBeHidden({ timeout: 15_000 });

  // ── found by its category, with the search cleared ───────────────────
  // The half the name search cannot answer: a consumer who does not know the
  // name browses the categories instead.
  await hubSearch(page).fill('');
  const categoryChip = page.getByRole('button', { name: HUB_CATEGORY, exact: true }).first();
  await expect(categoryChip, `the hub must offer the '${HUB_CATEGORY}' category`).toBeVisible({ timeout: 20_000 });
  await categoryChip.click();
  await expect(categoryChip, 'the chip must report itself selected').toHaveAttribute('aria-pressed', 'true');
  await expect(
    page.getByRole('heading', { name, exact: true }).first(),
    'the published agent must be reachable through its category with no search term',
  ).toBeVisible({ timeout: 30_000 });
});
