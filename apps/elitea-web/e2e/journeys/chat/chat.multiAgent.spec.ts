/**
 * A chat with three agents (ADR-0050 decisions 7c to 7f, issue 1612): the
 * composer addresses two of them with "@" and "#", the transcript folds the
 * agents' side talk into one chip and shows a platform line centred, the
 * participants panel makes an agent the lead, and a refused address reads as
 * a sentence.
 *
 * ── WHAT IS REAL AND WHAT IS SERVED ─────────────────────────────────────────
 *
 * REAL: the conversation, its three agents (created and attached over the
 * API, so the participants panel resolves them), the conversation PUT the
 * lead writes, and every read the page makes besides the two below.
 *
 * SERVED: the message list, because `author_kind`, `visibility` and `cause`
 * are written by the turn engine and no public write can produce them (the
 * reason `chat-message-classes.visual.spec.ts` serves them too); and the
 * start POST, because this lane mounts no runtime plane (see
 * `chat.mentions.spec.ts`). The start POST is captured: its body is the
 * assertion for the address.
 *
 * Keystrokes go through `pressSequentially`: `.fill()` never runs the
 * mention detection (`chat.mentions.spec.ts`).
 */
import { test, expect, type Page, type Route } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  addConversationParticipant,
  createAgent,
  createConversation,
  deleteAgent,
  deleteConversation,
  readConversationDetails,
} from '../../fixtures/api';
import { openParticipants } from '../../fixtures/sidePanel';

const SUFFIX = '-multi';
const SYSTEM_LINE = 'The agents reached their turn limit for this message.';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}${Date.now()}${SUFFIX}`;
}

interface Agent {
  readonly appId: string;
  readonly name: string;
  readonly participantId: number;
}

interface Fixture {
  readonly conversationId: string;
  readonly agents: readonly [Agent, Agent, Agent];
  readonly appIds: readonly string[];
}

/** A conversation with three real agents attached, read back for their participant ids. */
async function seed(page: Page): Promise<Fixture> {
  const names = [uniqueName('mira'), uniqueName('doctor'), uniqueName('scribe')];
  const created = await Promise.all(names.map((name) => createAgent(page.request, name)));
  const conversationId = await createConversation(page.request, uniqueName('conv'));
  for (const agent of created) {
    await addConversationParticipant(page.request, conversationId, { entity_name: 'application', entity_meta: { id: Number(agent.id) } });
  }
  const { participants } = await readConversationDetails(page.request, conversationId);
  const agents = created.map((agent, index) => {
    const row = participants.find((p) => p.entity_name === 'application' && String(p.entity_meta?.['id']) === agent.id);
    expect(row, `agent ${names[index] ?? ''} must be attached`).toBeDefined();
    return { appId: agent.id, name: names[index] ?? '', participantId: Number(row?.id) };
  });
  return { conversationId, agents: agents as unknown as Fixture['agents'], appIds: created.map((agent) => agent.id) };
}

async function cleanup(page: Page, fixture: Fixture | undefined): Promise<void> {
  if (fixture === undefined) return;
  await deleteConversation(page.request, fixture.conversationId);
  for (const id of fixture.appIds) await deleteAgent(page.request, id);
}

/** A question, an answer, two rows of side talk between the agents, a loop-guard line and a last answer. */
function transcript(conversationId: string, [mira, doctor, scribe]: Fixture['agents']) {
  const created = (n: number) => new Date(Date.now() - 60 * 60_000 + n * 1000).toISOString();
  const row = (n: number, fields: Record<string, unknown>) => ({
    id: String(9100 + n),
    uid: `00000000-0000-4000-8000-0000000091${String(n).padStart(2, '0')}`,
    conversation_id: conversationId,
    created_at: created(n),
    ...fields,
  });
  const agentRow = (n: number, author: Agent, content: string, extra: Record<string, unknown> = {}) =>
    row(n, { role: 'assistant', content, author_participant_id: author.participantId, author_kind: 'agent', reply_to_id: '9101', question_id: '9101', ...extra });
  const items = [
    row(1, { role: 'user', content: 'Is the release ready?', author_kind: 'human' }),
    agentRow(2, mira, 'Checking with the others.'),
    agentRow(3, mira, 'Is e2e green on main?', { addressed_to: [doctor.participantId], visibility: 'side', cause: 'agent_mention' }),
    agentRow(4, doctor, 'Green since this morning.', { addressed_to: [mira.participantId, scribe.participantId], visibility: 'side', cause: 'agent_mention' }),
    row(5, { role: 'assistant', content: SYSTEM_LINE, author_kind: 'system', cause: 'system', reply_to_id: '9101', question_id: '9101' }),
    agentRow(6, scribe, 'Release notes are drafted.'),
  ];
  return { items, total: items.length, page: 0, page_size: 50, total_pages: 1 };
}

/** Serves the transcript and captures (then answers) every start POST. */
async function serve(page: Page, fixture: Fixture, answerStart: (route: Route) => Promise<void>): Promise<Record<string, unknown>[]> {
  const starts: Record<string, unknown>[] = [];
  const messages = new RegExp(`/elitea_core/messages/prompt_lib/${DEFAULT_PROJECT_ID}/[^/?#]+(?:[?#]|$)`);
  await page.route(messages, async (route) => {
    const request = route.request();
    if (request.method() === 'POST' && request.url().includes('execution_contract=')) {
      starts.push(request.postDataJSON() as Record<string, unknown>);
      return answerStart(route);
    }
    if (request.method() === 'GET' && request.url().includes(`/${fixture.conversationId}`)) {
      return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(transcript(fixture.conversationId, fixture.agents)) });
    }
    return route.fallback();
  });
  await page.route(/\/elitea_core\/message_traces\/prompt_lib\//, (route) =>
    route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ rows: [], total: 0 }) }));
  return starts;
}

async function openChat(page: Page, conversationId: string): Promise<void> {
  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 20_000 });
  await expect(page.getByTestId('chat-system-line')).toBeVisible({ timeout: 20_000 });
}

/* ADR-0050 7c-7f: address two agents, fold side talk, show a system line, set the lead */
test('three-agent chat: address two agents, side talk folds, system line, lead', async ({ page }) => {
  let fixture: Fixture | undefined;
  try {
    fixture = await seed(page);
    const [mira, doctor, scribe] = fixture.agents;
    // An accepted start with no stream: the composer falls back and fails visibly, which this test does not read.
    const starts = await serve(page, fixture, (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '{}' }));
    await openChat(page, fixture.conversationId);

    // The platform line: centred, no avatar.
    const line = page.getByTestId('chat-system-line');
    await expect(line).toHaveText(SYSTEM_LINE);
    await expect(line.getByTestId('chat-message-avatar')).toHaveCount(0);

    // Side talk: one collapsed chip, expanding into "author → addressees" rows.
    const chip = page.getByTestId('chat-side-talk-chip');
    await expect(chip).toHaveText(/Side conversation \(2\)/);
    await expect(page.getByText('Is e2e green on main?')).toHaveCount(0);
    await chip.click();
    await expect(page.getByTestId('chat-side-talk-label')).toHaveText([
      `${mira.name} → ${doctor.name}`,
      `${doctor.name} → ${mira.name}, ${scribe.name}`,
    ]);

    // "To:" names who answers an unaddressed message: the newest addressed agent or a pick.
    await expect(page.getByTestId('chat-floor-target')).toBeVisible();

    // "@" offers the conversation's agents; a pick inserts the mention.
    const input = page.getByTestId('chat-message-input');
    await input.click();
    await input.pressSequentially('@', { delay: 15 });
    const atList = page.getByTestId('chat-user-mention-list');
    await expect(atList).toBeVisible({ timeout: 10_000 });
    await atList.getByText(mira.name, { exact: true }).click();
    await expect(input).toHaveValue(`@${mira.name} `);

    // "#" lists the conversation's agents first; a pick inserts "#Name".
    await input.pressSequentially(`and #${doctor.name.slice(0, 12)}`, { delay: 15 });
    const hashList = page.getByTestId('chat-agent-mention-list');
    await expect(hashList).toBeVisible({ timeout: 10_000 });
    await hashList.getByText(doctor.name, { exact: true }).click();
    await input.pressSequentially('is the release ready?', { delay: 10 });
    await expect(input).toHaveValue(`@${mira.name} and #${doctor.name} is the release ready?`);

    await page.getByTestId('chat-send-button').click();
    await expect.poll(() => starts.length, { timeout: 15_000 }).toBeGreaterThan(0);
    expect(starts[0]?.['addressed_to'], 'the start body addresses both agents, in pick order').toEqual([mira.participantId, doctor.participantId]);
    expect(starts[0]?.['participant_id'], 'the turn goes to the first addressee').toBe(mira.participantId);

    // The lead: the panel's row menu writes `meta.floor`, the row shows the badge, "To:" names the lead.
    await openParticipants(page);
    const scribeLead = page.locator(`[data-testid="participant-lead"][data-participant-id="${scribe.participantId}"]`);
    await expect(scribeLead).toBeVisible({ timeout: 20_000 });
    const saved = page.waitForResponse((response) =>
      response.request().method() === 'PUT' && response.url().includes(`/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${fixture?.conversationId ?? ''}`));
    await scribeLead.getByTestId('participant-lead-menu').click();
    await page.getByTestId('participant-lead-make').click();
    expect((await saved).ok()).toBe(true);
    await expect(scribeLead.getByTestId('participant-lead-badge')).toHaveText('Lead');
    await expect(page.getByTestId('chat-floor-target')).toHaveText(`To: ${scribe.name} (lead)`, { timeout: 15_000 });

    const stored = await page.request.get(`${API_BASE}/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}/${fixture.conversationId}`);
    const meta = ((await stored.json()) as { meta?: { floor?: unknown } }).meta;
    expect(meta?.floor, 'the lead is stored in the conversation meta').toEqual({ policy: 'lead', lead_participant_id: scribe.participantId });
  } finally {
    await cleanup(page, fixture);
  }
});

/* ADR-0050 7c: a refused address (422 addressee_not_in_conversation) reads as a sentence */
test('a message addressed to an agent that left reads as a readable error', async ({ page }) => {
  let fixture: Fixture | undefined;
  try {
    fixture = await seed(page);
    const [mira] = fixture.agents;
    await serve(page, fixture, (route) => route.fulfill({
      status: 422,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'addressee_not_in_conversation', message: 'An agent this message is addressed to is not in the conversation.' }),
    }));
    await openChat(page, fixture.conversationId);

    const input = page.getByTestId('chat-message-input');
    await input.click();
    await input.pressSequentially('@', { delay: 15 });
    await page.getByTestId('chat-user-mention-list').getByText(mira.name, { exact: true }).click();
    await input.pressSequentially('status?', { delay: 10 });
    await page.getByTestId('chat-send-button').click();

    await expect(page.getByText('One of the agents you addressed is no longer in this conversation.', { exact: false })).toBeVisible({ timeout: 15_000 });
  } finally {
    await cleanup(page, fixture);
  }
});
