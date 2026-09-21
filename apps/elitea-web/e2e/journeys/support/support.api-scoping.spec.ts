/**
 * WHOSE SUPPORT CONVERSATION IS IT — the four onetest cases about the
 * boundary around a support conversation (#939 group 7, the SUPPORT-LANE
 * rows of `ledger-Z2.tsv`).
 *
 * ELITEA-0603, 0604, 0606 and 0613 all ask one question from four sides: a
 * support conversation belongs to the ONE user who opened it, and to the
 * support surface alone. `internal/api/v2/supportassistant` answers it with a
 * single predicate — `author_id = <caller> AND source = 'support'`
 * (`store.go`) — and every route that addresses a conversation goes through
 * `conversationOwnedByCaller`. These tests are what makes that a measured
 * claim rather than a comment.
 *
 * ## WHY THIS FILE IS IN THE `support-stack` PROJECT
 *
 * Nothing here runs a model turn: a conversation is created by
 * `POST /support_assistant/conversations`, which writes a row and returns, and
 * every other call is a read. But the facade REFUSES every one of its gated
 * routes unless the assistant is READY — enabled, bootstrapped and pointed at
 * an agent (`requireSupportProject`) — and turning it on is a platform-wide
 * admin write. It therefore belongs beside `support.spec.ts` on the stack that
 * owns that switch for the length of a run, rather than in the journeys lane
 * where every other spec's page would grow a floating launcher.
 *
 * ## THE SECOND USER IS A SECOND PERSONA, NOT A SECOND NAME
 *
 * "User-B cannot see User-A's conversation" is only a claim about
 * authorization if User-B's request really carries a different identity. The
 * member persona's storage state is loaded into its own request context here,
 * so the two callers differ by the cookie the edge reads and by nothing this
 * test controls — the same reason `e2e/fixtures/pipelineTriggers.ts` mints an
 * anonymous context rather than reusing the page's.
 *
 * The support project enrols ANY authenticated caller as a viewer on first
 * contact (`resolve`, before the permission gate), so the member persona needs
 * no seeding of its own: its first call to the facade enrols it.
 *
 * ## 404, NOT 403, AND THAT IS THE DESIGNED ANSWER
 *
 * ELITEA-0604 words its two steps as "403 Forbidden or 404 Not Found" and
 * "403 Forbidden"; this service answers 404 to both, deliberately —
 * `conversationOwnedByCaller`'s own comment: "'not yours' and 'does not
 * exist' must answer the same 404 — a distinct 403 for somebody else's
 * conversation would confirm that the UUID names a real support conversation
 * belonging to a real other user." The case's PASS criteria allows exactly
 * that ("Non-owner gets 403 or 404 on both GET and POST"), so the assertions
 * below accept either and pin what actually matters: no body, no data, no
 * message written.
 */
import { randomUUID } from 'node:crypto';

import type { APIRequestContext } from '@playwright/test';
import { expect, request as apiRequest, test } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, createAgent, deleteAgent, DEFAULT_PROJECT_ID } from '../../fixtures/api';

import { disableSupportAssistant, enableSupportAssistant } from './helpers';

const SUPPORT_PROJECT_ID = Number(DEFAULT_PROJECT_ID);
const RUN_ID = Date.now();
const AGENT_NAME = `${AUTOTEST_PREFIX}support_scoping_agent_${RUN_ID}`;

/** One entry of `GET /support_assistant/conversations`. */
interface SupportConversation {
  readonly uuid: string;
  readonly name: string;
}

/** Open a support conversation as the caller behind `api`, and return it. */
async function createSupportConversation(api: APIRequestContext, name: string): Promise<SupportConversation> {
  const response = await api.post(`${API_BASE}/support_assistant/conversations`, { data: { name } });
  expect(
    response.ok(),
    `POST /support_assistant/conversations -> ${response.status()}: ${(await response.text()).slice(0, 300)}`,
  ).toBe(true);
  const body = (await response.json()) as { uuid?: string; name?: string };
  expect(body.uuid, 'a created support conversation must carry a uuid').toBeTruthy();
  return { uuid: body.uuid ?? '', name: body.name ?? name };
}

/** Every support conversation the caller behind `api` can list. */
async function listSupportConversations(api: APIRequestContext): Promise<readonly SupportConversation[]> {
  const response = await api.get(`${API_BASE}/support_assistant/conversations?limit=100`);
  expect(
    response.ok(),
    `GET /support_assistant/conversations -> ${response.status()}: ${(await response.text()).slice(0, 300)}`,
  ).toBe(true);
  const body = (await response.json()) as { items?: readonly SupportConversation[] };
  return body.items ?? [];
}

/** Every conversation the REGULAR chat listing answers for one project. */
async function listRegularConversations(
  api: APIRequestContext,
  projectId: number,
): Promise<readonly Record<string, unknown>[]> {
  const response = await api.get(
    `${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}?limit=100`,
  );
  expect(response.ok(), `the regular conversation listing must answer: ${response.status()}`).toBe(true);
  const body = (await response.json()) as { rows?: unknown; items?: unknown };
  const rows = Array.isArray(body.rows) ? body.rows : Array.isArray(body.items) ? body.items : [];
  return rows as readonly Record<string, unknown>[];
}

test.describe('support conversations belong to one user and to the support surface', () => {
  test.describe.configure({ mode: 'serial' });

  let agentId = '';
  /** The member persona's own request context — see the file header. */
  let member: APIRequestContext | undefined;

  test.beforeAll(async ({ request }) => {
    const agent = await createAgent(request, AGENT_NAME);
    agentId = agent.id;
    await enableSupportAssistant(request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentId),
      name: `${AUTOTEST_PREFIX}Support scoping ${RUN_ID}`,
      welcomeMessage: `${AUTOTEST_PREFIX}Welcome.`,
      placeholder: `${AUTOTEST_PREFIX}Ask...`,
    });
    member = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.member });
  });

  test.afterAll(async ({ request }) => {
    await member?.dispose();
    // The switch goes back OFF so a kept stack does not carry the assistant
    // into whatever runs next, and the agent this file made is removed. The
    // conversations are NOT deleted: the facade removed its delete route on
    // purpose, so there is no API left to clean them up with (see
    // `support.spec.ts`'s own note).
    await disableSupportAssistant(request).catch(() => {});
    if (agentId) await deleteAgent(request, agentId).catch(() => {});
  });

  /*
   * onetest: ELITEA-0603 — a support conversation is absent from the REGULAR
   * project listing and present in the support one. The support project and
   * the regular project are the SAME project here (the admin section points
   * `support_project_id` at project 1), which makes this the strongest form
   * of the case: the two listings read the same tenant schema and the same
   * table, and only `source = 'support'` separates them.
   */
  test('SUP-S1: a support conversation never appears in the regular conversation listing', async ({
    page,
  }) => {
    test.setTimeout(120_000);
    const name = `${AUTOTEST_PREFIX}support_only_${RUN_ID}`;
    const conversation = await createSupportConversation(page.request, name);

    const regular = await listRegularConversations(page.request, SUPPORT_PROJECT_ID);
    const regularIdentifiers = regular.flatMap((row) =>
      [row['uuid'], row['id'], row['name']].filter((value): value is string => typeof value === 'string'),
    );
    expect(
      regularIdentifiers,
      'the support conversation leaked into the regular project listing — `source = support` is what keeps ' +
        'support history out of a user’s own chat list',
    ).not.toContain(conversation.uuid);
    expect(regularIdentifiers, 'the support conversation leaked into the regular listing by name').not.toContain(
      name,
    );

    // And it really exists: an assertion about an ABSENCE is worth nothing
    // unless the row is there to be absent from.
    const support = await listSupportConversations(page.request);
    expect(
      support.map((entry) => entry.uuid),
      'the conversation must be listed by the support API it was created through',
    ).toContain(conversation.uuid);
  });

  /*
   * onetest: ELITEA-0604 — a non-owner can neither read another user's
   * support conversation nor post a message into it, while the owner can read
   * it. The POST is the half that matters most: a refusal that only covered
   * the read would leave a way to write into somebody else's transcript.
   */
  test('SUP-S2: a non-owner is refused on both the read and the message write, and the owner is not', async ({
    page,
  }) => {
    test.setTimeout(120_000);
    const conversation = await createSupportConversation(
      page.request,
      `${AUTOTEST_PREFIX}owner_only_${RUN_ID}`,
    );
    expect(member, 'the member persona context must be open').toBeDefined();
    const other = member as APIRequestContext;

    const read = await other.get(`${API_BASE}/support_assistant/conversation/${conversation.uuid}`);
    expect(
      [403, 404],
      `a non-owner read answered ${read.status()} — it must not be a 200 with another user’s data`,
    ).toContain(read.status());
    expect(
      (await read.text()).includes(conversation.uuid),
      'the refusal body must not echo the conversation it refused to serve',
    ).toBe(false);

    // A WELL-FORMED message, deliberately. `Predict` validates the body
    // BEFORE it resolves ownership (`predict.go`: content, then a lowercase
    // `question_id` UUID, then `conversationOwnedByCaller`), so a probe with
    // a missing `question_id` answers 400 and would prove nothing about who
    // may write into whose conversation — measured, on the first run of this
    // test.
    const write = await other.post(`${API_BASE}/support_assistant/predict/${conversation.uuid}`, {
      data: {
        content: `${AUTOTEST_PREFIX}message from a non-owner`,
        question_id: randomUUID(),
      },
    });
    expect(
      [403, 404],
      `a non-owner message write answered ${write.status()} — a refusal here is the whole case`,
    ).toContain(write.status());

    // The owner still reads it, so the two refusals above are about WHO asked
    // and not about a conversation that stopped working.
    const owner = await page.request.get(`${API_BASE}/support_assistant/conversation/${conversation.uuid}`);
    expect(owner.status(), 'the owner must still read their own conversation').toBe(200);
    const ownerBody = (await owner.json()) as { uuid?: string };
    expect(ownerBody.uuid).toBe(conversation.uuid);
  });

  /*
   * onetest: ELITEA-0606 — the listing answers the CALLER's conversations and
   * no others, and answers nothing at all without a credential.
   */
  test('SUP-S3: each user lists only their own support conversations, and an anonymous caller lists none', async ({
    page,
  }) => {
    test.setTimeout(120_000);
    expect(member, 'the member persona context must be open').toBeDefined();
    const other = member as APIRequestContext;

    const mine = await createSupportConversation(page.request, `${AUTOTEST_PREFIX}mine_${RUN_ID}`);
    const theirs = await createSupportConversation(other, `${AUTOTEST_PREFIX}theirs_${RUN_ID}`);

    const myList = (await listSupportConversations(page.request)).map((entry) => entry.uuid);
    const theirList = (await listSupportConversations(other)).map((entry) => entry.uuid);

    expect(myList, 'the owner must see their own conversation').toContain(mine.uuid);
    expect(myList, "another user's conversation must not appear in this list").not.toContain(theirs.uuid);
    expect(theirList, 'the second user must see their own conversation').toContain(theirs.uuid);
    expect(theirList, "the first user's conversation must not appear in the second user's list").not.toContain(
      mine.uuid,
    );

    // An ANONYMOUS context — and `storageState` is passed EXPLICITLY EMPTY,
    // which is the whole reason this works.
    //
    // MEASURED, on the first run of this test: `apiRequest.newContext({
    // baseURL })` inherits the API-shaped options from the config's `use`,
    // and this project's `use.storageState` is the ADMIN persona. The
    // "anonymous" caller was therefore signed in as an administrator, the
    // listing answered 200, and the assertion below failed for the one
    // reason that is not a defect. A probe that silently carries the very
    // credential it is supposed to lack is the same class of trap this suite
    // keeps recording elsewhere; the empty state is what makes it a probe.
    const anonymous = await apiRequest.newContext({
      baseURL: BASE_URL,
      storageState: { cookies: [], origins: [] },
    });
    try {
      // The control: this context really has no identity at all. Without it a
      // 401 below could equally mean "the route moved", and the case is about
      // WHO is refused.
      const identity = await anonymous.get(`${API_BASE}/social/author`);
      expect(identity.status(), 'the probe context must carry no session at all').toBe(401);

      const refused = await anonymous.get(`${API_BASE}/support_assistant/conversations?limit=100`);
      expect(
        refused.status(),
        'an unauthenticated listing must be refused, not answered with somebody’s history',
      ).toBe(401);
      expect((await refused.text()).includes(mine.uuid)).toBe(false);
    } finally {
      await anonymous.dispose();
    }
  });

  /*
   * onetest: ELITEA-0613 — the HISTORY a user is shown is their own. The
   * source case drives the widget's history panel as two users in turn; the
   * panel renders exactly what `GET /support_assistant/conversations` answers
   * (`ChatHeader.tsx` maps that list straight into
   * `.elitea-assistant-history-item`), so this drives the panel for one user
   * and reads the other user's list from the API rather than signing a second
   * browser session in and out — which would end the shared storage state
   * every other journey in this run depends on.
   */
  test('SUP-S4: the widget history panel lists the signed-in user’s sessions and not another user’s', async ({
    page,
  }) => {
    test.setTimeout(180_000);
    expect(member, 'the member persona context must be open').toBeDefined();
    const other = member as APIRequestContext;

    const mineName = `${AUTOTEST_PREFIX}history_mine_${RUN_ID}`;
    const theirsName = `${AUTOTEST_PREFIX}history_theirs_${RUN_ID}`;
    await createSupportConversation(page.request, mineName);
    const theirs = await createSupportConversation(other, theirsName);

    await page.goto(`${BASE_URL}/app/`, { waitUntil: 'domcontentloaded' });
    const launcher = page.locator('.elitea-assistant-button');
    await expect(launcher, 'the launcher must render while the assistant is on').toBeVisible({
      timeout: 30_000,
    });
    await launcher.click();

    const chatWindow = page.locator('.elitea-assistant-window');
    await expect(chatWindow).toBeVisible({ timeout: 15_000 });

    const historyButton = chatWindow.getByRole('button', { name: 'Chat history' });
    await expect(historyButton, 'the history control must be enabled once the user has sessions').toBeEnabled({
      timeout: 20_000,
    });
    await historyButton.click();

    const items = chatWindow.locator('.elitea-assistant-history-item');
    await expect(items.first(), 'the history panel must list the caller’s own sessions').toBeVisible({
      timeout: 15_000,
    });
    const listed = await items.allTextContents();
    expect(listed.join('\n'), 'the signed-in user’s own session must be listed').toContain(mineName);
    expect(
      listed.join('\n'),
      'another user’s session appeared in this user’s history panel',
    ).not.toContain(theirsName);

    // The other user still has it — so the absence above is scoping, not a
    // conversation that failed to be created.
    expect((await listSupportConversations(other)).map((entry) => entry.uuid)).toContain(theirs.uuid);
  });
});
