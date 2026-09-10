/**
 * The agent's "Allow attachments" internal-tool gate, on the MAIN chat page.
 *
 * Ported by use case from `w1-chat-interface.md` (ELITEA-0509).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS IS test.fail() AND NOT A PASSING PORT
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
 * `src/widgets/chat-box/ui/ChatBoxInputSlots.tsx:177`, the composition root
 * for the MAIN `/app/chat` page (`buildChatBoxInputSlots`), never made that
 * connection: it hardcodes `disableAttachments: false` unconditionally,
 * regardless of which participant is active or what its version's
 * `internal_tools` says. An agent whose "Allow attachments" toggle is OFF
 * (the documented default) therefore has its attach control fully enabled
 * the moment it is added to a Chat-page conversation — the opposite of what
 * the same toggle does one screen over, in that agent's own editor.
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
  test.fail(
    true,
    "ELITEA-0509: product gap — ChatBoxInputSlots.tsx:177 hardcodes disableAttachments:false on the main chat page, ignoring the agent's 'attachments' internal_tools gate that the Agent/Pipeline editor's own chat correctly honors",
  );

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
