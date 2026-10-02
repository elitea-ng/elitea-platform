/**
 * Issues programme, package BK1-chat-agents — #4826 (CLOSED legacy issue):
 * "Responses from other users in a public chat leak into a newly created
 * private conversation with the same name". The legacy report's own root
 * cause claim: creating a new conversation bound client state by NAME instead
 * of by id, so a same-named public conversation's content bled into it.
 *
 * `pages/chat/index.tsx`'s own module header (read for this package) says
 * the current app reads the `conversationId` ROUTE PARAM itself — an opaque
 * server-assigned id, never the display name — and `conversations.create.spec.ts`
 * already proves the "Create" control forces a full subtree REMOUNT (a fresh
 * key), independent of any name. Binding-by-name has no code path here to
 * reproduce against.
 *
 * This pins the falsifiable half without a model turn (this stack persists no
 * message — see `chat.management.spec.ts`'s header): two conversations that
 * share a display name never share participants or any other server-visible
 * state — each is addressed strictly by its own id.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  AUTOTEST_PREFIX,
  addConversationParticipant,
  createAgentWithVersion,
  createConversation,
  deleteAgent,
  deleteConversation,
  readConversationDetails,
  readCallerIdentity,
  readStoredMessageGroups,
  setConversationPrivacy,
  PUBLISH_AUTHOR_PROJECT_NAME,
  resolvePublishAuthorProjectId,
} from '../../fixtures/api';
import { ensureProjectSelected } from '../../fixtures/project';

const SUFFIX = '-conv-isolation';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now() % 1_000_000}${SUFFIX}`;
}

/* elitea_issues: #4826 — a new (private) conversation sharing the display name of
 * an existing PUBLIC one inherits no agent participant or conversation state,
 * bled in from the other conversation of the same name. */
test('L-conv-isolation: two conversations with the identical name stay isolated by id', async ({ page }) => {
  test.setTimeout(60_000);
  const projectId = await resolvePublishAuthorProjectId(page.request);
  const sharedName = uniqueName('game');
  const agentName = uniqueName('agent');
  const agent = await createAgentWithVersion(page.request, agentName, { instructions: 'Be helpful.' }, projectId);

  // "User A" conversation: public, with a participant attached — the content
  // that must NOT leak.
  const original = await createConversation(page.request, sharedName, projectId);
  // "User A" creates a second, private conversation with the SAME name —
  // the exact repro shape (same name, no relation, created after the first).
  const fresh = await createConversation(page.request, sharedName, projectId);
  const initialFresh = await readConversationDetails(page.request, fresh, projectId);
  try {
    await addConversationParticipant(page.request, original, {
      entity_name: 'application',
      entity_meta: { id: agent.id, project_id: Number(projectId) },
    }, projectId);
    await setConversationPrivacy(page.request, original, false, projectId);

    expect(original).not.toBe(fresh);
    const originalDetails = await readConversationDetails(page.request, original, projectId);
    expect(originalDetails.participants.map((p) => p.entity_name)).toContain('application');

    // The FRESH, same-named conversation must read back with NOTHING
    // inherited from `original` — server truth first, then the UI.
    const freshDetails = await readConversationDetails(page.request, fresh, projectId);
    expect(freshDetails.participants, 'the original must not change the fresh conversation participants').toEqual(initialFresh.participants);
    expect(freshDetails.participants.map((participant) => participant.entity_name).sort()).toEqual(['dummy', 'user']);
    const caller = await readCallerIdentity(page.request);
    expect(freshDetails.participants.find((participant) => participant.entity_name === 'user')?.entity_meta?.['id']).toBe(Number(caller.id));
    expect(freshDetails.participants.find((participant) => participant.entity_name === 'dummy')?.entity_meta).toEqual({});
    expect(freshDetails.is_private).toBe(true);
    expect(await readStoredMessageGroups(page, projectId, fresh, 10)).toEqual([]);

    await page.goto(`${BASE_URL}/app/chat`);
    await ensureProjectSelected(page, PUBLISH_AUTHOR_PROJECT_NAME);
    await page.goto(`${BASE_URL}/app/chat/${fresh}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    const expand = page.getByRole('button', { name: 'Expand participants' });
    if ((await expand.count()) > 0) await expand.click();
    // No "Agents" participant section at all renders here — `original`'s
    // attached agent participant never bleeds into this same-named conversation.
    await expect(page.getByTestId('participants-section-Agents')).toHaveCount(0);
  } finally {
    await deleteConversation(page.request, original, projectId);
    await deleteConversation(page.request, fresh, projectId);
    await deleteAgent(page.request, agent.id, projectId);
  }
});
