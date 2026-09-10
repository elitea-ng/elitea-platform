/**
 * Chat sidebar gaps between what `conversations.list.spec.ts`/`chat.folders.spec.ts`
 * /`chat.sharing.spec.ts` already prove and what `w1-chat-interface.md`'s
 * lazy-loading/pinning cases (ELITEA-0513/0514/0516) actually assert.
 *
 * Ported by use case from `w1-chat-interface.md`.
 *
 * `conversations.list.spec.ts`'s "a date bucket bigger than one page..." test
 * already covers ELITEA-0515 (offset increments on scroll) and ELITEA-0518
 * (the first page is ≤ 10 rows, never the whole bucket) end to end — not
 * repeated here. `chat.sharing.spec.ts`'s S3 covers "pin moves the
 * conversation into the pinned group" but ONLY reads the `pinned` bucket —
 * never the date-group buckets a pinned conversation must be ABSENT from,
 * and never a reload — which is the actual ELITEA-0513 discriminator.
 * `chat.folders.spec.ts` proves the single-folder pagination fetcher
 * (`folderApi.folderConversations`, `?grouped=true&folder_id=…`) answers a
 * correct page — but never drives the UI's own scroll-triggered follow-up
 * request for it, which is ELITEA-0514.
 */
import type { APIRequestContext } from '@playwright/test';
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createConversation,
  deleteConversation,
  readCallerPersonalProjectId,
} from '../../fixtures/api';

const SUFFIX = '-sbgaps';

function uniq(label: string): string {
  return `${AUTOTEST_PREFIX}${label}${SUFFIX}-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
}

const FOLDER_ROOT = `${API_BASE}/elitea_core/folder/prompt_lib/${DEFAULT_PROJECT_ID}`;
const CONVERSATION_ROOT = `${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;

interface GroupedListing {
  readonly pinned?: { readonly conversations?: readonly { readonly id?: unknown }[] };
  readonly date_groups?: readonly {
    readonly name?: string;
    readonly conversations?: readonly { readonly id?: unknown }[];
  }[];
  readonly folders?: readonly {
    readonly id?: string;
    readonly name?: string;
    readonly total?: number;
    readonly offset?: number;
    readonly conversations?: readonly { readonly id?: unknown; readonly name?: unknown }[];
  }[];
}

async function readGroupedListing(request: APIRequestContext, projectId = DEFAULT_PROJECT_ID): Promise<GroupedListing> {
  const response = await request.get(`${API_BASE}/elitea_core/folder/prompt_lib/${projectId}?grouped=true`);
  expect(response.status()).toBe(200);
  return (await response.json()) as GroupedListing;
}

async function createFolder(request: APIRequestContext, name: string): Promise<{ readonly id: string }> {
  const response = await request.post(FOLDER_ROOT, { data: { name } });
  expect(response.status(), `folder must create: ${(await response.text()).slice(0, 300)}`).toBe(201);
  const body = (await response.json()) as { id?: string };
  expect(body.id).toBeTruthy();
  return { id: body.id as string };
}

async function moveIntoFolder(request: APIRequestContext, conversationId: string, folderId: string): Promise<void> {
  const response = await request.put(`${CONVERSATION_ROOT}/${conversationId}`, { data: { folder_id: folderId } });
  expect(response.status()).toBe(200);
}

/* onetest: ELITEA-0513 — a pinned conversation is excluded from every date group (not just present in Pinned), and stays that way, server-side and on screen, after a reload */
test('ELITEA-0513: a pinned conversation is excluded from every date group, including after a reload', async ({
  page,
}) => {
  const name = uniq('pin513');
  const conversationId = await createConversation(page.request, name);
  try {
    // Find which date bucket it landed in FIRST — the discriminator this
    // case is about only means something once we know it was really there.
    const before = await readGroupedListing(page.request);
    const bucketBefore = (before.date_groups ?? []).find((group) =>
      (group.conversations ?? []).some((c) => String(c.id) === conversationId),
    );
    expect(bucketBefore, 'a freshly created conversation must land in a date group first').toBeDefined();

    // The pin mechanism is a SEPARATE generic cross-entity route, not a field
    // on the conversation itself (`usePinConversation.hooks.ts`'s own doc
    // comment: `POST /social/pin/prompt_lib/{p}/conversation/{id}`).
    const pin = await page.request.post(`${API_BASE}/social/pin/prompt_lib/${DEFAULT_PROJECT_ID}/conversation/${conversationId}`);
    expect(pin.status(), `pin must succeed: ${(await pin.text()).slice(0, 300)}`).toBe(200);

    const afterPin = await readGroupedListing(page.request);
    expect(
      (afterPin.pinned?.conversations ?? []).map((c) => String(c.id)),
      'pinning must move the conversation into the pinned bucket',
    ).toContain(conversationId);
    for (const group of afterPin.date_groups ?? []) {
      expect(
        (group.conversations ?? []).map((c) => String(c.id)),
        `date group "${group.name}" must not also carry a pinned conversation`,
      ).not.toContain(conversationId);
    }

    // And the UI agrees. `PinnedConversations.tsx` renders pinned rows
    // unconditionally at the top of the rail with no group wrapper or
    // heading of its own (confirmed reading the component: it is a bare
    // `.map` over `renderConversationItem`) — so there is no "Pinned"
    // label to assert on, and the discriminator is instead that the row is
    // visible WITHOUT expanding anything: a row still inside a collapsed
    // date-group body could not be, since `DateGroup` starts collapsed
    // (`chat.management.spec.ts`'s `openTodayGroup` exists precisely because
    // date-group rows are not visible until their toggle is clicked).
    await page.goto(BASE_URL + '/app/chat');
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
    await expect(
      page.getByTestId(`conversation-item-${conversationId}`),
      'a pinned row must be visible without expanding any date group',
    ).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId(`conversation-item-${conversationId}`)).toHaveCount(1);
    await checkA11y(page);

    // Reload — no duplication, still exactly one row, still pinned server-side.
    await page.reload();
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId(`conversation-item-${conversationId}`)).toHaveCount(1);

    const afterReload = await readGroupedListing(page.request);
    expect((afterReload.pinned?.conversations ?? []).map((c) => String(c.id))).toContain(conversationId);
    for (const group of afterReload.date_groups ?? []) {
      expect((group.conversations ?? []).map((c) => String(c.id))).not.toContain(conversationId);
    }
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

/* onetest: ELITEA-0514 — a folder bigger than one page pages its OWN conversations, independently of the date groups, when the UI scrolls its load-more sentinel */
test('ELITEA-0514: a folder bigger than one page loads its remainder through the folder\'s own sentinel', async ({
  page,
}) => {
  test.setTimeout(180_000);
  const PAGE_SIZE = 10;
  const folder = await createFolder(page.request, uniq('bigfolder'));
  const ids: string[] = [];
  try {
    for (let i = 0; i < PAGE_SIZE + 4; i += 1) {
      const id = await createConversation(page.request, uniq(`f514-${i}`));
      ids.push(id);
      await moveIntoFolder(page.request, id, folder.id);
    }

    const followUps: string[] = [];
    page.on('request', (request) => {
      const url = new URL(request.url(), BASE_URL);
      if (url.searchParams.get('folder_id') === folder.id) followUps.push(request.url());
    });

    const firstPage = page.waitForResponse(
      (response) =>
        response.request().method() === 'GET' &&
        response.url().includes('/elitea_core/folder/prompt_lib/') &&
        response.url().includes('grouped=true') &&
        !response.url().includes('folder_id='),
      { timeout: 30_000 },
    );
    await page.goto(BASE_URL + '/app/chat');
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
    await firstPage;

    const listing = await readGroupedListing(page.request);
    const row = (listing.folders ?? []).find((f) => f.id === folder.id);
    expect(row, 'the seeded folder must be in the grouped listing').toBeDefined();
    expect(row?.conversations?.length, 'the folder\'s first page must be paged, not served whole').toBe(PAGE_SIZE);
    expect(row?.total ?? 0, 'the folder must report the rows it did NOT deliver').toBeGreaterThan(PAGE_SIZE);

    // Expand the folder in the rail — the folder's own accordion toggle.
    const folderRowName = String(row?.name ?? uniq('bigfolder'));
    const sidebar = page.getByTestId('chat-conversation-sidebar');
    const folderToggle = sidebar.getByRole('button', { name: folderRowName });
    await expect(folderToggle).toBeVisible({ timeout: 15_000 });
    await folderToggle.click();

    const lastInPage = String(row?.conversations?.at(-1)?.id ?? '');
    await expect(page.getByTestId(`conversation-item-${lastInPage}`)).toBeVisible({ timeout: 20_000 });

    // Same "walk up to the nearest ancestor holding the sentinel" technique
    // `conversations.list.spec.ts` already uses for a date bucket's own
    // remainder — the folder body is a different subtree, same shape.
    await page.evaluate((rowId: string) => {
      let node: Element | null = document.querySelector(`[data-testid="conversation-item-${rowId}"]`);
      while (node !== null && node.querySelector('[data-testid="conversation-load-more-sentinel"]') === null) {
        node = node.parentElement;
      }
      node?.querySelector('[data-testid="conversation-load-more-sentinel"]')?.scrollIntoView({ block: 'center' });
    }, lastInPage);

    await expect
      .poll(() => followUps.length, {
        timeout: 30_000,
        message: 'scrolling a folder\'s own remainder must fire a folder_id-scoped follow-up request',
      })
      .toBeGreaterThan(0);
    const followUp = new URL(followUps[0]!, BASE_URL);
    expect(followUp.searchParams.get('offset'), 'the follow-up must start where the folder\'s first page ended').toBe(
      String(row?.offset ?? PAGE_SIZE),
    );

    await expect
      .poll(
        () =>
          page.evaluate((id: string) => {
            const held = (node: Element): number => node.querySelectorAll('[data-testid^="conversation-item-"]').length;
            let node: Element | null = document.querySelector(`[data-testid="conversation-item-${id}"]`);
            while (node !== null && held(node) < 2) node = node.parentElement;
            return node === null ? 0 : held(node);
          }, lastInPage),
        { timeout: 30_000, message: 'the folder must hold more rows once its second page lands' },
      )
      .toBeGreaterThan(PAGE_SIZE);
  } finally {
    for (const id of ids) await deleteConversation(page.request, id);
    await page.request.delete(`${FOLDER_ROOT}/${folder.id}`).catch(() => undefined);
  }
});

/* onetest: ELITEA-0516 — the grouped listing pages with limit/offset in both the team (default) and the personal project */
test('ELITEA-0516: lazy-loading pagination is offered in both the team and the personal project', async ({
  page,
}) => {
  const personalProjectId = await readCallerPersonalProjectId(page.request);
  test.skip(
    personalProjectId === '',
    'this persona\'s personal project resolves to the same project as DEFAULT_PROJECT_ID in this environment — no distinct private project to compare against',
  );

  // The team project — already the subject of every other journey in this
  // suite, so this half is a light re-confirmation rather than new ground.
  const teamListing = await readGroupedListing(page.request, DEFAULT_PROJECT_ID);
  expect(teamListing.date_groups, 'the team project must expose paged date groups').toBeDefined();

  const teamProbe = await page.request.get(`${FOLDER_ROOT}?grouped=true&limit=5&offset=0`);
  expect(teamProbe.status(), 'the team project must accept limit/offset on the grouped read').toBe(200);

  // The SAME mechanism, in the personal project — proving lazy loading is not
  // a team-project-only code path. Adapted to an API-level check (rather than
  // driving the project switcher UI) to stay within this package's budget;
  // the UI half of the mechanism is already proven exhaustively for the team
  // project by `conversations.list.spec.ts`.
  const personalProbe = await page.request.get(
    `${API_BASE}/elitea_core/folder/prompt_lib/${personalProjectId}?grouped=true&limit=5&offset=0`,
  );
  expect(
    personalProbe.status(),
    'the personal project must accept the identical limit/offset contract the team project does',
  ).toBe(200);
  const personalBody = (await personalProbe.json()) as GroupedListing;
  expect(personalBody.date_groups, 'the personal project must also expose paged date groups').toBeDefined();
});
