/**
 * Issues programme, package BK1-chat-agents — three CLOSED legacy issues
 * about state that must survive an action taken WHILE composing, or across a
 * reload, judged against the current app (no message-persistence capability
 * in this stack — see `chat.management.spec.ts`'s own header — so all three
 * are asserted without any model turn).
 *
 *  - #2449 — opening a participant's editor (Canvas) while a new conversation
 *    is being composed used to wipe the composer's draft text.
 *  - #2436 — adding a participant via the picker while composing used to wipe
 *    the draft the same way.
 *  - #4697 — selecting a non-default participant, then refreshing the page,
 *    used to revert the conversation to the default LLM instead of keeping
 *    the selected participant.
 *
 * All three are read from CODE as well as asserted here: `useLocalActiveParticipant`
 * (`features/chat-participants/hooks/chat/useLocalActiveParticipant.ts`)
 * persists the active participant per-conversation in `localStorage` under
 * the SAME key the old app used, and `pages/chat/index.tsx` reads it back on
 * mount — so #4697 is expected to already hold. Composer state
 * (`UserInput`/`useNewChatInputController`) is local to the input feature and
 * is never reset by the participants rail or the editor column mounting —
 * so #2449/#2436 are expected to already hold too. These are REGRESSION PINS,
 * not repro attempts at a live bug.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  addConversationParticipant,
  createAgentWithVersion,
  createConversation,
  deleteAgent,
  deleteConversation,
  readConversationDetails,
} from '../../fixtures/api';

const SUFFIX = '-composer-state';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now() % 1_000_000}${SUFFIX}`;
}

async function openParticipantsRail(page: Page): Promise<void> {
  const expand = page.getByRole('button', { name: 'Expand participants' });
  if ((await expand.count()) > 0) await expand.click();
  await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 15_000 });
}

/** The signed-in persona, read from the same endpoint the app itself reads it from. */
async function readAuthor(request: APIRequestContext): Promise<{ readonly id: string; readonly name: string }> {
  const response = await request.get(`${API_BASE}/social/author`);
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { id?: string; name?: string };
  return { id: body.id as string, name: body.name as string };
}

/** Some OTHER seeded user, the same way `chat.management.spec.ts`'s M2c/M3 resolve it. */
async function readOtherUser(
  request: APIRequestContext,
  selfId: string,
): Promise<{ readonly id: string; readonly name: string }> {
  const response = await request.get(
    `${API_BASE}/admin/users/default/${DEFAULT_PROJECT_ID}?limit=200&offset=0`,
  );
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { rows?: readonly { id?: string; name?: string }[] };
  const other = (body.rows ?? []).find((row) => row.id !== undefined && row.id !== selfId && row.name);
  expect(other, 'the seed must provision a SECOND user').toBeDefined();
  return { id: String(other?.id), name: String(other?.name) };
}

// ─────────────────────────────────────────────────────────────────────────────
// legacy: issue #2449 — opening Canvas for a participant must not clear the draft
// ─────────────────────────────────────────────────────────────────────────────
/* elitea_issues: #2449 — a composer draft typed while starting a new conversation
 * survives opening a participant's editor (Canvas) panel. */
test('L1: the composer draft survives opening a participant editor', async ({ page }) => {
  test.setTimeout(60_000);
  const agentName = uniqueName('agent');
  const agent = await createAgentWithVersion(page.request, agentName, { instructions: 'Be helpful.' });
  const conversationId = await createConversation(page.request, uniqueName('conv'));
  try {
    await addConversationParticipant(page.request, conversationId, {
      entity_name: 'application',
      entity_meta: { id: agent.id },
    });

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });

    const draft = `${AUTOTEST_PREFIX}draft-must-survive-canvas`;
    await input.fill(draft);
    await expect(input).toHaveValue(draft);

    await openParticipantsRail(page);
    const section = page.getByTestId('participants-section-Agents');
    await expect(section).toBeVisible({ timeout: 20_000 });
    await section.hover();
    // The accessible name is "Edit agent Participant" (participant display-name
    // resolution is a separate, already-tracked gap — not what this test is
    // about), so match on the stable "Edit agent" prefix instead of the name.
    const edit = page.getByRole('button', { name: /^Edit agent/ });
    await expect(edit).toBeVisible({ timeout: 15_000 });
    await edit.click();
    await expect(page.getByTestId('chat-editor-panel')).toBeVisible({ timeout: 15_000 });

    // The composer draft must still be there — the panel opening is a SIBLING
    // mount, not a remount of the input's own subtree.
    await expect(input).toHaveValue(draft);
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agent.id);
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// legacy: issue #2436 — adding a participant via the picker must not clear the draft
// ─────────────────────────────────────────────────────────────────────────────
/* elitea_issues: #2436 — a composer draft typed while starting a new conversation
 * survives adding a participant through the "Add participants" picker. */
test('L2: the composer draft survives adding a participant via the picker', async ({ page }) => {
  test.setTimeout(60_000);
  const reader = await readAuthor(page.request);
  const teammate = await readOtherUser(page.request, reader.id);
  const conversationId = await createConversation(page.request, uniqueName('conv'));
  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });

    const draft = `${AUTOTEST_PREFIX}draft-must-survive-add-participant`;
    await input.fill(draft);
    await expect(input).toHaveValue(draft);

    await openParticipantsRail(page);
    const add = page.getByTestId('participants-add-button');
    await expect(add).toBeVisible({ timeout: 15_000 });
    await add.click();

    const picker = page.getByTestId('add-participants-dialog');
    await expect(picker).toBeVisible({ timeout: 10_000 });
    await picker.getByTestId('add-participants-search').fill(teammate.name);
    const row = picker.getByTestId(`add-participant-option-${teammate.id}`);
    await expect(row).toBeVisible({ timeout: 15_000 });
    await row.click();
    const confirm = picker.getByTestId('add-participants-confirm');
    await expect(confirm).toBeEnabled();
    await confirm.click();
    await expect(picker).toBeHidden({ timeout: 10_000 });

    // The composer draft must still be there after the picker's round trip.
    await expect(input).toHaveValue(draft);
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// legacy: issue #4697 — the selected participant must survive a reload
// ─────────────────────────────────────────────────────────────────────────────
/* elitea_issues: #4697 — selecting a non-default agent participant, then
 * reloading the page, keeps that agent selected instead of reverting to the
 * default LLM. Pins `useLocalActiveParticipant`'s per-conversation localStorage
 * persistence (read back by `pages/chat/index.tsx` on mount). */
test('L3: the selected agent participant survives a page reload', async ({ page }) => {
  test.setTimeout(60_000);
  const agentName = uniqueName('agent');
  const agent = await createAgentWithVersion(page.request, agentName, { instructions: 'Be helpful.' });
  const conversationId = await createConversation(page.request, uniqueName('conv'));
  try {
    await addConversationParticipant(page.request, conversationId, {
      entity_name: 'application',
      entity_meta: { id: agent.id },
    });
    const { participants } = await readConversationDetails(page.request, conversationId);
    const participantId = participants[0]?.id;
    expect(participantId, 'the participant must have attached before selection can be tested').toBeDefined();

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
    await openParticipantsRail(page);

    const section = page.getByTestId('participants-section-Agents');
    await expect(section).toBeVisible({ timeout: 20_000 });
    // The avatar initial is the only stable, name-resolution-independent
    // target for the single participant row this test sets up.
    await section.getByText('E', { exact: true }).click();

    async function isRecordedActive(): Promise<boolean> {
      return page.evaluate(
        ([cid, pid]) => {
          const raw = window.localStorage.getItem('ActiveConversationParticipantKey');
          if (!raw) return false;
          const map = JSON.parse(raw) as Record<string, { cid: string; pid: string | number }[]>;
          return Object.values(map).some((list) => list.some((e) => e.cid === cid && String(e.pid) === String(pid)));
        },
        [conversationId, participantId] as const,
      );
    }

    // Selecting sets `ActiveConversationParticipantKey` in localStorage keyed
    // by this conversation's id — assert it landed before reloading, so a
    // failure here is legible as "selection never persisted" rather than
    // conflated with "the reload lost it".
    await expect.poll(isRecordedActive, { timeout: 15_000 }).toBe(true);

    await page.reload();
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
    await openParticipantsRail(page);

    // The agent row (not "default LLM") must still read as the active one —
    // asserted through the same localStorage read, which is what the page's
    // mount-time restore depends on (`pages/chat/index.tsx`'s
    // `getLocalActiveParticipant` call).
    expect(
      await isRecordedActive(),
      'the selected agent participant must still be recorded as active after reload',
    ).toBe(true);
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agent.id);
  }
});
