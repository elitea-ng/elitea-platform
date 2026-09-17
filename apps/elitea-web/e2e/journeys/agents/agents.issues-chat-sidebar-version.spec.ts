/**
 * elitea_issues package I1-agents — #6543 "[BUG] Frontend Fetches Mismatched
 * Agent Version ID When Switching Between Agents in Chat Right Sidebar".
 *
 * Re-judged: DEFECT-CHECK, NOT reproduced. `useActiveParticipantDetails`
 * (`src/features/chat-participants/lib/hooks/useActiveParticipantDetails.ts`)
 * re-fetches keyed by the ACTIVE participant's OWN
 * `entity_meta.id`/`project_id`/`entity_settings.version_id` — there is no
 * shared "currently selected version" variable for it to leak through, so
 * switching the active participant re-derives from the newly active
 * object's own ids on the same render. This journey pins that: two agents,
 * each with its own distinctly-named version, attached to one conversation
 * — clicking between them in the participants rail must show EACH agent's
 * OWN version, never the other one's leftover.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  addConversationParticipant,
  createAgentWithVersion,
  createConversation,
  deleteAgent,
  deleteConversation,
} from '../../fixtures/api';

const SUFFIX = '-sidebarver';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
}

/* onetest: elitea_issues #6543 — switching agents in the chat sidebar re-fetches EACH agent's own version, never the previous agent's */
test('J-sidebar-version: switching between two chat agents shows each one\'s own version, not the other\'s', async ({
  page,
  request,
}) => {
  const agentAName = uniqueName('agent-a');
  const agentBName = uniqueName('agent-b');
  const agentA = await createAgentWithVersion(request, agentAName, { name: 'alpha-version' });
  const agentB = await createAgentWithVersion(request, agentBName, { name: 'beta-version' });
  const conversationId = await createConversation(request, uniqueName('sidebarverconv'));

  try {
    await addConversationParticipant(request, conversationId, {
      entity_name: 'application',
      entity_meta: { id: agentA.id, project_id: DEFAULT_PROJECT_ID, name: agentAName },
      entity_settings: { version_id: agentA.versionId },
    });
    await addConversationParticipant(request, conversationId, {
      entity_name: 'application',
      entity_meta: { id: agentB.id, project_id: DEFAULT_PROJECT_ID, name: agentBName },
      entity_settings: { version_id: agentB.versionId },
    });

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    const expandParticipants = page.getByRole('button', { name: 'Expand participants' });
    await expect(expandParticipants).toBeVisible({ timeout: 15_000 });
    await expandParticipants.click();
    const agentsSection = page.getByTestId('participants-section-Agents');
    await expect(agentsSection).toBeVisible({ timeout: 15_000 });

    const versionButton = page.getByRole('button', { name: 'version selector menu' });

    await agentsSection.getByText(agentAName, { exact: true }).click();
    await expect(versionButton, 'agent A must show its OWN version').toHaveText('alpha-version', { timeout: 15_000 });

    // Flaky on webkit (PR #957, passed on retry): `VersionSelector`'s button
    // renders bare `selectedVersion?.name` with no loading state at all, so
    // right after this click it can still show agent A's label (the
    // `useActiveParticipantDetails` re-fetch keyed to agent B's own ids
    // hasn't resolved yet) for longer on a slow webkit run than on chromium.
    // Asserting straight to 'beta-version' let `toHaveText`'s poll catch a
    // window where the button still read the STALE 'alpha-version' one poll
    // before the real value landed, close enough to the assertion's start
    // that a slow webkit paint occasionally pushed the whole thing past
    // its 15s budget. Demanding the stale text is GONE first is a positive
    // wait on that settled-state transition, not a race against the same
    // clock the flaky assertion was already racing.
    await agentsSection.getByText(agentBName, { exact: true }).click();
    await expect(versionButton, 'must leave agent A\'s version behind before agent B\'s resolves').not.toHaveText(
      'alpha-version',
      { timeout: 15_000 },
    );
    await expect(
      versionButton,
      'agent B must show its OWN version, not agent A\'s leftover — the #6543 regression',
    ).toHaveText('beta-version', { timeout: 15_000 });

    // Round-trip back to A: the fetch must re-key off A's ids again, not get
    // stuck on whatever B last resolved to. Same settled-state wait first.
    await agentsSection.getByText(agentAName, { exact: true }).click();
    await expect(versionButton, 'must leave agent B\'s version behind before agent A\'s resolves again').not.toHaveText(
      'beta-version',
      { timeout: 15_000 },
    );
    await expect(versionButton, 'switching back to A must not carry over B\'s version').toHaveText('alpha-version', {
      timeout: 15_000,
    });
  } finally {
    await deleteConversation(request, conversationId);
    await deleteAgent(request, agentA.id);
    await deleteAgent(request, agentB.id);
  }
});
