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

    // Re-judged again (PR #957): the previous webkit hardening's theory
    // (stale label lingering) wasn't what CI hit. Both webkit AND chromium
    // failed with the button rendering EMPTY (icon-only), not agent A's
    // leftover name — `useAgentEditorPanelFit.hooks.ts` measures the
    // composer's controls container against a fixed 430px breakpoint
    // (`CHAT_CONTROLS_WIDTH_THRESHOLD`) and renders icon-only
    // (`VersionSelector`'s `isSmallView` branch) below it. At this journey's
    // default 1280px viewport, expanding the participants rail narrows that
    // container to ~350px — reliably below the breakpoint once a render
    // happens slowly enough (CI's shared runners, or any injected network
    // latency) for `useAgentEditorPanelFit`'s hook to catch the narrowed
    // width; a fast local run can race past it and observe the transient
    // wide-view render instead, which is why this looked like webkit-only
    // flakiness rather than a reliable failure. A locator-name assertion
    // will never resolve in icon-only mode no matter how long it waits —
    // this is a viewport/breakpoint mismatch, not a timing race to wait out.
    // Widening the viewport keeps the composer's measured container clear of
    // the breakpoint regardless of participants-rail state or render timing,
    // so the version NAME assertions below test version identity, not which
    // responsive mode happened to win a race. Proven against 3s of injected
    // latency on the application-details fetch, on both webkit and chromium
    // (`page.route('**/elitea_core/application/prompt_lib/**', ...)`).
    await page.setViewportSize({ width: 1920, height: 1080 });
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

    // `VersionSelector`'s button renders bare `selectedVersion?.name` with no
    // loading state, so right after this click it can still show agent A's
    // label for a poll or two before the `useActiveParticipantDetails`
    // re-fetch (keyed to agent B's own ids) resolves. Demanding the stale
    // text is GONE first is a positive wait on that settled-state
    // transition, kept independent of the viewport fix above (which
    // addresses a different failure mode — icon-only rendering — not this
    // one).
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
