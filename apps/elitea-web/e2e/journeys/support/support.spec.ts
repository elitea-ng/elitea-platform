/**
 * The FIRST end-to-end journey for the in-app Support Assistant widget.
 *
 * Before this file, no E2E spec opened the widget at all. The only support
 * coverage was `e2e/journeys/admin/admin.features.spec.ts`, and it drives
 * the ADMIN section only — it never renders the widget the section
 * controls.
 *
 * Runs against the FULL standalone stack, project `support-stack`
 * (`scripts/support-e2e.sh`). A real support turn is an agent execution.
 * `deploy/docker-compose.e2e-standalone.yml` has no runtime plane, worker or
 * model backend, so the turn would fail there for a reason unrelated to
 * this widget.
 *
 * THE ANSWERING AGENT MUST LIVE IN THE SUPPORT PROJECT. A support turn
 * resolves `application_versions` from the support project's OWN tenant
 * schema (`ResolveCurrentApplicationTurn`), and requires the agent
 * participant's `entity_meta.project_id` to equal that project. The seed
 * below points `support_project_id` AND `support_agent_project_id` at the
 * SAME project the created agent lives in. Pointing them at different
 * projects makes the Go facade refuse with 503, by design — do not change
 * that in this file.
 *
 * The three tests run IN ORDER, inside one `serial` describe block (never at
 * file level — issue #539 is the reason this repository states that rule
 * once and checks it in `scripts/e2e-journey-shape.test.mjs`). Test 1 leaves
 * the switch OFF. Test 2 turns it ON and starts a turn. Test 3 reads that
 * turn back. Each depends on the one before it.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, createAgent, deleteAgent, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import {
  disableSupportAssistant,
  enableSupportAssistant,
  hasFencedSupportContext,
  waitForSupportTurnSettled,
} from './helpers';

// The seeding writes an admin-only section, and creates an agent. The same
// session then opens the widget as an ordinary authenticated user — the
// support project enrols any caller as a viewer on first use, admin
// included, so one persona covers both jobs.
test.use({ storageState: STORAGE_STATE.admin });

const SUPPORT_PROJECT_ID = Number(DEFAULT_PROJECT_ID);
const RUN_ID = Date.now();
const AGENT_NAME = `${AUTOTEST_PREFIX}support_agent_${RUN_ID}`;
const ASSISTANT_NAME = `${AUTOTEST_PREFIX}Support ${RUN_ID}`;
const WELCOME_MESSAGE = `${AUTOTEST_PREFIX}Welcome. Ask me about ELITEA.`;
const PLACEHOLDER = `${AUTOTEST_PREFIX}Type your question...`;

test.describe('the support assistant widget, off then on', () => {
  test.describe.configure({ mode: 'serial' });

  // Shared between tests 2 and 3. Set by test 2, read by test 3.
  let agentId = '';
  let conversationUuid = '';

  test.afterAll(async ({ request }) => {
    // Best effort. Restore the switch to OFF so a kept stack
    // (E2E_REUSE_STACK) does not carry the assistant ON into whatever runs
    // next, and remove the agent this file created.
    //
    // The conversation itself is NOT deleted: `supportassistant/conversations.go`
    // removed the delete route on purpose (nothing in the widget called it,
    // and one of the two things it delegated to would 500 on a missing
    // table) — there is no API left to clean it up with. It stays in the
    // support project, named by neither test, and does no harm.
    await disableSupportAssistant(request).catch(() => {});
    if (agentId) {
      await deleteAgent(request, agentId).catch(() => {});
    }
  });

  /** Land on an authenticated page and wait for the shell to be ready. */
  async function openApp(page: Page): Promise<void> {
    await page.goto(`${BASE_URL}/app/`, { waitUntil: 'domcontentloaded' });
    await page.waitForURL('**/chat**', { timeout: 15_000 });
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  }

  test('1. The assistant is off. The app shows no launcher. The config API reports it disabled.', async ({ page }) => {
    // A shared stack answers slower than a local one under load. No model
    // turn runs in this test, so this is headroom, not a model budget.
    test.setTimeout(120_000);

    // Force the OFF state first. A previous run of this file may have left
    // the switch ON — this test must not depend on run order across runs.
    await disableSupportAssistant(page.request);

    await openApp(page);

    // The launcher is absent, not merely hidden. `.count()` does not wait
    // for the element to attach, unlike `textContent()` or a visibility
    // assertion on a locator that might still be mounting — it reads the
    // DOM as it is right now, which is the correct read for an ABSENCE.
    await expect(page.locator('.elitea-assistant-button')).toHaveCount(0);

    const response = await page.request.get(`${API_BASE}/support_assistant/config`);
    expect(response.ok(), 'the config route must answer even when the assistant is off').toBe(true);
    const body = (await response.json()) as { enabled?: boolean };
    expect(body.enabled).toBe(false);
  });

  test('2. The assistant is on, points at an agent, and streams an answer.', async ({ page }) => {
    // A support turn is a real agent execution. The model behind it can be a
    // fast mock or a real remote one — this spec does not assume which — so
    // the budget is sized for the slow case, matching the heaviest of this
    // repository's own `chat-stream-real` waits.
    test.setTimeout(600_000);

    const agent = await createAgent(page.request, AGENT_NAME);
    agentId = agent.id;

    await enableSupportAssistant(page.request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentId),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME_MESSAGE,
      placeholder: PLACEHOLDER,
    });

    await openApp(page);

    const launcher = page.locator('.elitea-assistant-button');
    await expect(launcher).toBeVisible({ timeout: 20_000 });
    await launcher.click();

    const chatWindow = page.locator('.elitea-assistant-window');
    await expect(chatWindow).toBeVisible({ timeout: 10_000 });

    // Force a FRESH conversation. A stack left up between runs
    // (E2E_REUSE_STACK) may already hold history for this admin session,
    // and the widget opens on the MOST RECENT conversation rather than the
    // welcome screen when it has one.
    //
    // Scoped to the widget's own window: the app's main chat surface has its
    // OWN "New Chat" control (a different label, different case, but a
    // scoped locator costs nothing and cannot be confused with it).
    const newChatButton = chatWindow.getByRole('button', { name: 'New chat' });
    await expect(newChatButton).toBeEnabled({ timeout: 10_000 });
    await newChatButton.click();

    // The operator's own strings render — not the widget's built-in
    // defaults.
    await expect(chatWindow.locator('.elitea-assistant-header-title')).toHaveText(ASSISTANT_NAME);
    await expect(chatWindow.locator('.elitea-assistant-message--assistant')).toHaveCount(1);
    await expect(chatWindow.locator('.elitea-assistant-message--assistant').first()).toContainText(WELCOME_MESSAGE);
    await expect(chatWindow.locator('#elitea-assistant-message-input')).toHaveAttribute('placeholder', PLACEHOLDER);

    const question = `${AUTOTEST_PREFIX}question ${RUN_ID}`;

    // Armed BEFORE the send: the widget creates its conversation on the
    // FIRST message, and this response is the only place the UUID test 3
    // needs is named.
    const conversationCreated = page.waitForResponse(
      (response) =>
        response.url().includes('/support_assistant/conversations') && response.request().method() === 'POST',
      { timeout: 20_000 },
    );

    await chatWindow.locator('#elitea-assistant-message-input').fill(question);
    await chatWindow.locator('.elitea-assistant-send-button').click();

    const createdResponse = await conversationCreated;
    expect(createdResponse.ok(), 'the widget must create a conversation before it can start a turn').toBe(true);
    const createdBody = (await createdResponse.json()) as { uuid?: string };
    expect(createdBody.uuid, 'the created conversation must carry a uuid').toBeTruthy();
    conversationUuid = createdBody.uuid ?? '';

    // The user's own bubble renders immediately — optimistic, client-side,
    // before any server round trip.
    await expect(chatWindow.locator('.elitea-assistant-message--user').last()).toContainText(question);

    /*
     * THE ANSWER IS A SECOND ASSISTANT BUBBLE, and the count is what proves it.
     *
     * `.last()` alone does NOT: the welcome message is itself an assistant
     * bubble, it is non-empty, and it carries no error class. An assertion on
     * the last bubble therefore passes the instant the panel opens, whether or
     * not the agent ever answers — measured, on a run whose turn never started
     * at all. Waiting for the COUNT to reach two is the discriminating check,
     * and the text assertions then describe the answer rather than the
     * greeting.
     *
     * A generous budget: a real remote model can take minutes for one turn,
     * and this run may queue behind another stack's turn.
     */
    const assistantBubbles = chatWindow.locator('.elitea-assistant-message--assistant');
    await expect(assistantBubbles).toHaveCount(2, { timeout: 570_000 });

    const answerBubble = assistantBubbles.last();
    await expect(answerBubble).toContainText(/\S/, { timeout: 570_000 });
    await expect(answerBubble).not.toContainText(WELCOME_MESSAGE);
    await expect(answerBubble).not.toHaveClass(/elitea-assistant-message--error/);
  });

  test('3. The stored user message carries the page context.', async ({ page }) => {
    /*
     * THE SAME BUDGET AS TEST 2, for the same reason.
     *
     * Test 2 stops as soon as the FIRST token paints; this waits for the whole
     * answer to finish and its projection to land. Against a mock model that
     * is seconds. Against the standalone stack's real 35B model it took 3.6
     * minutes on one run and over 5 on the next, so a budget below test 2's
     * fails on model latency and reads as a broken feature.
     */
    test.setTimeout(600_000);

    expect(conversationUuid, 'test 2 must have created a conversation first').toBeTruthy();

    const details = await waitForSupportTurnSettled(page.request, conversationUuid, { timeout: 570_000 });
    expect(
      hasFencedSupportContext(details),
      'the stored user message must carry the <support_assistant_context> fenced block ' +
        '`composeUserInput` appends to every question',
    ).toBe(true);
  });
});
