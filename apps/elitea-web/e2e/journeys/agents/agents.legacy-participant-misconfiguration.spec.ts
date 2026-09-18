/**
 * Issues programme, package BK1-chat-agents — a GENUINE product gap, not a
 * live bug in a working feature, covering three CLOSED legacy issues against
 * the SAME root cause:
 *
 *  - #4050 — parent agent shows no misconfiguration warning when a nested
 *    child agent/toolkit is broken.
 *  - #4391 — no warning when a toolkit is added without its required private
 *    credentials.
 *  - #2159 — "show correct notifications and highlight the errors" for a
 *    misconfigured credential/PgVector/Embedding configuration on a Toolkit,
 *    an Agent, or a Chat participant.
 *
 * ROOT CAUSE (code read, this pass): the Chat Participants panel's
 * misconfiguration flag (`hasMisconfigurationErrors`, rendered by
 * `ParticipantItem.cards.tsx` via `ParticipantWarning`) is driven by
 * `deriveParticipantFlags`'s `hasValidationIssue` parameter
 * (`ParticipantStatusRunner.helpers.ts:85`: `hasMisconfigurationErrors =
 * !!hasValidationIssue`). That parameter is only ever supplied by
 * `ParticipantStatusRunner`/`useParticipantStatus`
 * (`features/chat-participants/lib/context/ParticipantStatusRunner.tsx`) —
 * and NOTHING renders or calls that runner anywhere in the app. Grepped
 * `<ParticipantStatusRunner` and `useParticipantStatus(` across `src/`: the
 * only hits are the runner's own file, its unit tests, and doc-comment
 * MENTIONS of it in `ParticipantDetailsContext.tsx` ("NOTE:
 * ParticipantStatusRunner children are NOT rendered here ... consumers wire
 * up the runners with their slots") and `ParticipantsWrapper.tsx` — no
 * consumer ever does. `ParticipantDetailsContext`'s own `statusMap` is
 * therefore never written to, `getParticipantStatus` always answers
 * `EMPTY_STATUS` (`hasMisconfigurationErrors: false`, every other flag false
 * or empty), and `isParticipantOKForChat`
 * (`features/chat-participants/lib/helpers.ts:116`) — the other input to the
 * card's "has errors" union — checks only the participant's ENTITY TYPE, not
 * its configuration validity. The practical result: no participant in Chat,
 * however broken (a deleted sub-agent reference, a toolkit missing its
 * credentials, anything) can ever show a misconfiguration warning here.
 *
 * This is a real feature-restoration job (wiring the runner with its MCP/
 * SharePoint/tool-validation slots at the real call site), not a small fix —
 * out of this package's budget. Pinned as a product gap; see
 * S/issues/gaps.md.
 */
import { test, expect } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  AUTOTEST_PREFIX,
  addConversationParticipant,
  attachSubAgent,
  createAgentWithVersion,
  createConversation,
  deleteAgent,
  deleteConversation,
} from '../../fixtures/api';

const SUFFIX = '-legacy-misconfig';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now() % 1_000_000}${SUFFIX}`;
}

/* elitea_issues: #4050, #4391, #2159 — product gap: the Chat participants
 * panel never shows a misconfiguration warning for ANY participant, however
 * broken — `ParticipantStatusRunner`/`useParticipantStatus` (the only code
 * that can turn `hasMisconfigurationErrors` true) has zero call sites in the
 * app. Reproduced here with a parent agent whose nested sub-agent reference
 * is dangling (the child was deleted after being attached). */
test('parent agent shows a misconfiguration warning in Chat when its nested sub-agent is broken', async ({ page }) => {
  test.setTimeout(60_000);
  test.fail(
    true,
    '#4050/#4391/#2159: product gap — ParticipantStatusRunner/useParticipantStatus (the only ' +
      'source of hasMisconfigurationErrors) is never rendered or called anywhere in the app; ' +
      'the Chat participants panel cannot show a misconfiguration warning for any participant',
  );

  const childName = uniqueName('child');
  const parentName = uniqueName('parent');
  const child = await createAgentWithVersion(page.request, childName, { instructions: 'Child.' });
  const parent = await createAgentWithVersion(page.request, parentName, { instructions: 'Parent.' });
  const attach = await attachSubAgent(page.request, parent.versionId, {
    applicationId: child.id,
    versionId: child.versionId,
  });
  expect(attach.ok(), 'attaching the sub-agent must succeed, or the dangling-reference setup means nothing').toBe(true);

  // Break the reference: the child agent no longer exists, but the parent's
  // toolkit list still names it — the shape #4050's "nested child has errors"
  // and #4391's "toolkit added without what it needs" both reduce to.
  await deleteAgent(page.request, child.id);

  const conversationId = await createConversation(page.request, uniqueName('conv'));
  try {
    await addConversationParticipant(page.request, conversationId, {
      entity_name: 'application',
      entity_meta: { id: parent.id },
    });

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    const expand = page.getByRole('button', { name: 'Expand participants' });
    if ((await expand.count()) > 0) await expand.click();
    await expect(page.getByTestId('participants-container')).toBeVisible({ timeout: 15_000 });

    const section = page.getByTestId('participants-section-Agents');
    await expect(section).toBeVisible({ timeout: 20_000 });

    // The ask: a warning icon/message on the parent's own row. Currently:
    // nothing ever lights this up, for any reason, on any participant.
    await expect(page.getByRole('img', { name: /warning/i }).or(page.getByText(/misconfig/i))).toBeVisible({
      timeout: 10_000,
    });
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, parent.id);
  }
});
