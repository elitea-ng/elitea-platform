/**
 * The Support Assistant widget's own CLIENT state — launcher, close/reopen,
 * and full view — against the e2e-standalone stack.
 *
 * Ported from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/support_assistant/
 * test_support_assistant_smoke.py`), by use case:
 *
 *  - `TestSupportAssistantLauncher::test_launcher_visible_and_opens_widget`
 *    — its own assertion here. `support.spec.ts` opens the launcher only as a
 *    precondition for a streaming turn, so the launcher/close half was proven
 *    only on a stack that can answer a question.
 *  - `TestSupportAssistantLauncher::test_widget_state_persists_after_close_reopen`
 *    — new. The legacy body sends a message first; a message needs a worker
 *    and a model, which `deploy/docker-compose.e2e-standalone.yml` has
 *    neither of. What that test is ABOUT — checklist 2.2.3, "opening/closing
 *    does not lose conversation state" — is a client fact: `ChatWindow`
 *    returns `null` while closed but is never unmounted, so `useChat`'s state
 *    survives. The draft in the composer is the discriminator that a remount
 *    would destroy, and it needs no turn.
 *  - `TestSupportAssistantViewModes::test_expand_collapse_fullview` — new.
 *
 * Three legacy cases from that file are deliberately NOT here:
 * `test_send_message_and_receive_response` (already covered by
 * `support.spec.ts` test 2, on the full stack), `test_new_chat_creates_fresh_session`
 * and `test_history_restore_and_continue` (both need a real prior turn — they
 * are ported into `support.spec.ts`, the `support-stack` project), and
 * `TestSupportAssistantAttachments` (the widget is vendored and its attachment
 * path was deliberately not ported — a product decision, not a hosting gap).
 *
 * ## Why this file is not in the `support-stack` project
 *
 * `playwright.config.ts`'s `SUPPORT_JOURNEY` names `support.spec.ts` exactly,
 * so this file is picked up by the ordinary `journeys/**` glob into chromium
 * and webkit. That is the point: none of the three tests below starts a turn,
 * so none of them needs the runtime plane.
 *
 * ## Why the writer half of the platform-flag lock
 *
 * `support_assistant_enabled` is ONE row for the whole deployment, and while
 * it is on EVERY page of every other journey grows a floating launcher
 * (`.elitea-assistant-button`, `position: fixed`, bottom-left, z-index
 * 2147483647). It is 1.75rem across and sits in the very corner, but a
 * `fixed` element at the top of the stacking order is exactly the shape that
 * eats a neighbouring click — the failure `e2e/fixtures/platformFlags.ts` was
 * written for, on `mcp_enabled`. So the switch is only ever on inside
 * `withPlatformFlagLock`, the window is one test long, and the `finally`
 * turns it off again.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgent, deleteAgent, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { withPlatformFlagLock } from '../../fixtures/platformFlags';

import { disableSupportAssistant, enableSupportAssistant } from './helpers';

// The seeding writes an admin-only section. The same session then opens the
// widget as an ordinary authenticated user — the support project enrols any
// caller as a viewer on first use, admin included, so one persona covers both
// jobs (the same reasoning `support.spec.ts` states).
test.use({ storageState: STORAGE_STATE.admin });

const SUPPORT_PROJECT_ID = Number(DEFAULT_PROJECT_ID);
const RUN_ID = Date.now();
const AGENT_NAME = `${AUTOTEST_PREFIX}sup_w_${String(RUN_ID).slice(-6)}`;
const ASSISTANT_NAME = `${AUTOTEST_PREFIX}Widget ${RUN_ID}`;
const WELCOME_MESSAGE = `${AUTOTEST_PREFIX}Welcome to the widget journey.`;
const PLACEHOLDER = `${AUTOTEST_PREFIX}Ask the widget...`;

/**
 * The agent the switch points at.
 *
 * `platformconfig.SupportAssistant.Ready()` requires the switch, a project
 * AND an agent id — a deployment with no agent reports `enabled: false` and
 * `ui/SupportAssistantWidget.tsx` then mounts nothing at all, so a journey
 * that skipped this step would assert an absent launcher and read as a
 * regression. Created once for the file.
 */
let agentId = '';

test.beforeAll(async ({ browser }) => {
  const context = await browser.newContext({ storageState: STORAGE_STATE.admin });
  try {
    const agent = await createAgent(context.request, AGENT_NAME);
    agentId = agent.id;
  } finally {
    await context.close();
  }
});

test.afterAll(async ({ browser }) => {
  /*
   * A NET, not the restore. Each test turns the switch off in its own
   * `finally`, inside the lock, so the window stays as short as the
   * assertions allow. This runs afterwards and covers the one case that
   * `finally` cannot: a test that ends on its own TIMEOUT loses its page
   * while the restore is still running — measured on
   * `admin.features.spec.ts`, which left `mcp_enabled` false for the rest of
   * the run. Its own context, so neither a dead page nor a spent clock can
   * stop it.
   *
   * The conversations the widget created are NOT deleted: the delete route
   * was removed on purpose (`supportassistant/conversations.go`), so there is
   * no API left to clean them up with. They stay in the support project,
   * named by no test, and do no harm.
   */
  const context = await browser.newContext({ storageState: STORAGE_STATE.admin });
  try {
    await disableSupportAssistant(context.request).catch(() => {});
    if (agentId) await deleteAgent(context.request, agentId).catch(() => {});
  } finally {
    await context.close();
  }
});

/**
 * Run `body` with the assistant switched ON, exclusively.
 *
 * 210 s is the arithmetic of the lock rather than a round number, and it is
 * the same sum `admin.features.spec.ts` states: up to STALE_MS (90 s) to take
 * the writer, up to STALE_MS again for the readers to drain, and about 30 s
 * of assertions after that.
 */
async function withAssistantOn(page: Page, body: () => Promise<void>): Promise<void> {
  test.setTimeout(210_000);
  await withPlatformFlagLock(async () => {
    await enableSupportAssistant(page.request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentId),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME_MESSAGE,
      placeholder: PLACEHOLDER,
    });
    try {
      await body();
    } finally {
      await disableSupportAssistant(page.request).catch(() => {});
    }
  });
}

/** Land on an authenticated page and wait for the shell to be ready. */
async function openApp(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/`, { waitUntil: 'domcontentloaded' });
  await page.waitForURL('**/chat**', { timeout: 15_000 });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
}

/**
 * Open the widget on a FRESH conversation.
 *
 * A stack kept between runs (`E2E_REUSE_STACK`) may already hold history for
 * this admin session, and `useChat`'s init effect opens on the MOST RECENT
 * conversation rather than on the welcome screen when it has one. "New chat"
 * is scoped to the widget's own window: the app's main chat surface carries
 * its own new-conversation control.
 */
async function openWidgetFresh(page: Page) {
  const launcher = page.locator('.elitea-assistant-button');
  await expect(launcher, 'the launcher must render while the assistant is on').toBeVisible({
    timeout: 20_000,
  });
  await launcher.click();

  const chatWindow = page.locator('.elitea-assistant-window');
  await expect(chatWindow).toBeVisible({ timeout: 15_000 });

  const newChat = chatWindow.getByRole('button', { name: 'New chat' });
  await expect(newChat, 'the header is disabled until the init read settles').toBeEnabled({
    timeout: 20_000,
  });
  await newChat.click();

  // The OPERATOR's strings, not the widget's built-in defaults — proof that
  // the config this file wrote is what the widget rendered from.
  await expect(chatWindow.locator('.elitea-assistant-header-title')).toHaveText(ASSISTANT_NAME);
  await expect(chatWindow.locator('.elitea-assistant-message--assistant')).toHaveCount(1);
  await expect(chatWindow.locator('#elitea-assistant-message-input')).toHaveAttribute(
    'placeholder',
    PLACEHOLDER,
  );
  return chatWindow;
}

test('SUP-W1: the launcher opens the widget and the close control puts it away (legacy test_launcher_visible_and_opens_widget)', async ({
  page,
}) => {
  await withAssistantOn(page, async () => {
    await openApp(page);

    const launcher = page.locator('.elitea-assistant-button');
    await expect(launcher).toBeVisible({ timeout: 20_000 });
    await launcher.click();

    const chatWindow = page.locator('.elitea-assistant-window');
    await expect(chatWindow).toBeVisible({ timeout: 15_000 });
    // The window is the OPERATOR's, not a generic panel.
    await expect(chatWindow.locator('.elitea-assistant-header-title')).toHaveText(ASSISTANT_NAME, {
      timeout: 20_000,
    });

    await chatWindow.locator('.elitea-assistant-header-close-action').click();

    // ABSENT, not merely hidden: `ChatWindow` returns null while closed, so
    // there is no element to be invisible. `.count()` reads the DOM as it is
    // rather than waiting for an attach that will never happen.
    await expect
      .poll(async () => page.locator('.elitea-assistant-window').count(), {
        timeout: 15_000,
        message: 'the close control left the widget window mounted',
      })
      .toBe(0);

    // …and the launcher is still there to open it again. That is the legacy
    // assertion this test ends on, and it is what tells "closed" from "the
    // whole widget unmounted".
    await expect(launcher).toBeVisible();
  });
});

test('SUP-W2: closing and reopening the widget keeps its conversation state (legacy test_widget_state_persists_after_close_reopen)', async ({
  page,
}) => {
  await withAssistantOn(page, async () => {
    await openApp(page);
    const chatWindow = await openWidgetFresh(page);

    /*
     * THE DRAFT IS THE DISCRIMINATOR.
     *
     * The welcome bubble comes back on a REMOUNT as well — `useChat` rebuilds
     * it from `welcomeMessage` — so a bubble count alone passes whether or
     * not any state survived. `inputText` does not: it exists only in the
     * live hook instance, and a `ChatWindow` that was unmounted while closed
     * would reopen with an empty composer. Both are asserted; only this one
     * can fail for the reason the test is about.
     */
    const draft = `${AUTOTEST_PREFIX}draft ${RUN_ID}`;
    const input = chatWindow.locator('#elitea-assistant-message-input');
    await input.fill(draft);
    await expect(input).toHaveValue(draft);

    await chatWindow.locator('.elitea-assistant-header-close-action').click();
    await expect
      .poll(async () => page.locator('.elitea-assistant-window').count(), { timeout: 15_000 })
      .toBe(0);

    await page.locator('.elitea-assistant-button').click();
    const reopened = page.locator('.elitea-assistant-window');
    await expect(reopened).toBeVisible({ timeout: 15_000 });

    await expect(
      reopened.locator('#elitea-assistant-message-input'),
      'reopening the widget must not discard what the user had typed',
    ).toHaveValue(draft);
    await expect(reopened.locator('.elitea-assistant-message--assistant')).toHaveCount(1);
    await expect(reopened.locator('.elitea-assistant-message--assistant').first()).toContainText(
      WELCOME_MESSAGE,
    );
  });
});

test('SUP-W3: the widget expands to full view and collapses back (legacy test_expand_collapse_fullview)', async ({
  page,
}) => {
  await withAssistantOn(page, async () => {
    await openApp(page);
    const chatWindow = await openWidgetFresh(page);

    // `elitea-assistant-window--expanded` is the class `ChatWindow` adds, and
    // the scrim is a real dismiss CONTROL rather than decoration — this port
    // gave it a role and a name, so it is addressable here.
    await expect(chatWindow).not.toHaveClass(/elitea-assistant-window--expanded/);
    await expect(page.getByRole('button', { name: 'Collapse the assistant' })).toHaveCount(0);

    await chatWindow.getByRole('button', { name: 'Expand chat' }).click();

    await expect(page.locator('.elitea-assistant-window')).toHaveClass(
      /elitea-assistant-window--expanded/,
      { timeout: 15_000 },
    );
    await expect(page.getByRole('button', { name: 'Collapse the assistant' })).toBeVisible();

    // Still a working widget in full view, which is what the legacy body
    // asserts between the two clicks.
    await expect(page.locator('.elitea-assistant-header-title')).toHaveText(ASSISTANT_NAME);

    await page.locator('.elitea-assistant-window').getByRole('button', { name: 'Expand chat' }).click();

    await expect(page.locator('.elitea-assistant-window')).not.toHaveClass(
      /elitea-assistant-window--expanded/,
      { timeout: 15_000 },
    );
    await expect(page.getByRole('button', { name: 'Collapse the assistant' })).toHaveCount(0);
    await expect(page.locator('.elitea-assistant-window')).toBeVisible();
  });
});
