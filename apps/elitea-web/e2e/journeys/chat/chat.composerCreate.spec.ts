/**
 * Journey: creating an agent from the chat composer's "+" menu (issue #867).
 *
 * Before this fix, the "+" menu's Agents/Pipelines/Toolkits submenus showed
 * no "Create new" row at all — `usePlusMenuEntities.ts`'s own doc comment
 * said so directly, and `resolveSubmenuCreateConfig` hides the row when no
 * `onCreate*` handler reaches it. This journey exercises the closed loop:
 * open the menu, create a brand-new agent through the real inline editor
 * (the same `AgentEditor` the participant rail's edit icon opens), and
 * assert the SERVER holds both a new agent row and a participant mapping
 * onto the conversation the click happened in — the same
 * "a UI click could close the menu and change nothing" discriminator
 * `chat.management.spec.ts`'s M2b journey establishes for the sibling
 * existing-entity picker.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, DEFAULT_PROJECT_ID, createConversation, deleteAgent, deleteConversation } from '../../fixtures/api';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}`;
}

const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;

interface ParticipantRow {
  readonly entity_name?: string;
  readonly entity_meta?: { readonly id?: string | number };
}

async function readParticipants(request: APIRequestContext, conversationId: string): Promise<readonly ParticipantRow[]> {
  const response = await request.get(`${API_BASE}${CONVERSATION_PATH}/${conversationId}`);
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { participants?: readonly ParticipantRow[] };
  return body.participants ?? [];
}

/** Finds the agent the server created for this journey, by name, so its id can be cleaned up — the editor's own POST response is not read directly by this test (it asserts through the SERVER's participant mapping instead, the same M2b discriminator). */
async function findCreatedAgentId(request: APIRequestContext, agentName: string): Promise<string | undefined> {
  const response = await request.get(`${API_BASE}/elitea_core/applications/prompt_lib/${DEFAULT_PROJECT_ID}?query=${agentName}`);
  if (!response.ok()) return undefined;
  const body = (await response.json()) as { rows?: readonly { id?: string | number; name?: string }[] };
  const row = (body.rows ?? []).find((r) => r.name === agentName);
  return row?.id === undefined ? undefined : String(row.id);
}

test('creating an agent from the "+" menu attaches it to the conversation the click happened in', async ({ page }) => {
  const conversationId = await createConversation(page.request, uniqueName('composercreate'));
  // Short tag: `CreateAgentForm`'s name field is capped at
  // `shared/lib/limits.ts`'s `MAX_NAME_LENGTH` (32, with an on-screen
  // "characters left" counter) and truncates on type — `uniqueName`'s own
  // `${AUTOTEST_PREFIX}${tag}-${Date.now()}` shape is 43+ chars with the
  // longer tag other journeys use, which silently saved a DIFFERENT
  // (truncated) name than `agentName` names below, so the by-name lookup
  // after creation never found it (#882 CI).
  const agentName = uniqueName('cc-agent');
  let createdAgentId: string | undefined;

  try {
    // Nothing is attached yet, so what the create flow below does is the
    // only thing that could attach anything.
    expect(await readParticipants(page.request, conversationId)).toEqual([]);

    await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    const plus = page.getByTestId('plus-menu-button');
    await expect(plus).toBeEnabled({ timeout: 20_000 });
    await plus.click();
    const agentsRow = page.getByTestId('plus-menu-agents');
    await expect(agentsRow).toBeVisible({ timeout: 10_000 });
    await agentsRow.click();

    // The submenu is populated (an existing-entity picker, per M2b) AND now
    // carries a "Create new" row — the row this fix adds.
    const createNew = page.getByTestId('plus-submenu-create-new');
    await expect(createNew, 'the Agents submenu must offer "Create new" once an onCreateAgent handler is wired').toBeVisible({ timeout: 10_000 });
    await createNew.click();

    // The real AgentEditor opens in create mode — same component/testids
    // `agents.editor.spec.ts` drives on the standalone agent-create page.
    const nameInput = page.getByTestId('agent-name-input');
    await expect(nameInput).toBeVisible({ timeout: 15_000 });
    await nameInput.fill(agentName);
    await page.getByTestId('agent-description-input').fill(`${AUTOTEST_PREFIX}created from the chat composer`);

    const saveButton = page.getByTestId('agent-save-button');
    await expect(saveButton).toBeEnabled({ timeout: 10_000 });
    await saveButton.click();

    // The attach is a SERVER-side mapping — a picker/editor that closes
    // without actually attaching anything is indistinguishable from a
    // working one on screen alone, which is why this polls the API rather
    // than asserting only that the editor closed.
    await expect
      .poll(async () => (await readParticipants(page.request, conversationId)).map((p) => p.entity_name), {
        timeout: 20_000,
        message: 'creating an agent from the "+" menu must attach it to the conversation it was created in',
      })
      .toEqual(['application']);

    createdAgentId = await findCreatedAgentId(page.request, agentName);
    expect(createdAgentId, 'the created agent must be findable server-side by name').toBeDefined();

    const attached = await readParticipants(page.request, conversationId);
    expect(String(attached[0]?.entity_meta?.id)).toBe(String(createdAgentId));
  } finally {
    await deleteConversation(page.request, conversationId);
    if (createdAgentId !== undefined) await deleteAgent(page.request, createdAgentId);
  }
});
