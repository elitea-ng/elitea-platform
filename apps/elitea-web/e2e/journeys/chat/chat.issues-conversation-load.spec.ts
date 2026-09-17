/**
 * Two DEFECT-CHECK issues about a conversation FAILING TO LOAD, re-judged
 * against this platform's Go handler and React page rather than the legacy
 * Pylon/EliteaUI stack both were originally filed against.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * elitea_issues #6523 — "Conversation becomes permanently inaccessible after
 * partial participant-settings update drops required field"
 * ─────────────────────────────────────────────────────────────────────────────
 * NOT REPRODUCED. The legacy report describes a participant-settings
 * enrichment step (version-details lookup, available-tools computation) that
 * throws when `version_id` is missing, caught several layers up and
 * surfaced as a misleading "No such conversation". This platform's
 * `Handler.Get` (`services/elitea-main/internal/api/v2/conversations/
 * handler.go`) does none of that enrichment inline: it reads the conversation
 * row, lists participants as stored (`h.repo.ListParticipants`, errors
 * swallowed to an empty slice, never propagated as "not found"), and returns
 * them verbatim — there is no version-dependent lookup in this path that a
 * missing `entity_settings.version_id` could break.
 *
 * The suggested frontend trigger (`useChatBoxVersioning.ts`'s auto-recovery
 * effect firing `updateParticipantSettings` while version details are still
 * loading) is also not reproduced: `handleSelectVersion` only ever runs from
 * an effect gated on `activeParticipantVersions` already being a non-empty,
 * loaded list (`useChatBoxVersioning.ts:81-87`), and its payload's
 * `version_id` is read off the SELECTED version object, not off a
 * still-loading `versionDetails` value — there is no code path here that
 * sends a settings update with `version_id` absent.
 *
 * This journey pins the current, non-throwing behaviour at the layer the
 * issue actually broke: a participant whose `entity_settings` is missing
 * `version_id` (and every other version-dependent key) must not make the
 * conversation unreadable.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * elitea_issues #6387 — "First-Time Login Redirects to Team Project Chat Page
 * with No Chat Selected and Persistent Loading Icon"
 * ─────────────────────────────────────────────────────────────────────────────
 * NOT REPRODUCED. `src/pages/chat/index.tsx` only drives `isLoadingConversation`
 * off `useChatPageData`'s fetch for a route-level `conversationId` — arriving
 * at `/app/chat` with none present fetches nothing, so there is no in-flight
 * request for a spinner to hang on. A brand-new account (this suite has none)
 * cannot be simulated here; what this pins instead is the one precondition
 * the "persistent loading icon" bug needs and this app's routing does not
 * supply: landing on chat with no conversation selected settles immediately
 * to the composer, never to a spinner with nothing behind it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * elitea_issues #6621 — OUTDATED, no journey
 * ─────────────────────────────────────────────────────────────────────────────
 * Filed against `EliteaUI` (the legacy frontend), PR #887, commit `b4d00fcc`,
 * component `src/[fsd]/features/chat/participants/ui/ExpandedParticipants/
 * ParticipantNormalCard.jsx` — a file that has no counterpart in this
 * repository. Grepped: `chat-participant-row` (the collided testid the issue
 * names) does not appear anywhere under `apps/elitea-web/src` — this port
 * never used that naming convention. The one row that DOES key a testid off
 * a bare participant id, `ParticipantItemRow.tsx`'s
 * `participant-item-${participantId}`, is the USERS row (one participant
 * type, no cross-type collision possible); entity-type rows key off
 * `add-participant-option-${rowKey}` in the add-participants dialog, built
 * from a different composite already. See `S/issues/ledger-I2.tsv`.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { AUTOTEST_PREFIX, addConversationParticipant, createAgent, createConversation, deleteAgent, deleteConversation, readConversationDetails } from '../../fixtures/api';

const SUFFIX = '-convload';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}${SUFFIX}`;
}

test.describe('conversation load survives an incomplete participant (elitea_issues #6523)', () => {
  test('GET returns the conversation and its participants when one participant carries no version_id in entity_settings', async ({ page }) => {
    test.setTimeout(60_000);
    const agent = await createAgent(page.request, uniqueName('agent'));
    const conversationId = await createConversation(page.request, uniqueName('conv'));
    try {
      // The exact shape the issue names: an `application` participant whose
      // `entity_settings` carries fields OTHER than `version_id` — no version
      // identity at all, the state a dropped-field partial update would leave.
      await addConversationParticipant(page.request, conversationId, {
        entity_name: 'application',
        entity_meta: { id: agent.id },
        entity_settings: { llm_settings: {} },
      });

      const details = await readConversationDetails(page.request, conversationId);
      expect(details.participants.length, 'the incomplete participant must still be listed, not silently dropped').toBeGreaterThan(0);
      const incomplete = details.participants.find((p) => p.entity_name === 'application');
      expect(incomplete, 'the application participant must be present despite its missing version_id').toBeDefined();

      // The UI path: opening the conversation must reach the composer, not
      // the "No such conversation" error state the issue reports.
      await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
      await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
      await expect(page.getByText(/no such conversation/i)).toHaveCount(0);
    } finally {
      await deleteConversation(page.request, conversationId);
      await deleteAgent(page.request, agent.id);
    }
  });
});

test.describe('landing on chat with nothing selected settles, never spins forever (elitea_issues #6387)', () => {
  test('the composer is reachable immediately, with no error and no stuck loading indicator', async ({ page }) => {
    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
    // A stuck spinner is a `CircularProgress`/`progressbar` that never clears;
    // by the time the composer is visible, none should remain in the page.
    await expect(page.getByRole('progressbar')).toHaveCount(0);
  });
});
