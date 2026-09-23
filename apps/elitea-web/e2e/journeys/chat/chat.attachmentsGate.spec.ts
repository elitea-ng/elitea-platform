/**
 * The agent's "Allow attachments" internal-tool gate, on the MAIN chat page.
 *
 * Ported by use case from `w1-chat-interface.md` (ELITEA-0509).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE GAP THIS CLOSED (#905)
 * ─────────────────────────────────────────────────────────────────────────────
 * The gate is real, and it DOES work — one level up the stack. An agent
 * participant's attach control is only meant to be enabled when
 * `'attachments'` is present in its version's `meta.internal_tools`
 * (`entities/application-form/model/toolStatus.ts`'s `isAttachmentsEnabled`),
 * and `features/agents/lib/useAgentAttachments.ts` /
 * `features/pipelines/lib/hooks/usePipelineAttachments.ts` both correctly
 * compute `disableAttachments = !isAttachmentsEnabled(internalTools)` for the
 * Agent/Pipeline EDITOR's own embedded test-chat.
 *
 * `widgets/chat-box`, the composition root for the MAIN `/app/chat` page,
 * never made that connection: the composer's only `disableAttachments` input
 * was the in-flight-run flag (A17's `isStreaming`), regardless of which
 * participant was active or what its version's `internal_tools` said. An
 * agent whose "Allow attachments" toggle is OFF (the documented default)
 * therefore had its attach control fully enabled the moment it was added to a
 * Chat-page conversation — the opposite of what the same toggle does one
 * screen over, in that agent's own editor.
 *
 * `ChatBox.helpers.ts`'s `shouldDisableParticipantAttachments` now runs the
 * same `isAttachmentsEnabled` predicate against the ACTIVE participant's
 * resolved `version_details.meta.internal_tools`
 * (`useChatBoxParticipant`'s `activeParticipantInternalTools`). It is scoped
 * to agent/pipeline participants, so a plain-model conversation is untouched
 * — the second test below is what holds that scoping honest.
 *
 * This is measured directly against the running stack below, not inferred:
 * the row's own `aria-disabled` attribute is read after attaching an agent
 * with no `attachments` entry in `meta.internal_tools`.
 */
import { test, expect } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createAgentWithVersion,
  createConversation,
  deleteAgent,
  deleteConversation,
} from '../../fixtures/api';

const SUFFIX = '-attachgate';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
}

/* onetest: ELITEA-0509 — an agent's "Allow attachments" toggle must disable the composer's attach control on the main chat page, the same way it does in the agent's own editor */
test('the "Allow attachments" toggle gates the attach control on the main chat page too', async ({ page }) => {

  const agentName = uniqueName('gateag');
  // The documented default: no `attachments` entry, so `isAttachmentsEnabled`
  // must answer false and the attach control must be disabled.
  const agent = await createAgentWithVersion(page.request, agentName, {
    agentType: 'openai',
    meta: { internal_tools: [] },
  });

  const conversationId = await createConversation(page.request, uniqueName('gateconv'));
  try {
    const path = `/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`;
    const attached = await page.request.post(`${API_BASE}${path}`, {
      data: [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID, name: agentName },
          entity_settings: { version_id: agent.versionId },
        },
      ],
    });
    expect(attached.status(), 'the agent participant must attach').toBe(200);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
    await checkA11y(page);

    const plus = page.getByTestId('plus-menu-button');
    await expect(plus).toBeEnabled({ timeout: 20_000 });
    await plus.click();

    const attachRow = page.getByTestId('plus-menu-attachments');
    await expect(attachRow).toBeVisible({ timeout: 10_000 });
    await expect(
      attachRow,
      'an agent with attachments OFF must disable the attach row on the main chat page, the same way it does in its own editor',
    ).toHaveAttribute('aria-disabled', 'true');
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agent.id);
  }
});

/* onetest: ELITEA-0509 — the same gate must not over-reach: an agent WITH "Allow attachments" on keeps its attach control enabled on the main chat page */
test('an agent with "Allow attachments" ON keeps the attach control enabled', async ({ page }) => {
  const agentName = uniqueName('gateon');
  const agent = await createAgentWithVersion(page.request, agentName, {
    agentType: 'openai',
    meta: { internal_tools: ['attachments'] },
  });

  const conversationId = await createConversation(page.request, uniqueName('gateonconv'));
  try {
    const path = `/elitea_core/participants/prompt_lib/${DEFAULT_PROJECT_ID}/${conversationId}`;
    const attached = await page.request.post(`${API_BASE}${path}`, {
      data: [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: DEFAULT_PROJECT_ID, name: agentName },
          entity_settings: { version_id: agent.versionId },
        },
      ],
    });
    expect(attached.status(), 'the agent participant must attach').toBe(200);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    const plus = page.getByTestId('plus-menu-button');
    await expect(plus).toBeEnabled({ timeout: 20_000 });
    await plus.click();

    const attachRow = page.getByTestId('plus-menu-attachments');
    await expect(attachRow).toBeVisible({ timeout: 10_000 });
    await expect(
      attachRow,
      'the gate must read the toggle, not disable attachments for every agent',
    ).toHaveAttribute('aria-disabled', 'false');
  } finally {
    await deleteConversation(page.request, conversationId);
    await deleteAgent(page.request, agent.id);
  }
});
