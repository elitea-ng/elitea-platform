/**
 * Support Assistant entry points — the sidebar footer item
 * (`sidebar-support-assistant`) and the floating launcher
 * (`.elitea-assistant-button`), across pages and platform-flag states.
 *
 * Ported by use case from the onetest `elitea-chat-bot` package. None of the
 * tests below sends a chat message — a send is a real agent turn, which the
 * e2e-standalone stack has no worker for (the same boundary
 * `support.widget.spec.ts`'s SUP-W4 states) — so every case here is one this
 * stack CAN answer: presence/absence of the two entry points, and that they
 * open the SAME session (proved with an unsent draft, the same
 * discriminator `support.widget.spec.ts`'s SUP-W2 uses, since a remounted
 * window would not carry it).
 *
 * Runs in the ordinary `chromium`/`webkit` projects (not `support-stack`):
 * `playwright.config.ts`'s `SUPPORT_JOURNEY` names `support.spec.ts` only.
 *
 * ── ONE PLATFORM FLAG, IN ORDER ──────────────────────────────────────────
 *
 * `support_assistant_enabled` is one row for the whole deployment, exactly
 * the reason `support.widget.spec.ts` gives for its own serial waiver
 * (`scripts/e2e-journey-shape.test.mjs`'s `FILE_LEVEL_SERIAL`, issue #539):
 * `fullyParallel` would put every test below in its own worker, each taking
 * the same exclusive window, and the queue is the failure — not a defect in
 * any one test. Serial removes the queue: one worker, one test at a time,
 * each holding the window uncontended.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';
import { AUTOTEST_PREFIX, createAgent, deleteAgent, DEFAULT_PROJECT_ID } from '../../fixtures/api';
import { withPlatformFlagLock } from '../../fixtures/platformFlags';

import { disableSupportAssistant, enableSupportAssistant } from './helpers';

test.use({ storageState: STORAGE_STATE.admin });
test.describe.configure({ mode: 'serial' });

const SUPPORT_PROJECT_ID = Number(DEFAULT_PROJECT_ID);
const RUN_ID = Date.now();
const AGENT_NAME = `${AUTOTEST_PREFIX}sup_ep_${String(RUN_ID).slice(-6)}`;
const ASSISTANT_NAME = `${AUTOTEST_PREFIX}Entrypoints`;
const WELCOME_MESSAGE = `${AUTOTEST_PREFIX}Welcome to the entrypoints journey.`;
const PLACEHOLDER = `${AUTOTEST_PREFIX}Ask...`;

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
  test.setTimeout(120_000);
  const context = await browser.newContext({ storageState: STORAGE_STATE.admin });
  try {
    await withPlatformFlagLock(async () => {
      await disableSupportAssistant(context.request).catch(() => {});
    });
    if (agentId) await deleteAgent(context.request, agentId).catch(() => {});
  } finally {
    await context.close();
  }
});

async function openApp(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/`, { waitUntil: 'domcontentloaded' });
  await page.waitForURL('**/chat**', { timeout: 15_000 });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
}

/* onetest: ELITEA-0634 — no launcher and no sidebar entry appear anywhere while the feature is disabled */
test('no launcher or sidebar entry appears while the assistant is disabled', async ({ page }) => {
  await withPlatformFlagLock(async () => {
    await disableSupportAssistant(page.request);
    await openApp(page);
    await expect(page.locator('.elitea-assistant-button')).toHaveCount(0);
    await expect(page.getByTestId('sidebar-support-assistant')).toHaveCount(0);

    await page.goto(`${BASE_URL}/app/agents`, { waitUntil: 'domcontentloaded' });
    await expect(page.locator('.elitea-assistant-button')).toHaveCount(0);
    await expect(page.getByTestId('sidebar-support-assistant')).toHaveCount(0);
  });
});

/* onetest: ELITEA-0626 — the floating launcher is visible on every main application page */
test('the launcher is visible on every main application page', async ({ page }) => {
  await withPlatformFlagLock(async () => {
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

    for (const path of ['/app/agents', '/app/pipelines', '/app/credentials', '/app/toolkits']) {
      await page.goto(`${BASE_URL}${path}`, { waitUntil: 'domcontentloaded' });
      await expect(launcher, `the launcher must survive navigating to ${path}`).toBeVisible({
        timeout: 20_000,
      });
    }
  });
});

/* onetest: ELITEA-0630, ELITEA-0635 — the sidebar entry sits at the footer, distinct from the main nav, and stays reachable (icon + tooltip) when the sidebar is collapsed */
test('the sidebar entry is present, distinct from the nav group, and survives collapsing the sidebar', async ({
  page,
}) => {
  await withPlatformFlagLock(async () => {
    await enableSupportAssistant(page.request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentId),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME_MESSAGE,
      placeholder: PLACEHOLDER,
    });
    await openApp(page);

    const entry = page.getByTestId('sidebar-support-assistant');
    await expect(entry).toBeVisible({ timeout: 20_000 });
    await expect(entry).toContainText('Support Bot');

    // Standard nav items are unaffected — the entry is a footer element, not
    // inserted into the nav list itself.
    await expect(page.getByRole('link', { name: 'Chats' }).first()).toBeVisible();

    const collapseToggle = page.getByTestId('sidebar-collapse-toggle');
    await collapseToggle.click();
    // The icon-only entry is still there and still reachable.
    await expect(entry).toBeVisible();

    await collapseToggle.click();
    await expect(entry).toContainText('Support Bot');
  });
});

/* onetest: ELITEA-0610, ELITEA-0632 — the sidebar entry and the floating launcher open the SAME session (same-page close/reopen, entry points swapped) */
test('the sidebar entry and the launcher share one session, on a close and reopen', async ({ page }) => {
  test.setTimeout(60_000);
  await withPlatformFlagLock(async () => {
    await enableSupportAssistant(page.request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentId),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME_MESSAGE,
      placeholder: PLACEHOLDER,
    });
    await openApp(page);

    // Open via the SIDEBAR entry first.
    await page.getByTestId('sidebar-support-assistant').click();
    const chatWindow = page.locator('.elitea-assistant-window');
    await expect(chatWindow).toBeVisible({ timeout: 15_000 });
    await expect(chatWindow.locator('.elitea-assistant-header-title')).toHaveText(ASSISTANT_NAME, {
      timeout: 20_000,
    });

    // An unsent draft — the discriminator: it exists only in the live `useChat`
    // instance, so it proves "the same session", not merely "a same-looking one".
    const draft = `${AUTOTEST_PREFIX}entrypoints draft ${RUN_ID}`;
    const input = chatWindow.locator('#elitea-assistant-message-input');
    await input.fill(draft);

    await chatWindow.locator('.elitea-assistant-header-close-action').click();
    await expect
      .poll(async () => page.locator('.elitea-assistant-window').count(), { timeout: 15_000 })
      .toBe(0);

    // Reopen from the OTHER entry point — the floating LAUNCHER — same page.
    const launcher = page.locator('.elitea-assistant-button');
    await expect(launcher).toBeVisible({ timeout: 20_000 });
    await launcher.click();

    const reopened = page.locator('.elitea-assistant-window');
    await expect(reopened).toBeVisible({ timeout: 15_000 });
    await expect(
      reopened.locator('#elitea-assistant-message-input'),
      'the draft must survive a close and reopening from the OTHER entry point',
    ).toHaveValue(draft, { timeout: 15_000 });
  });
});

/*
 * onetest: ELITEA-0623 — the Support Assistant's session should be shared
 * across every application page, not scoped to the page it was opened on.
 *
 * PRODUCT GAP. The draft above survives a close/reopen on the SAME page
 * (proved above), but does NOT survive a client-side navigation to a
 * DIFFERENT page: measured live, closing the widget, navigating to
 * `/app/pipelines`, and reopening from the launcher shows an EMPTY composer,
 * not the draft just typed — the widget's `useChat` state does not outlive
 * the page it was mounted under, contrary to the "global session" the
 * legacy case (and `SupportAssistantWidget.tsx`'s own module doc, "the
 * assistant widget wraps the sidebar") both describe.
 */
test('the session is shared across a page navigation, not scoped to one page', async ({ page }) => {
  test.setTimeout(60_000);
  await withPlatformFlagLock(async () => {
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
    await expect(chatWindow).toBeVisible({ timeout: 15_000 });

    const draft = `${AUTOTEST_PREFIX}crosspage draft ${RUN_ID}`;
    await chatWindow.locator('#elitea-assistant-message-input').fill(draft);
    await chatWindow.locator('.elitea-assistant-header-close-action').click();
    await expect
      .poll(async () => page.locator('.elitea-assistant-window').count(), { timeout: 15_000 })
      .toBe(0);

    await page.goto(`${BASE_URL}/app/pipelines`, { waitUntil: 'domcontentloaded' });
    await expect(launcher).toBeVisible({ timeout: 20_000 });
    await launcher.click();
    const reopened = page.locator('.elitea-assistant-window');
    await expect(reopened).toBeVisible({ timeout: 15_000 });

    test.fail(
      true,
      'ELITEA-0623: product gap — the widget draft/session does not survive a client-side ' +
        'navigation to a different page; it resets as if freshly mounted',
    );
    await expect(
      reopened.locator('#elitea-assistant-message-input'),
      'the draft must survive a page navigation',
    ).toHaveValue(draft, { timeout: 15_000 });
  });
});

/* onetest: ELITEA-0611 — the config route reports enabled/name and flips live with the admin toggle */
test('the config route reports the operator strings and the live enabled state', async ({
  request,
}) => {
  await withPlatformFlagLock(async () => {
    await disableSupportAssistant(request);
    const off = await request.get(`${BASE_URL}/api/v2/support_assistant/config`);
    expect(off.ok()).toBe(true);
    const offBody = (await off.json()) as { enabled?: boolean };
    expect(offBody.enabled).toBe(false);

    await enableSupportAssistant(request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentId),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME_MESSAGE,
      placeholder: PLACEHOLDER,
    });
    const on = await request.get(`${BASE_URL}/api/v2/support_assistant/config`);
    expect(on.ok()).toBe(true);
    const body = (await on.json()) as { enabled?: boolean; title?: string };
    expect(body.enabled).toBe(true);
    expect(body.title, 'the operator-configured name must reach the config response').toBe(ASSISTANT_NAME);
  });
});

/*
 * onetest: ELITEA-0648 — the Send button stays inactive while the message
 * input is empty, and neither a click nor Enter on an empty field submits
 * anything.
 *
 * No message is ever sent here: `MessageInput.tsx`'s own `handleSend` bails
 * out before calling `onSend` whenever `!trimmed || isSendDisabled`, so a
 * click on the disabled button and an Enter on an empty field are both
 * genuine no-ops — this stays inside the "never trigger a real send in the
 * chromium lane" boundary `support.widget.spec.ts`'s SUP-W4 states.
 */
test('the Send button stays inactive while the composer is empty', async ({ page }) => {
  await withPlatformFlagLock(async () => {
    await enableSupportAssistant(page.request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentId),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME_MESSAGE,
      placeholder: PLACEHOLDER,
    });
    await openApp(page);
    await page.locator('.elitea-assistant-button').click();
    const chatWindow = page.locator('.elitea-assistant-window');
    await expect(chatWindow).toBeVisible({ timeout: 15_000 });

    const send = chatWindow.locator('.elitea-assistant-send-button');
    const input = chatWindow.locator('#elitea-assistant-message-input');
    await expect(input).toBeVisible({ timeout: 15_000 });
    await expect(send, 'Send must start disabled on an empty composer').toBeDisabled();

    await input.press('Enter');
    await expect(chatWindow.locator('.elitea-assistant-message--user')).toHaveCount(0);

    await input.fill('A');
    await expect(send).toBeEnabled();

    await input.fill('');
    await expect(send).toBeDisabled();
  });
});
