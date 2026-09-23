/**
 * The composer's "Modules" panel — the chat-side internal-tools catalogue, and
 * the fact that a toggle is a conversation setting rather than a screen state.
 *
 * Ported by use case from the legacy public suite:
 *  - `tests/ui/chat/test_chat_interface.py::TestConversationUIElements::test_internal_tools_panel_shows_all_tools`
 *    (TC-CHAT-012)
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE LEGACY ASSERTION ("exactly 7 tools") IS NOT PORTED LITERALLY
 * ─────────────────────────────────────────────────────────────────────────────
 * `useAvailableInternalTools` does not return a fixed list. It filters
 * `INTERNAL_TOOLS_LIST` three ways (`features/agents/lib/internalTools.ts`):
 *
 *  - `agentOnly` tools are dropped unless the caller asks for them, and the
 *    chat surface does not (`useChatBoxInternalTools` passes
 *    `includeAgentOnly: !!isAgentsPage`). `attachments` is the only such tool,
 *    and it is the one the legacy suite's own count got wrong — that list
 *    counted the AGENT editor's panel, not the chat's.
 *  - `internal_mcp` needs `useIsMcpVisible()`, an admin-configurable gate.
 *  - `image_generation` needs the project's toolkit-type schema map to carry
 *    `ImageGenServiceProvider_ImageGen`.
 *
 * So a hardcoded count asserts the seed, not the product, and would go red the
 * day an operator turns MCP off. What is asserted instead are the three rules
 * that hold whatever the gates say: every ungated tool is offered, every
 * offered row is a member of the catalogue (no invented rows), and the
 * agent-only tool is NOT offered on the chat surface.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * AND THE HALF THE LEGACY TEST NEVER REACHED
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy test opened the panel and counted switches. It never flipped one.
 * A panel of switches that toggle nothing looks identical. `useChatBoxInternalTools`
 * optimistically updates its local snapshot and then `PUT`s
 * `meta.internal_tools` on the conversation, reverting the local state if the
 * write fails — so the ONLY way to tell a working toggle from a decorative one
 * is to read the conversation back from the server. Both directions are
 * asserted there, because a toggle that only ever appends is a real and easy
 * bug in that hook's `value ? [...prev, key] : prev.filter(...)` line.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';

/** Every entity this file creates carries this suffix (concurrent-agent hygiene). */
const SUFFIX = '-tools';

const CONVERSATION_PATH = `/elitea_core/conversation/prompt_lib/${DEFAULT_PROJECT_ID}`;

/**
 * `INTERNAL_TOOLS_LIST` (`features/agents/lib/internalTools.ts`), split by the
 * gate each entry sits behind. Names, not titles: the name is what the row is
 * keyed on and what `meta.internal_tools` stores; the title is only a label.
 */
const UNGATED_TOOLS = ['data_analysis', 'planner', 'pyodide', 'swarm', 'lazy_tools_mode'] as const;
/**
 * #940 A8's two chat-authored builder modules. Ungated like the five above —
 * neither declares `agentOnly` nor a `requiredToolkitType`, deliberately: a
 * toolkit-type gate would make the toggle VANISH in every project with no such
 * provider, and these two need none. They are listed separately only because
 * the independence assertions below are theirs.
 */
const BUILDER_TOOLS = ['skills_builder', 'project_context_builder'] as const;
/** Offered only when their gate is open — present or absent, never invented. */
const GATED_TOOLS = ['image_generation', 'internal_mcp'] as const;
/** `agentOnly: true` — the agent editor's panel offers it, the chat's must not. */
const AGENT_ONLY_TOOL = 'attachments';

/** The tool this journey switches on and off. Ungated, so it is offered in every deployment. */
const TOGGLED_TOOL = 'planner';

/** The conversation's `meta.internal_tools`, as the server holds it right now. */
async function readInternalTools(
  request: APIRequestContext,
  conversationId: string,
): Promise<readonly string[]> {
  const response = await request.get(`${API_BASE}${CONVERSATION_PATH}/${conversationId}`);
  expect(response.status()).toBe(200);
  const body = (await response.json()) as { meta?: { internal_tools?: readonly string[] } };
  return body.meta?.internal_tools ?? [];
}

/**
 * Opens the composer's "+" menu and then its "Modules" submenu.
 *
 * The rows open their submenu on hover as well as click (`MainMenuList`), and
 * the submenu renders in its own portal beside the row rather than replacing
 * the menu — so the main menu stays on screen and nothing here closes it.
 */
async function openModules(page: Page) {
  const plus = page.getByTestId('plus-menu-button');
  await expect(plus).toBeEnabled({ timeout: 20_000 });
  await plus.click();
  const row = page.getByTestId('plus-menu-tools');
  await expect(row).toBeVisible({ timeout: 10_000 });
  await row.click();
  const items = page.getByTestId('plus-submenu-item');
  await expect(items.first()).toBeVisible({ timeout: 10_000 });
  return items;
}

test('the Modules panel offers the chat catalogue, and a switch is a conversation setting', async ({ page }) => {
  const conversationId = await createConversation(
    page.request,
    `${AUTOTEST_PREFIX}modules${SUFFIX}-${Date.now()}`,
  );

  // A fresh conversation carries no tools — the starting point every assertion
  // below is measured against.
  expect(await readInternalTools(page.request, conversationId)).toEqual([]);

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

  const rows = await openModules(page);

  // The rows the panel really rendered, by the key they are keyed on.
  const offered = await rows.evaluateAll((nodes) =>
    nodes.map((node) => node.getAttribute('data-item-key') ?? ''),
  );

  // 1. Every ungated tool is there.
  for (const tool of UNGATED_TOOLS) {
    expect(offered, `the chat Modules panel must offer ${tool}`).toContain(tool);
  }
  // 1b. ELITEA-2783 step 1 — and both builder modules with them.
  for (const tool of BUILDER_TOOLS) {
    expect(offered, `the chat Modules panel must offer ${tool}`).toContain(tool);
  }
  // 2. Nothing outside the catalogue is there — a panel that invented rows, or
  //    that leaked the agent editor's list, fails here.
  const known = new Set<string>([...UNGATED_TOOLS, ...BUILDER_TOOLS, ...GATED_TOOLS]);
  for (const key of offered) {
    expect(known.has(key), `${key} is not a member of INTERNAL_TOOLS_LIST for the chat surface`).toBe(true);
  }
  // 3. …and the agent-only tool is not, however the gates are configured.
  expect(offered, 'attachments is agentOnly and belongs to the agent editor, not the chat').not.toContain(
    AGENT_ONLY_TOOL,
  );

  await checkA11y(page);

  // ── the switch is a setting, not a screen state ──────────────────────────
  // The row IS the element carrying the key (`PlusChatSubmenu` puts both
  // attributes on the same `MenuItem`), so it is addressed directly rather than
  // filtered out of the list — two entities can share a display name, and the
  // key is the identity the list is built on.
  const row = page.locator(`[data-testid="plus-submenu-item"][data-item-key="${TOGGLED_TOOL}"]`);
  await expect(row).toHaveCount(1);
  // The Switch stops the click from reaching the row, so this fires the change
  // handler exactly once rather than twice.
  const toggle = row.locator('input[type="checkbox"]');
  await expect(toggle).not.toBeChecked();

  await toggle.click();
  await expect
    .poll(async () => [...(await readInternalTools(page.request, conversationId))], {
      timeout: 15_000,
      message: 'switching a module on must persist onto the conversation',
    })
    .toEqual([TOGGLED_TOOL]);
  await expect(toggle).toBeChecked();

  // Off again — the same control must REMOVE the entry, not append a second
  // copy of it or leave the list as it was.
  await toggle.click();
  await expect
    .poll(async () => [...(await readInternalTools(page.request, conversationId))], {
      timeout: 15_000,
      message: 'switching a module off must remove it from the conversation',
    })
    .toEqual([]);
  await expect(toggle).not.toBeChecked();

  await deleteConversation(page.request, conversationId);
});

/* onetest: ELITEA-2783 — Skills Builder and Project Context Builder are
   independent toggles, disabled by default, each with its own tooltip.
   ELITEA-2779 — while a module is OFF the conversation stores nothing for it,
   which is what leaves the runtime with no tool to bind however explicit a
   later request is (the binding half is `internal_tools.rs`'s own tests; the
   turn half is `e2e/streaming/chat.builders.spec.ts`).
   ELITEA-2778 — turning one on leaves every other module exactly as it was. */
test('the two builder modules are off by default and toggle independently of each other', async ({ page }) => {
  const conversationId = await createConversation(
    page.request,
    `${AUTOTEST_PREFIX}builders${SUFFIX}-${Date.now()}`,
  );

  // ELITEA-2779's precondition, asserted rather than assumed: nothing is on.
  expect(await readInternalTools(page.request, conversationId)).toEqual([]);

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });
  await openModules(page);

  const rowFor = (key: string) =>
    page.locator(`[data-testid="plus-submenu-item"][data-item-key="${key}"]`);
  const skills = rowFor('skills_builder');
  const projectContext = rowFor('project_context_builder');
  await expect(skills).toHaveCount(1);
  await expect(projectContext).toHaveCount(1);

  // The LABELS the cases name. Asserted on the rendered row rather than on the
  // catalogue constant: a panel that renders the right key under the wrong
  // label is what a user actually meets.
  await expect(skills).toContainText('Skills Builder');
  await expect(projectContext).toContainText('Project Context Builder');

  // Step 2 — both off by default.
  const skillsToggle = skills.locator('input[type="checkbox"]');
  const contextToggle = projectContext.locator('input[type="checkbox"]');
  await expect(skillsToggle, 'Skills Builder must be disabled by default').not.toBeChecked();
  await expect(contextToggle, 'Project Context Builder must be disabled by default').not.toBeChecked();

  // Step 3 — one on, the other untouched, measured on the SERVER. A pair of
  // switches wired to one setting looks identical on screen until this read.
  await skillsToggle.click();
  await expect
    .poll(async () => [...(await readInternalTools(page.request, conversationId))], {
      timeout: 15_000,
      message: 'enabling Skills Builder must persist onto the conversation',
    })
    .toEqual(['skills_builder']);
  await expect(contextToggle, 'enabling one builder must not enable the other').not.toBeChecked();

  // Step 4 — swap them. The first must come OFF, which is the half a toggle
  // that only ever appends gets wrong.
  await skillsToggle.click();
  await contextToggle.click();
  await expect
    .poll(async () => [...(await readInternalTools(page.request, conversationId))], {
      timeout: 15_000,
      message: 'the two builder modules must toggle independently',
    })
    .toEqual(['project_context_builder']);
  await expect(skillsToggle).not.toBeChecked();

  // ELITEA-2778: an unrelated module switched on alongside is unaffected — the
  // regression the case is written to catch is one module's write clobbering
  // the list rather than adding to it.
  await rowFor(TOGGLED_TOOL).locator('input[type="checkbox"]').click();
  await expect
    .poll(async () => [...(await readInternalTools(page.request, conversationId))].sort(), {
      timeout: 15_000,
      message: 'enabling another module must not drop the builder module already on',
    })
    .toEqual(['planner', 'project_context_builder']);

  await deleteConversation(page.request, conversationId);
});
