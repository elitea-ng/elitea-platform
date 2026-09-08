/**
 * Chat MANAGEMENT surfaces — the three things a user does to a conversation
 * that are not "send a message": delete it, manage who is in it, and open one
 * by link.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THESE THREE, AND WHY IN THE JOURNEYS STACK
 * ─────────────────────────────────────────────────────────────────────────────
 * `chat.conversation.spec.ts` next door covers the SEND path, and its module
 * header records what that stack cannot reach (no socket server, therefore no
 * assistant message, therefore no stop/regenerate). This file covers the
 * surfaces that need no turn at all, so all three run fully in
 * `deploy/docker-compose.e2e-standalone.yml` — no worker, no model.
 *
 * MEASURED LIMITS OF THIS STACK, so no assertion below quietly rests on one:
 *
 *  1. NOTHING CAN PERSIST A MESSAGE HERE. The only route that writes a
 *     `chat_message_group` is the runtime plane's agent-start
 *     (`POST /elitea_core/messages/prompt_lib/{p}/{c}`), which this stack does
 *     not mount — measured: it answers 405, not 404, i.e. the path exists for
 *     GET/DELETE and has no POST handler.
 *     `internal/api/v2/conversations/handler.go` also carried a `PostMessage`
 *     predict shim that DID insert message groups, but it was routed by
 *     nothing and has since been DELETED: it re-implemented that same URL,
 *     which the runtime plane owns (see the `NOTE(#126, #93)` left in its
 *     place). So there is no second writer to reach for here.
 *     Consequence for M3: a transcript with a foreign author cannot be seeded
 *     from the outside, so the "user bubble is not captioned with the reader's
 *     name" half is asserted in its ANONYMOUS form (nothing on the chat
 *     surface carries the reader's display name), and the discriminating half
 *     of the author-identity work is asserted where it IS observable — on the
 *     participants payload the page itself reads.
 *
 *  2. `GET /elitea_core/messages/prompt_lib/{p}/{c}` USED TO ANSWER 500 IN
 *     THIS STACK, for every conversation, empty ones included: the list query
 *     joins `{schema}.chat_messages_text`, and the E2E database is
 *     bootstrapped from `001_initial.sql`, which did not create that table.
 *     Closed in the same session — `create_tenant_schema` now declares the
 *     four chat payload tables (text, context, attachment, trace_step) plus
 *     the canvas pair, and the seed additionally runs the tenant history with
 *     `-all-tenants`. M3 still only asserts the page survives the read rather
 *     than pinning a status, because that is the property it is about; the
 *     status itself is pinned where it belongs, in the fresh-install
 *     integration tests under `internal/infra/db/repos`.
 *
 *  3. THE RAIL'S "USERS" ROW RENDERS (this was a defect, now fixed). It used
 *     not to render in any stack: `features/chat-participants/ui/
 *     Participants.tsx` filtered user participants through
 *     `entities/participant`'s `isParticipantStillActive`, which read the
 *     camelCase `entityName` off the raw snake_case wire rows the conversation
 *     payload carries — so every user row was dropped, twice over. The shape
 *     missed (`entityName` is `undefined` on a wire row), AND that predicate
 *     answers `false` for `user` BY DESIGN: the baseline uses it only to gate
 *     the last message's Regenerate control (`ChatMessageWrapper.jsx:148`),
 *     never as a rail-visibility filter — the baseline rail lists users by
 *     `entity_name === 'user'` alone (`ExpandedParticipantsList.jsx:50-56`).
 *     The fix drops that filter from the rail (users render through their own
 *     section, as in the baseline) and makes the selector read both shapes.
 *     M3 now asserts the RENDERED user row (`users-section` +
 *     `participant-item-<id>`) AND, independently, the resolved identity on
 *     the payload THE PAGE RECEIVED. The payload check is the stronger of the
 *     two — it discriminates a resolver that answers the reader instead of the
 *     named participant, which a rendered name alone cannot — so both stay.
 *     M2, which attaches no user, asserts the row is ABSENT: the fix must not
 *     manufacture an empty users row.
 *
 * Every entity this file creates carries the `autotest_` prefix and this
 * file's own `-mgmt` tag, and each test deletes what it made on its way out —
 * through `page.request`, which shares the browser context's session, NOT
 * through the bare `request` fixture the api fixture's own `createConversation`
 * records as unauthenticated here. A `deleteConversation` that answers 401 and
 * is not checked reads exactly like a sweep that worked. A test that FAILS
 * still leaves its rows, as every other chat journey already does; the rail
 * sorts newest-first, so M1's freshly created row stays at the top of the
 * group regardless.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createAgent,
  createConversation,
  deleteAgent,
  deleteConversation,
} from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-mgmt';

const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;
const PARTICIPANTS_PATH = `/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}`;
const PARTICIPANT_PATH = `/elitea_core/participant/prompt_lib/${DEFAULT_PROJECT_ID}`;

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
}

/** One participant row exactly as `GET /conversation/...` and the attach echo state it. */
interface ParticipantRow {
  readonly id: number | string;
  readonly entity_name?: string;
  readonly entity_meta?: { readonly id?: string | number; readonly name?: string };
  readonly entity_settings?: { readonly version_id?: string | number };
  readonly meta?: { readonly name?: string; readonly user_name?: string };
}

/** The signed-in persona, read from the same endpoint the app itself reads it from. */
async function readAuthor(request: APIRequestContext): Promise<{ readonly id: string; readonly name: string }> {
  const response = await request.get(`${API_BASE}/social/author`);
  expect(response.status(), 'the persona must be resolvable, or nothing below means anything').toBe(200);
  const body = (await response.json()) as { id?: string; name?: string };
  expect(body.id, '/social/author must name the caller').toMatch(/^\d+$/);
  expect(body.name, 'the caller must have a display name to be wrongly captioned WITH').not.toBe('');
  return { id: body.id as string, name: body.name as string };
}

/**
 * Some OTHER seeded user — the foreign author M3 needs.
 *
 * Read from the project's user listing rather than hardcoded: the seed
 * provisions its personas by email, and their ids are whatever the database
 * assigned. A hardcoded id would silently attach a participant that resolves
 * to nobody, which is the same payload shape as the defect M3 is about.
 */
async function readOtherUser(
  request: APIRequestContext,
  selfId: string,
): Promise<{ readonly id: string; readonly name: string }> {
  const response = await request.get(
    `${API_BASE}/admin/users/default/${DEFAULT_PROJECT_ID}?limit=200&offset=0`,
  );
  expect(response.status(), 'the user listing backs the foreign-author fixture').toBe(200);
  const body = (await response.json()) as { rows?: readonly { id?: string; name?: string }[] };
  const other = (body.rows ?? []).find((row) => row.id !== undefined && row.id !== selfId && row.name);
  expect(other, 'the seed must provision a SECOND user, or "foreign author" has no meaning').toBeDefined();
  return { id: String(other?.id), name: String(other?.name) };
}

/** POST the participants array and hand back what the SERVER stored for it. */
async function attachParticipants(
  request: APIRequestContext,
  conversationId: string,
  bodies: readonly Record<string, unknown>[],
): Promise<readonly ParticipantRow[]> {
  const response = await request.post(`${API_BASE}${PARTICIPANTS_PATH}/${conversationId}`, { data: bodies });
  expect(
    response.status(),
    `the participants must attach: ${(await response.text()).slice(0, 300)}`,
  ).toBe(200);
  return (await response.json()) as readonly ParticipantRow[];
}

/** The conversation's participants as the server holds them right now. */
async function readParticipants(
  request: APIRequestContext,
  conversationId: string,
): Promise<readonly ParticipantRow[]> {
  const response = await request.get(`${API_BASE}${CONVERSATION_PATH}/${conversationId}`);
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { participants?: readonly ParticipantRow[] };
  return body.participants ?? [];
}

/**
 * Opens the rail's date group that holds today's conversations.
 *
 * `DateGroup` starts collapsed, so its rows are mounted but not visible — a
 * plain `.click()` on a row would time out on "element is not visible" and
 * read like a missing row. Driven off `aria-expanded` rather than clicked
 * unconditionally, so a future default of "expanded" closes nothing.
 */
async function openTodayGroup(page: Page): Promise<void> {
  const sidebar = page.getByTestId('chat-conversation-sidebar');
  await expect(sidebar).toBeVisible({ timeout: 20_000 });
  const today = sidebar.getByRole('button', { name: 'Today' });
  await expect(today).toBeVisible({ timeout: 20_000 });
  if ((await today.getAttribute('aria-expanded')) !== 'true') await today.click();
}

/** Opens the participants rail, which `ChatPage` mounts collapsed (baseline `NewChat.jsx:166`). */
async function openParticipantsRail(page: Page): Promise<void> {
  const expand = page.getByRole('button', { name: 'Expand participants' });
  await expect(expand).toBeVisible({ timeout: 20_000 });
  await expand.click();
  await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 10_000 });
}

// ─────────────────────────────────────────────────────────────────────────────
// M1: delete a conversation from the rail
//
// The regression this pins: `useConversationSidebar.deleteConversation` used
// to filter only `conversations`/`pinnedConversations`, while the visible rows
// render out of `dateGroups`/`folders` — so a successful DELETE left the row
// on screen and the transcript open, and only a reload made the conversation
// look gone. Both halves are asserted WITHOUT reloading, and a page-lifetime
// sentinel proves no reload happened, because a reload would make the
// assertions pass against the very bug they exist for.
// ─────────────────────────────────────────────────────────────────────────────
test('M1: deleting a conversation removes its rail row, blanks the route and 404s the read', async ({ page }) => {
  const name = uniqueName('m1');
  const conversationId = await createConversation(page.request, name);

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await openTodayGroup(page);

  const row = page.getByTestId(`conversation-item-${conversationId}`);
  await expect(row, 'a conversation created over the API must appear in the rail').toBeVisible({ timeout: 15_000 });
  await expect(row).toContainText(name);
  await checkA11y(page);

  // Open it: `deleteConversation` only navigates away from a transcript that
  // is ACTIVE, and the sidebar's active conversation is set by a click, not by
  // the route (`useConversationSidebar`'s restore-vs-click split).
  await row.click();
  await expect(page).toHaveURL(new RegExp(`/app/chat/${conversationId}$`), { timeout: 15_000 });

  // Anything set on this page object dies with a reload or a navigation that
  // is not client-side. It is read back after the delete below.
  await page.evaluate(() => {
    (window as unknown as { __eliteaMgmt?: string }).__eliteaMgmt = 'alive';
  });

  /*
   * The kebab is `display: none` until the row is hovered — `ConversationItem`
   * drives its menu wrapper off `isHovering` state with no CSS `:hover`
   * fallback (`ConversationItem.styles.ts`) — so hovering the row is the
   * interaction that reveals what the next line clicks.
   *
   * This step used to carry a pointer-jiggle workaround (`mouse.move(0, 0)`
   * then `row.hover()`) that forced `DateGroup` to re-render the row and
   * refresh a STALE `deleteConversation` closure. `useConversationSidebar`
   * read `activeConversation` from the closure its handler was built in, and a
   * memoised `ConversationItem` — reached through `useRenderConversationItem`'s
   * `useCallback([])` render-prop, which reads its inputs from a `useLatestRef`
   * so its identity never changes — kept the closure captured BEFORE the row
   * was selected. That closure's `activeConversation?.id === conversation.id`
   * test was false, so the DELETE landed and the row vanished but the route
   * stayed on the deleted transcript (flaked 2 runs in 3 under `--workers=3`).
   * `deleteConversation` now reads the active conversation LIVE (via a ref), so
   * the navigate-away branch is correct regardless of when the handler closure
   * was created, and the jiggle is no longer needed.
   */
  const trigger = page.locator(`#conversation-menu-${conversationId}-trigger`);
  await row.hover();
  await expect(trigger).toBeVisible({ timeout: 5_000 });
  await trigger.click();

  const menu = page.locator(`#conversation-menu-${conversationId}-menu`);
  await expect(menu).toBeVisible({ timeout: 5_000 });
  await menu.getByRole('menuitem', { name: 'Delete' }).click();

  // The first Delete only ARMS the confirmation — `ControlsDropdown` replaces
  // the row in place with the message and a Cancel/Delete pair. A menu that
  // deleted on the first click would fail here, since the row it needs is the
  // one that only exists in the confirming state.
  await expect(menu).toContainText("Are you sure to delete conversation? It can't be restored.");
  await expect(menu.getByRole('menuitem', { name: 'Cancel' })).toBeVisible();

  const deleted = page.waitForResponse(
    (r) => r.url().includes(`${CONVERSATION_PATH}/${conversationId}`) && r.request().method() === 'DELETE',
    { timeout: 20_000 },
  );
  await menu.getByRole('menuitem', { name: 'Delete' }).click();
  expect((await deleted).status(), 'the confirmed delete must reach the server').toBe(204);

  // The row leaves the rail and the route falls back to the blank composer —
  // both in the SAME page lifetime.
  await expect(row).toHaveCount(0);
  await expect(trigger).toHaveCount(0);
  // `toHaveURL`, not `waitForURL`: this is a client-side route change, and
  // `waitForURL` waits on a navigation lifecycle event that a history push
  // never fires. Retried polling of the address itself is what this asserts.
  await expect(page).toHaveURL(/\/app\/chat$/, { timeout: 15_000 });
  await expect(page.getByTestId('chat-message-input')).toBeEditable();
  await expect(page.getByTestId('chat-message-input')).toHaveValue('');
  expect(
    await page.evaluate(() => (window as unknown as { __eliteaMgmt?: string }).__eliteaMgmt),
    'the rail and the route must settle without a reload — a reload would pass this test against the bug it exists for',
  ).toBe('alive');

  // Gone from the store too, not just from the screen.
  const read = await page.request.get(`${API_BASE}${CONVERSATION_PATH}/${conversationId}`);
  expect([404, 410], `a deleted conversation must stop resolving (got ${read.status()})`).toContain(read.status());

  await checkA11y(page);
});

// ─────────────────────────────────────────────────────────────────────────────
// M2: manage the participants of a conversation
//
// An agent attached over the REST API must reach the rail, and the rail's own
// remove control must delete the mapping SERVER-side. The server read at the
// end is what discriminates: `DeleteParticipantButton` performs no request of
// its own — it hands the participant back to the page through `onDelete` — so
// a wrapper that drops that callback still closes its dialog and still removes
// nothing, and only the conversation's own participants list can tell.
// ─────────────────────────────────────────────────────────────────────────────
test('M2: an agent participant renders in the rail and the rail control removes it server-side', async ({ page }) => {
  const conversationId = await createConversation(page.request, uniqueName('m2'));
  const agentName = uniqueName('m2ag');
  const agent = await createAgent(page.request, agentName);

  const attached = await attachParticipants(page.request, conversationId, [
    {
      entity_name: 'application',
      entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID, name: agentName },
      // The agent resolver joins the participant on this id; a participant
      // without it is a row the product never produces.
      entity_settings: { version_id: agent.versionId },
    },
  ]);
  expect(attached.map((p) => p.entity_name)).toEqual(['application']);
  expect(String(attached[0]?.entity_settings?.version_id)).toBe(String(agent.versionId));
  const participantId = String(attached[0]?.id);

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
  await openParticipantsRail(page);

  const agents = page.getByTestId('participants-section-Agents');
  await expect(agents, 'the attached agent must reach the rail').toBeVisible({ timeout: 15_000 });
  await expect(agents).toContainText('Agents (1)');
  await expect(agents).toContainText(agentName);
  // No user participant was attached, so the users row must not appear — the
  // Users-row fix renders that section only for real user participants, never
  // an empty one (module header, note 3).
  await expect(page.getByTestId('users-section')).toHaveCount(0);
  await checkA11y(page);

  // The per-participant action bar renders only while the card is hovered.
  await agents.getByText(agentName).hover();
  const remove = agents.getByRole('button', { name: /Remove agent/i });
  await expect(remove).toBeVisible({ timeout: 5_000 });
  await remove.click();

  // Confirmation first: the button's own regression was calling `onDelete`
  // straight out of the icon's `onClick`, with no dialog at all.
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible({ timeout: 5_000 });
  await expect(dialog).toContainText(agentName);

  const removed = page.waitForResponse(
    (r) =>
      r.url().includes(`${PARTICIPANT_PATH}/${conversationId}/${participantId}`) &&
      r.request().method() === 'DELETE',
    { timeout: 20_000 },
  );
  await dialog.getByRole('button', { name: 'Remove' }).click();
  expect((await removed).status()).toBe(204);

  // The server-side list is what "removed" means. Polled, because the page
  // refetches the conversation after the mutation settles.
  await expect
    .poll(async () => (await readParticipants(page.request, conversationId)).length, {
      timeout: 15_000,
      message: 'the confirmed removal left the participant mapping in place',
    })
    .toBe(0);

  await expect(page.getByTestId('participants-section-Agents')).toHaveCount(0);

  await deleteConversation(page.request, conversationId);
  await deleteAgent(page.request, agent.id);
});

// ─────────────────────────────────────────────────────────────────────────────
// M3: open a conversation cold, by link
//
// Cold means this page object has never been to `/chat`: the route resolves
// the conversation, its participants and its transcript from nothing but the
// URL and the session. Two things are asserted about that load.
//
// FIRST, the author identity. The attach below posts a user participant
// carrying ONLY `entity_meta.id` — no name, no email, exactly what the REST
// path stores. `ListParticipants` resolves the display name from
// `auth_core__user` and overlays it as `meta.user_name`; without that, a
// transcript's author captions read "User <n>", and the web client's own
// fallback used to caption them with whoever was READING. The id posted is a
// DIFFERENT user from the caller, so a resolver that answered the caller
// instead of the named participant fails here.
//
// The assertion is made against the response THE PAGE received, not against a
// second request from the test: a payload that carries the identity only when
// the test asks for it would not put a name on anyone's screen.
//
// SECOND, the anonymous transcript. This stack cannot persist a message group
// at all (module header, note 1), so there is no foreign-authored bubble to
// caption; what remains assertable is that nothing on the chat surface carries
// the READER's name, which is the shape the removed fallback produced.
// ─────────────────────────────────────────────────────────────────────────────
test('M3: a cold deep link resolves a foreign participant server-side and captions nothing with the reader', async ({ page }) => {
  const reader = await readAuthor(page.request);
  const foreign = await readOtherUser(page.request, reader.id);
  expect(foreign.name, 'the foreign author must be distinguishable from the reader').not.toBe(reader.name);

  const conversationId = await createConversation(page.request, uniqueName('m3'));
  const agentName = uniqueName('m3ag');
  const agent = await createAgent(page.request, agentName);

  const attached = await attachParticipants(page.request, conversationId, [
    // Only the id. Any name on this payload would have been put there by this
    // test, and would prove nothing about the server's own resolution.
    { entity_name: 'user', entity_meta: { id: foreign.id } },
    {
      entity_name: 'application',
      entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID, name: agentName },
      entity_settings: { version_id: agent.versionId },
    },
  ]);
  const storedUser = attached.find((p) => p.entity_name === 'user');
  expect(storedUser?.meta?.user_name, 'the server resolves a user participant’s display name').toBe(foreign.name);

  // Cold: armed before the navigation, so what is asserted is the payload the
  // ROUTE fetched for itself.
  const detail = page.waitForResponse(
    (r) => r.url().includes(`${CONVERSATION_PATH}/${conversationId}`) && r.request().method() === 'GET',
    { timeout: 30_000 },
  );
  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);

  const payload = (await (await detail).json()) as { participants?: readonly ParticipantRow[] };
  const pageUser = (payload.participants ?? []).find((p) => p.entity_name === 'user');
  expect(pageUser, 'the page’s own read must carry the user participant').toBeDefined();
  expect(pageUser?.meta?.user_name, 'the identity the PAGE received names the participant').toBe(foreign.name);
  expect(pageUser?.meta?.user_name, 'and never the reader — that substitution is the defect').not.toBe(reader.name);
  expect(String(pageUser?.entity_meta?.id)).toBe(foreign.id);

  // The deep link survives its own load. `networkidle` first: the composer
  // does render for a frame before the route's queries settle, so an
  // unsettled assertion can catch the pre-crash frame and call it a pass.
  await page.waitForLoadState('networkidle');
  await expect(page.getByText('Something went wrong.')).toHaveCount(0);
  await expect(page.getByTestId('chat-message-input')).toBeEditable();
  expect(page.url()).toContain(`/app/chat/${conversationId}`);

  // The payload really drove the screen: BOTH the conversation's agent and
  // its user participant are rendered. The user row is the participants-rail
  // fix (module header, note 3) — it lists users by group membership alone,
  // no longer through `isParticipantStillActive`, so the foreign user reaches
  // the rail with the display name the server resolved into `meta.user_name`.
  await openParticipantsRail(page);
  await expect(page.getByTestId('participants-section-Agents')).toContainText(agentName);

  const usersSection = page.getByTestId('users-section');
  await expect(usersSection, 'the user participant must reach the rail after the fix').toBeVisible({
    timeout: 15_000,
  });
  await expect(usersSection).toContainText(foreign.name);
  // Keyed on `entity_meta.id`, the only id a REST-attached user row carries —
  // proving the rendered row is THIS foreign participant, not some placeholder.
  // (The reader-substitution guard stays where it discriminates: on the
  // payload above and on the chat surface below, not here — a real staff
  // name can be a substring of another, which a section-scoped text check
  // cannot tell apart.)
  await expect(page.getByTestId(`participant-item-${foreign.id}`)).toBeVisible();

  // Nothing on the chat surface is captioned with the reader's name. No row
  // can be, in this stack — see the header — so this is a standing guard
  // against the reader-name fallback returning, not a demonstration of it.
  await expect(page.getByTestId('user-message')).toHaveCount(0);
  await expect(page.getByTestId('chat-message-list').getByText(reader.name, { exact: true })).toHaveCount(0);

  await checkA11y(page);

  await deleteConversation(page.request, conversationId);
  await deleteAgent(page.request, agent.id);
});

// ─────────────────────────────────────────────────────────────────────────────
// M1b: cancelling the delete confirmation keeps the conversation
//
// Ported from the legacy public suite:
//   `tests/ui/chat/test_conversation_management.py::TestConversationActions::
//    test_delete_conversation_cancel` (TC-CONV-007b)
//
// M1 above proves the CONFIRM path. Nothing proved the other one, and the two
// share every step but the last click — which is exactly the shape in which a
// destructive control goes wrong: `ControlsDropdown` replaces the menu row in
// place with a Cancel/Delete pair, so "Cancel" is a sibling of the button that
// deletes, one row apart. A Cancel wired to the same handler, or a first
// "Delete" click that already deleted, both look correct until someone cancels.
//
// The server read at the end is what discriminates. A conversation whose row
// stayed on the rail because the rail never refreshed reads exactly like one
// that was not deleted, and only the store can tell the two apart.
// ─────────────────────────────────────────────────────────────────────────────
test('M1b: cancelling the delete confirmation leaves the conversation on the rail and in the store', async ({ page }) => {
  const name = uniqueName('m1b');
  const conversationId = await createConversation(page.request, name);

  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await openTodayGroup(page);

  const row = page.getByTestId(`conversation-item-${conversationId}`);
  await expect(row).toBeVisible({ timeout: 15_000 });
  await expect(row).toContainText(name);

  // The kebab is `display: none` until the row is hovered (`ConversationItem.styles.ts`).
  const trigger = page.locator(`#conversation-menu-${conversationId}-trigger`);
  await row.hover();
  await expect(trigger).toBeVisible({ timeout: 5_000 });
  await trigger.click();

  const menu = page.locator(`#conversation-menu-${conversationId}-menu`);
  await expect(menu).toBeVisible({ timeout: 5_000 });
  await menu.getByRole('menuitem', { name: 'Delete' }).click();
  await expect(menu).toContainText("Are you sure to delete conversation? It can't be restored.");

  // No DELETE may leave the browser at all — not before the confirmation, and
  // not because of the Cancel. Registered here so a request the arming click
  // itself issued would already have been counted.
  const deletes: string[] = [];
  page.on('request', (request) => {
    if (request.method() === 'DELETE' && request.url().includes(`${CONVERSATION_PATH}/${conversationId}`)) {
      deletes.push(request.url());
    }
  });

  await menu.getByRole('menuitem', { name: 'Cancel' }).click();

  // The confirmation closed and the menu went back to its ordinary rows — the
  // Delete entry is offerable again, which is what "cancelled" means here.
  await expect(menu).not.toContainText("Are you sure to delete conversation?");

  // The row is still on the rail, in the same page lifetime.
  await expect(row).toBeVisible();
  await expect(row).toContainText(name);

  // …and the conversation is still in the store. Read after a round-trip so a
  // DELETE the Cancel click issued has been sent by the time it is counted.
  await page.evaluate(() => undefined);
  expect(deletes, 'Cancel must not reach the delete route').toEqual([]);
  const read = await page.request.get(`${API_BASE}${CONVERSATION_PATH}/${conversationId}`);
  expect(read.status(), 'a cancelled delete must leave the conversation resolvable').toBe(200);
  expect(((await read.json()) as { name?: string }).name).toBe(name);

  await deleteConversation(page.request, conversationId);
});

// ─────────────────────────────────────────────────────────────────────────────
// M2b: adding a participant through the composer's own picker
//
// Ported from the legacy public suite:
//   `tests/ui/chat/test_chat_interface.py::TestHashSearch::test_hash_search_participants`
//   `tests/ui/chat/test_chat_interface.py::TestHashSearch::test_add_participant_via_hash_search`
//   (TC-CHAT-017 / TC-CHAT-018)
//
// THE TRIGGER IS DIFFERENT HERE, AND THAT IS THE PORT'S WHOLE POINT. The legacy
// UI opened a participant search by typing "#". elitea-web has no "#" trigger:
// `useSlashMention` and its siblings own "/", "@" and "~" only, and "#" appears
// in this codebase solely as the toolkit/tool SEPARATOR inside an already
// committed mention (`instructionsMention.utils.ts`). The affordance that does
// the legacy test's job is the composer's "+" menu, whose Agents submenu is a
// searchable list of the project's agents wired to
// `useAddEntityParticipant.onSelectParticipant`. So the use case — "find a
// participant from the composer and add it to this conversation" — is ported
// onto the control that has it, rather than onto a keystroke this app does not
// implement.
//
// M2 above attaches its agent over the REST API and then removes it through the
// rail. This is the other direction, and it is the one a user takes.
// ─────────────────────────────────────────────────────────────────────────────
test('M2b: the composer’s picker finds an agent by name and attaches it to the conversation', async ({ page }) => {
  const conversationId = await createConversation(page.request, uniqueName('m2b'));
  const agentName = uniqueName('m2bag');
  const agent = await createAgent(page.request, agentName);

  // Nothing is attached yet, so what the picker does below is the only thing
  // that could have attached anything.
  expect(await readParticipants(page.request, conversationId)).toEqual([]);

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

  // The "+" menu's four entity queries are deferred until the menu is opened
  // for the first time (`usePlusMenuEntities`'s `onOpen` gate), so the agents
  // list does not exist before this click.
  const plus = page.getByTestId('plus-menu-button');
  await expect(plus).toBeEnabled({ timeout: 20_000 });
  await plus.click();
  const agentsRow = page.getByTestId('plus-menu-agents');
  await expect(agentsRow).toBeVisible({ timeout: 10_000 });
  await agentsRow.click();

  // The submenu is a real, populated list — the legacy "typing the trigger
  // opens a participant search" assertion, on this app's trigger.
  const options = page.getByTestId('plus-submenu-item');
  await expect(options.first()).toBeVisible({ timeout: 15_000 });

  // Its search narrows that list. `exact: true` matters: the conversation
  // rail's own search bar reads "Search conversations..." and a substring
  // placeholder match would find whichever of the two happened to be mounted.
  const search = page.getByPlaceholder('Search...', { exact: true });
  await expect(search).toBeVisible();
  await search.fill(agentName);

  // `toEntityItems` keys each row `agent-<applications.id>`, which is the only
  // identity that survives two agents sharing a display name.
  const target = page.locator(`[data-testid="plus-submenu-item"][data-item-key="agent-${agent.id}"]`);
  await expect(target, 'the agent created for this journey must be findable in the picker').toHaveCount(1);
  await expect(options, 'the search must narrow the list, not merely accept text').toHaveCount(1);
  await expect(target).toContainText(agentName);

  await target.click();

  // The attach is a SERVER-side mapping, and that is what is asserted: the
  // picker's row could close the menu and change nothing (its own
  // `applyParticipantSelection` resolves without attaching on several paths),
  // which no screen assertion can tell apart from a working one.
  await expect
    .poll(async () => (await readParticipants(page.request, conversationId)).map((p) => p.entity_name), {
      timeout: 20_000,
      message: 'selecting an agent in the "+" picker must attach it to the conversation',
    })
    .toEqual(['application']);

  const attached = await readParticipants(page.request, conversationId);
  // The agent that was picked, and the version the agent resolver joins on —
  // a participant row without `entity_settings.version_id` is one the product
  // never produces (see M2).
  expect(String(attached[0]?.entity_meta?.id)).toBe(String(agent.id));
  expect(attached[0]?.entity_settings?.version_id).toBeDefined();

  // …and it reaches the rail the user reads it from.
  await openParticipantsRail(page);
  const agents = page.getByTestId('participants-section-Agents');
  await expect(agents).toBeVisible({ timeout: 15_000 });
  await expect(agents).toContainText(agentName);

  await checkA11y(page);

  await deleteConversation(page.request, conversationId);
  await deleteAgent(page.request, agent.id);
});
