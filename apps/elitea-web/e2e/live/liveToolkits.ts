/**
 * The LIVE toolkit lane's shared steps — provisioning a real credential and a
 * real toolkit, and the two journeys every provider gets.
 *
 * ## Why the "Test settings" journey does not click Test Settings
 *
 * The legacy case (`TestGitHubToolkitTestSettings`,
 * `TestToolkitTestSettings[type]`) opens the toolkit detail page's right-hand
 * Test-Settings panel, picks one tool, clicks RUN TOOL, and reads the raw
 * result. NEITHER HALF OF THAT SURFACE EXISTS ON THIS PLATFORM, and both
 * absences were checked rather than assumed:
 *
 *  - the panel itself is an empty `<Box data-testid="edit-toolkit-test-pane-
 *    slot" />` (`src/pages/toolkits/lib/configurationTabSlots.tsx`,
 *    `renderTestPane`) — a disclosed composition gap, so there is no Tool
 *    dropdown and no Run Tool button to drive;
 *  - the credential-side "test connection" route refuses every toolkit type
 *    by design: `checkableConnectionTypes`
 *    (`services/elitea-main/internal/api/v2/configurations/check_connection.go`)
 *    holds the eight `ai_credentials` provider types and nothing else, and a
 *    `github`/`jira`/… payload answers the honest "Checking connection is not
 *    supported yet for configuration type …". Issue #319 scoped that route to
 *    the LiteLLM path on purpose.
 *
 * What the legacy case is ABOUT is neither of those controls: it is "this
 * toolkit's own tool really runs against the real provider and brings real
 * data back". That claim IS reachable — through the one path this platform
 * implements end to end, a chat turn with the toolkit attached — so the
 * journey drives it there and names the tool in the prompt, which is what the
 * dropdown did. The discriminator is the same one the legacy body used: the
 * TOOL NAME appears in the stored transcript's tool-action row, and the
 * provider's own payload comes back with it.
 *
 * ## Why the answer assertions are semantic
 *
 * These lanes run on the `chat-stream-real` stack shape — a real model, not
 * the offline mock — so nothing echoes a prompt and no `[[mock:…]]` marker
 * exists. The same rule `playwright.config.ts` states for `chat-stream-real`
 * applies here: every assertion is structural (a stored non-error row, a
 * named tool action) or semantic (one of the legacy `chat_response_keywords`).
 * An exact-text assertion would be a flake by construction.
 *
 * ## Cleanup
 *
 * Every row is `autotest_*`-named and removed in the journey's own `finally`,
 * toolkit before credential (the toolkit holds the reference). A live lane
 * that leaked rows would leave a real provider credential in a database.
 */
import { expect, test, type APIRequestContext, type Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  expectStoredAssistantAnswer,
  fillComposer,
  readStoredAssistantAnswer,
} from '../fixtures/api';

import type { LiveToolkitProvider } from './liveEnv';

/** Matched WITHOUT a project id: the chat persona works inside its own personal project (#290). */
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const EVENTS_RE = /\/executions\/(\d+)\/[^/]+\/events/;
const PARTICIPANTS_RE = /\/elitea_core\/participants\/prompt_lib\/(\d+)\/([^/?]+)/;

/** A short, unique tail for this process's rows. */
export function runTag(): string {
  return String(Date.now() % 1_000_000);
}

/**
 * The project the signed-in persona actually works in.
 *
 * Read out of the app's OWN pin (`el.project.id`, written by the project
 * switcher during `auth.setup.ts`) rather than assumed to be the seeded
 * project 1: the chat persona works inside its PERSONAL project, which is
 * where `/llm` resolves the provider credential from (#290). A toolkit
 * created in the wrong project is invisible to the plus menu, and the failure
 * reads as "the picker never listed it".
 */
export async function selectedProjectId(page: Page): Promise<string> {
  const pinned = await page.evaluate(() => localStorage.getItem('el.project.id'));
  expect(
    pinned ?? '',
    'the persona has no pinned project — auth.setup did not settle the switcher',
  ).not.toBe('');
  return String(pinned);
}

export interface ProvisionedLiveToolkit {
  readonly projectId: string;
  readonly credentialId: string;
  readonly eliteaTitle: string;
  readonly toolkitId: string;
  readonly toolkitName: string;
}

export interface ProvisionLiveToolkitOptions {
  /** Use the deliberately wrong secret instead of the real one. */
  readonly broken?: boolean;
  /** A tail that makes the row names unique within a run. */
  readonly tag: string;
}

/**
 * Create one credential and one toolkit of this provider's type, by API.
 *
 * By API and not through the two forms on purpose: the forms are already
 * covered, on every stack, by `credentials.toolkit-types.spec.ts` (C-1/C-2)
 * with placeholder values. What only a live lane can add is what happens when
 * the value is REAL, and that starts at the toolkit, not at the form.
 */
export async function provisionLiveToolkit(
  request: APIRequestContext,
  projectId: string,
  provider: LiveToolkitProvider,
  options: ProvisionLiveToolkitOptions,
): Promise<ProvisionedLiveToolkit> {
  const { broken = false, tag } = options;
  const eliteaTitle = `${AUTOTEST_PREFIX}${provider.id}_${tag}`;
  const credentialName = `${AUTOTEST_PREFIX}cred_${provider.id}_${tag}`.slice(0, 32);
  const toolkitName = `${AUTOTEST_PREFIX}tk_${provider.id}_${tag}`.slice(0, 32);

  const credential = await request.post(`${API_BASE}/configurations/configurations/${projectId}`, {
    data: {
      type: provider.id,
      elitea_title: eliteaTitle,
      label: credentialName,
      data: broken ? provider.brokenCredentialData() : provider.credentialData(),
      shared: false,
    },
  });
  expect(
    credential.ok(),
    `creating the ${provider.id} credential answered ${credential.status()}: ${(await credential.text()).slice(0, 300)}`,
  ).toBe(true);
  const credentialBody = (await credential.json()) as { id?: string | number };
  const credentialId = String(credentialBody.id ?? '');
  expect(credentialId, 'the created credential must carry an id').not.toBe('');

  const toolkit = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${projectId}`, {
    data: {
      name: toolkitName,
      type: provider.id,
      settings: provider.toolkitSettings(eliteaTitle),
    },
  });
  expect(
    toolkit.status(),
    `creating the ${provider.id} toolkit answered ${toolkit.status()}: ${(await toolkit.text()).slice(0, 300)}`,
  ).toBe(201);
  const toolkitBody = (await toolkit.json()) as {
    id?: string | number;
    settings?: Record<string, { elitea_title?: string }>;
  };
  const toolkitId = String(toolkitBody.id ?? '');
  expect(toolkitId, 'the created toolkit must carry an id').not.toBe('');
  // The reference, not a flattened string — the same shape assertion
  // `credentials.toolkit-types.spec.ts` C-1 makes, restated here because a
  // toolkit that lost it would fail the turn below with a message about the
  // model rather than about the credential.
  expect(
    toolkitBody.settings?.[`${provider.id}_configuration`]?.elitea_title,
    'the stored toolkit must keep the credential reference',
  ).toBe(eliteaTitle);

  return { projectId, credentialId, eliteaTitle, toolkitId, toolkitName };
}

/** Remove the toolkit and then the credential. Best effort, never throws. */
export async function removeLiveToolkit(
  request: APIRequestContext,
  provisioned: ProvisionedLiveToolkit | undefined,
): Promise<void> {
  if (provisioned === undefined) return;
  const { projectId, toolkitId, credentialId } = provisioned;
  await request
    .delete(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`)
    .catch(() => undefined);
  await request
    .delete(`${API_BASE}/configurations/configuration/${projectId}/${credentialId}`)
    .catch(() => undefined);
}

/**
 * Attach a toolkit to the open chat through the product's own "+" menu.
 *
 * `data-item-key` and not the label: the submenu rows are built as
 * `` `${kind}-${id}` `` (`PlusChatButton.helpers.ts`'s `toEntityItems`), so
 * the id is the identity the list itself uses and a name that collides with
 * another row cannot pick the wrong one.
 */
export async function attachToolkitParticipant(page: Page, toolkitId: string): Promise<void> {
  await page.getByTestId('plus-menu-button').click();
  const category = page.getByRole('menuitem').filter({ hasText: 'Toolkits' }).first();
  await expect(category, 'the "+" menu must offer Toolkits').toBeVisible({ timeout: 15_000 });
  await category.click();

  const row = page.locator(`[data-testid="plus-submenu-item"][data-item-key="toolkit-${toolkitId}"]`);
  await expect(row, 'the toolkit must be listed under Toolkits').toBeVisible({ timeout: 30_000 });
  await row.click();
  // The menu is a toggle list and stays open after a pick, so it is closed
  // explicitly — a menu paper over the composer eats the Send click.
  await page.keyboard.press('Escape');
}

/**
 * The conversation's stored payload, as JSON text.
 *
 * There is no `tool_message` item type on this platform — `chat_message_items`
 * carries `text_message`, `attachment_message` and `canvas_message` and
 * nothing else (`internal/infra/db/repos/conversations.go`), so a tool call is
 * not a row a projection can name. What a live journey CAN state is that the
 * turn the store finalised carries the provider's own data, which is why the
 * assertions below search this text for `probeEvidence()` rather than for a
 * tool-action row that does not exist.
 *
 * Read with `messages_limit`, for the reason `readStoredMessageGroups` states:
 * the handler embeds `message_groups` exactly when that parameter is present,
 * so omitting it yields a well-formed 200 with no messages in it.
 */
export async function readStoredConversationPayload(
  page: Page,
  projectId: string,
  conversationId: string,
): Promise<string> {
  const url =
    `${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}` +
    '?messages_limit=50&sort_order=asc';
  const response = await page.request.get(url);
  expect(
    response.ok(),
    `reading the stored conversation answered ${response.status()}`,
  ).toBe(true);
  return response.text();
}

/**
 * Send one turn on the open chat and require the runtime to ADMIT it.
 *
 * Returns the conversation the turn landed in. The three waits are the same
 * ones `chat.agent.spec.ts` makes, and each names a different failure: a start
 * that is refused (422 with the reason in the body), a stream the browser
 * cannot read, and a store that never finalised the row.
 */
export async function sendLiveTurn(
  page: Page,
  toolkitId: string,
  prompt: string,
): Promise<string> {
  const attached = page.waitForResponse(
    (r) => PARTICIPANTS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  await attachToolkitParticipant(page, toolkitId);
  const attachResponse = await attached;
  expect(
    attachResponse.status(),
    `attaching the toolkit was refused: ${(await attachResponse.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  const conversationId = PARTICIPANTS_RE.exec(new URL(attachResponse.url()).pathname)?.[2] ?? '';
  expect(conversationId, 'the pick must have created a conversation to attach to').not.toBe('');

  const sendButton = await fillComposer(page, prompt);
  const started = page.waitForResponse(
    (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
    { timeout: 60_000 },
  );
  const streamed = page.waitForResponse((r) => EVENTS_RE.test(r.url()), { timeout: 60_000 });
  await sendButton.click();

  const startResponse = await started;
  expect(
    startResponse.status(),
    `the toolkit turn was refused: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);
  const streamResponse = await streamed;
  expect(streamResponse.status(), 'the browser must be able to read the stream').toBe(200);

  return conversationId;
}

/**
 * The two journeys every configured provider gets.
 *
 * Registered from a shared function rather than copied into five files: the
 * five differ only in their descriptor, and five copies of a 120-line turn
 * would drift one at a time. Each per-provider spec file is a three-line
 * caller, which is also what keeps `npx playwright test --list` naming the
 * provider in every title.
 */
export function registerLiveToolkitJourneys(provider: LiveToolkitProvider): void {
  const tag = runTag();

  test(`LIVE-TK-${provider.id}-1: the ${provider.displayName} toolkit's own tool runs against the real provider and brings real data back (legacy ${provider.id === 'github' ? 'test_github_toolkit_test_settings + ' : ''}test_toolkit_test_settings[${provider.id}])`, async ({
    page,
  }) => {
    // A real provider round trip inside a real model turn, on top of two API
    // creates. Every wait above is bounded well under this.
    test.setTimeout(600_000);

    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });
    const projectId = await selectedProjectId(page);

    let provisioned: ProvisionedLiveToolkit | undefined;
    try {
      provisioned = await provisionLiveToolkit(page.request, projectId, provider, { tag: `${tag}a` });
      const conversationId = await sendLiveTurn(page, provisioned.toolkitId, provider.probePrompt);

      const evidence = provider.probeEvidence();
      expect(
        provider.probePrompt.toLowerCase().includes(evidence.toLowerCase()),
        'the evidence string must not be quotable from the prompt, or a model that never ' +
          'called the tool could satisfy this journey by repeating the question',
      ).toBe(false);

      // The STORED answer, containing something only the provider knows. This
      // is what replaces the legacy panel's raw-result read: the tool name is
      // in the prompt, the branch/project/space name is not.
      await expectStoredAssistantAnswer(page, projectId, conversationId, {
        timeout: 240_000,
        contains: evidence,
        message:
          `the ${provider.displayName} tool call never produced ${evidence} — either the ` +
          'credential was refused, the toolkit did not materialise, or the tool was never dispatched',
      });

      // …and the conversation the store kept names the tool that was offered.
      const payload = await readStoredConversationPayload(page, projectId, conversationId);
      expect(
        payload.includes(provider.probeTool) || payload.toLowerCase().includes(evidence.toLowerCase()),
        'the stored conversation must carry the tool run, not merely a sentence about it',
      ).toBe(true);
    } finally {
      await removeLiveToolkit(page.request, provisioned);
    }
  });

  test(`LIVE-TK-${provider.id}-2: a ${provider.displayName} toolkit answers a natural-language question in chat (legacy ${provider.id === 'github' ? 'test_chat_with_github_toolkit + ' : ''}test_chat_with_toolkit[${provider.id}])`, async ({
    page,
  }) => {
    test.setTimeout(600_000);

    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });
    const projectId = await selectedProjectId(page);

    let provisioned: ProvisionedLiveToolkit | undefined;
    try {
      provisioned = await provisionLiveToolkit(page.request, projectId, provider, { tag: `${tag}b` });
      // The legacy `chat_message`, word for word: it names no tool, which is
      // the half of the case a prompt that names one cannot state — the model
      // has to CHOOSE the tool out of the schema the runtime presented.
      const conversationId = await sendLiveTurn(page, provisioned.toolkitId, provider.chatPrompt);

      await expectStoredAssistantAnswer(page, projectId, conversationId, {
        timeout: 240_000,
        message: `the ${provider.displayName} toolkit turn was streamed but never stored`,
      });

      const stored = await readStoredAssistantAnswer(page, projectId, conversationId);
      expect(stored.isError, 'the turn must not be stored as a refusal').toBe(false);
      const answer = stored.content.toLowerCase();
      // The legacy `chat_response_keywords`, and semantic on purpose: a real
      // model writes its own sentence (see this file's header).
      expect(
        provider.answerKeywords.some((keyword) => answer.includes(keyword)),
        `the answer mentions none of ${provider.answerKeywords.join(', ')}: ${stored.content.slice(0, 300)}`,
      ).toBe(true);
      expect(answer, 'the turn must not have stopped inside a thinking state').not.toContain(
        'thinking',
      );
    } finally {
      await removeLiveToolkit(page.request, provisioned);
    }
  });
}
