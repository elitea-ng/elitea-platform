/**
 * A CONVERSATION WHOSE AGENT WAS UNPUBLISHED (#939 group 10).
 *
 * `agents.publishing.spec.ts` proves what a withdrawal does to the CATALOGUE
 * and to the author's own listing. It says nothing about the person on the
 * other side: someone who already has a conversation with that agent open.
 *
 * The source case (ELITEA-0199) states the contract in two halves:
 *
 *  1. the conversation is PRESERVED — it still opens, it does not 404, and
 *     what it holds is intact;
 *  2. it is DISABLED — a notice says the agent is no longer available, and the
 *     composer cannot be used.
 *
 * Half 1 holds. Half 2 is still marked — see the note on that test.
 *
 * WHAT THIS STACK CANNOT SAY, stated rather than quietly dropped: the case's
 * "all prior messages are visible and content is intact" needs a transcript,
 * and a transcript needs a model turn this stack composes no worker for. What
 * is asserted instead is the conversation ROW and its participant — the thing
 * the messages would hang off, and the thing a withdrawal could plausibly
 * cascade into deleting.
 */
import { expect, test } from '@playwright/test';

import type { APIRequestContext, Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  createConversation,
  deleteConversation,
  DEFAULT_PROJECT_ID,
  deleteAgent,
  PUBLISHABLE_TAGS,
  readConversationDetails,
  unpublishAllVersions,
} from '../../fixtures/api';

interface PublishedAgent {
  readonly id: string;
  /** The author's own draft version. */
  readonly versionId: string;
  /** The PUBLISHED clone — the version a consumer's conversation is bound to. */
  readonly publicVersionId: string;
  readonly name: string;
}

/** Publish an agent and return both version ids: the draft and the public clone. */
async function publishAgent(request: APIRequestContext, name: string): Promise<PublishedAgent> {
  const created = await request.post(`${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: {
      name,
      description: `${name} answers questions about a release.`,
      versions: [
        {
          name: 'base',
          agent_type: 'openai',
          instructions: 'You answer questions about a release from the notes you are given.',
          welcome_message: 'Ask me about the release.',
          conversation_starters: ['What changed?'],
          tags: [...PUBLISHABLE_TAGS],
          meta: { category: 'Development' },
        },
      ],
    },
  });
  expect(created.ok(), `the agent was not created: ${(await created.text()).slice(0, 300)}`).toBe(true);
  const body = (await created.json()) as { id?: unknown; version_details?: { id?: unknown } };
  const id = String(body.id ?? '');
  const versionId = String(body.version_details?.id ?? '');
  expect(id, 'the create must answer an id').not.toBe('');

  const published = await request.post(
    `${API_BASE}/elitea_core/publish/prompt_lib/${DEFAULT_PROJECT_ID}/${versionId}`,
    { data: { version_name: `rel${String(Date.now()).slice(-6)}` } },
  );
  expect(published.status(), `the publish was refused: ${(await published.text()).slice(0, 300)}`).toBe(200);
  const publicVersionId = String(((await published.json()) as { public_version_id?: unknown }).public_version_id ?? '');
  expect(publicVersionId, 'the publish must answer the public version id a consumer binds to').not.toBe('');
  return { id, versionId, publicVersionId, name };
}

/** Attach the PUBLISHED version of an agent to a conversation, the way a consumer's chat does. */
async function attachPublishedAgent(
  request: APIRequestContext,
  conversationId: string,
  agent: PublishedAgent,
): Promise<void> {
  const attached = await request.post(
    `${API_BASE}/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`,
    {
      data: [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID, name: agent.name },
          entity_settings: { version_id: agent.publicVersionId },
        },
      ],
    },
  );
  expect(attached.status(), `the agent participant must attach: ${(await attached.text()).slice(0, 300)}`).toBe(200);
}

/** Withdraw the published version — the author's "unpublish" action. */
async function unpublish(request: APIRequestContext, agent: PublishedAgent): Promise<void> {
  const withdrawn = await request.post(
    `${API_BASE}/elitea_core/unpublish/prompt_lib/${DEFAULT_PROJECT_ID}/${agent.publicVersionId}`,
    { data: {} },
  );
  expect(withdrawn.status(), `the unpublish was refused: ${(await withdrawn.text()).slice(0, 300)}`).toBe(200);
}

/** Set up "a conversation with a published agent, whose agent is then withdrawn". */
async function conversationWithWithdrawnAgent(
  page: Page,
): Promise<{ agent: PublishedAgent; conversationId: string }> {
  const name = `${AUTOTEST_PREFIX}unpub-${String(Date.now()).slice(-7)}`;
  const agent = await publishAgent(page.request, name);
  const conversationId = await createConversation(page.request, `${AUTOTEST_PREFIX}unpubconv-${String(Date.now()).slice(-7)}`);
  await attachPublishedAgent(page.request, conversationId, agent);
  await unpublish(page.request, agent);
  return { agent, conversationId };
}

async function cleanUp(request: APIRequestContext, agentId: string, conversationId: string): Promise<void> {
  await deleteConversation(request, conversationId).catch(() => undefined);
  await unpublishAllVersions(request, agentId).catch(() => undefined);
  await deleteAgent(request, agentId).catch(() => undefined);
}

/*
 * onetest: ELITEA-0199 (the PRESERVED half) — withdrawing a published agent
 * must not take the conversations that use it with it: the conversation still
 * opens, its row is intact, and the agent is still its participant.
 */
test('a conversation survives the withdrawal of the agent it uses', async ({ page }) => {
  test.setTimeout(120_000);
  const { agent, conversationId } = await conversationWithWithdrawnAgent(page);

  try {
    // The ROW first, through the API: a withdrawal that cascaded into deleting
    // the conversation would leave the page below showing an empty-state that
    // an unwary DOM check could read as "it opened fine".
    const details = await readConversationDetails(page.request, conversationId);
    expect(details.participants, 'the withdrawn agent is still the conversation’s participant').toHaveLength(1);
    expect(details.participants[0]?.entity_name).toBe('application');

    // And the page opens. `chat-message-input` is the composer; its mere
    // PRESENCE is the "no crash, no 404" assertion, not a statement about
    // whether it can be used — that is the next test's subject.
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(
      page.getByTestId('chat-message-input'),
      'the conversation must still open after its agent was withdrawn',
    ).toBeVisible({ timeout: 30_000 });
  } finally {
    await cleanUp(page.request, agent.id, conversationId);
  }
});

/*
 * onetest: ELITEA-0199 (the DISABLED half) — the conversation must tell its
 * holder that the agent is gone, and must not let them send into it.
 *
 * PARTLY FIXED (#972) — and the part that is missing is the SIGNAL, not the
 * rendering. `useChatBoxState` now derives `isActiveParticipantWithdrawn`
 * from the bound version's own state — a withdrawal REVERTS the published
 * clone to a draft and stamps `-withdrawn-` into its name, so both halves
 * together are specific to a version that WAS published and no longer is.
 * `ChatBoxWithdrawnNotice` renders the notice as a `role="alert"`, and
 * `deriveChatBoxInputState` folds the same flag into `disabledSend`.
 *
 * MEASURED after the change: the notice still does not appear in this flow.
 * The renderer and the composer gate are unit-tested and correct; what does
 * not fire is the flag, because the chat page either does not carry the
 * participant's VERSION LIST here or the withdrawn clone is not renamed with
 * the `-withdrawn-` marker on this path. A server-side `withdrawn_at` on the
 * version row — or the published state on the participant projection — would
 * settle it; reading a name substring was the cheapest signal available and
 * is evidently not available here.
 *
 * Before the change the conversation looked entirely live: no notice, an
 * editable composer, an enabled Send, and the only way to find out was to
 * send.
 */
test('a conversation whose agent was withdrawn says so and refuses new messages', async ({ page }) => {
  test.fail(
    true,
    '#972: the notice and the disabled composer are implemented (ChatBoxWithdrawnNotice + isActiveParticipantWithdrawn, unit-tested) but the flag does not yet fire in this flow — the chat page either does not carry the participant VERSION LIST here, or the withdrawn clone is not renamed with the `-withdrawn-` marker on this path. Needs the signal re-derived from data the chat page really has.',
  );
  test.setTimeout(120_000);
  const { agent, conversationId } = await conversationWithWithdrawnAgent(page);

  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    const composer = page.getByTestId('chat-message-input');
    await expect(composer).toBeVisible({ timeout: 30_000 });

    // The notice, by what it MEANS rather than by one exact sentence: any of
    // these words would tell the holder what happened.
    await expect(
      page.getByText(/no longer (published|available)|has been unpublished|unavailable/i).first(),
      'the conversation must say that its agent is no longer available',
    ).toBeVisible({ timeout: 15_000 });

    // And the composer must refuse input. `toBeEditable` is the product's own
    // readiness signal elsewhere in this suite, so its NEGATION is the honest
    // way to state "cannot be typed into".
    await expect(composer, 'the composer must be read-only once the agent is gone').not.toBeEditable({
      timeout: 15_000,
    });
  } finally {
    await cleanUp(page.request, agent.id, conversationId);
  }
});
