/**
 * The LIVE toolkit lane's shared steps — provisioning a real credential and a
 * real toolkit, and the two journeys every provider gets.
 *
 * ## The "Test settings" journey drives the Test-settings pane again
 *
 * The legacy case (`TestGitHubToolkitTestSettings`,
 * `TestToolkitTestSettings[type]`) opens the toolkit detail page's right-hand
 * Test-Settings panel, picks one tool, clicks RUN TOOL, and reads the raw
 * result. This file used to drive a CHAT TURN instead, and said why: the panel
 * was an empty `<Box data-testid="edit-toolkit-test-pane-slot" />`
 * (`src/pages/toolkits/lib/configurationTabSlots.tsx`, `renderTestPane`), so
 * there was no Tool dropdown and no Run Tool button to drive.
 *
 * That gap is closed. `features/toolkits/ui/test-tools/TestToolPane.tsx` is the
 * real panel — a tool picker, a schema-driven argument form, Run, and a result
 * panel — over the synchronous run route
 * `POST /elitea_core/test_tool/prompt_lib/{projectId}/{toolId}`. So the journey
 * drives the control the legacy case drove, and reads the RAW RESULT the legacy
 * case read (`test_tool_result_content`) rather than a model's sentence about
 * it.
 *
 * The discriminator is unchanged, and is still provider-only: `probeEvidence()`
 * is a string only the real provider can produce (a branch name, a project key,
 * a space key), and it appears in nothing this journey types. Reading it out of
 * the tool's OWN result is a stronger form of the same claim than reading it out
 * of a model's answer was — no model stands between the provider and the
 * assertion any more.
 *
 * `LIVE-TK-<provider>-2` still drives a chat turn, and should: it is the legacy
 * `test_chat_with_toolkit[type]` case, which is about a model CHOOSING the tool.
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

/** One settled run of the Test-settings pane. */
interface TestPaneOutcome {
  /** The result panel's own `data-status`: one branch of `TestToolkitToolOutcome`. */
  readonly status: string;
  /** The raw result payload, present only for an `ok` run. */
  readonly payload: string;
  /** Everything the panel rendered, for a failure message that says what happened. */
  readonly text: string;
}

/**
 * Open the toolkit's editor, pick its probe tool, fill what the tool requires,
 * press Run, and wait for the panel to settle.
 *
 * The wait is on the panel's `data-status` leaving `running` rather than on a
 * fixed delay: a real provider round trip inside a bounded server-side wait can
 * take tens of seconds, and a timeout here would report a slow provider as a
 * broken pane.
 */
async function runToolFromTestPane(
  page: Page,
  toolkitId: string,
  provider: LiveToolkitProvider,
): Promise<TestPaneOutcome> {
  await page.goto(`${BASE_URL}/app/toolkits/all/${toolkitId}`, { waitUntil: 'domcontentloaded' });

  const pane = page.getByTestId('edit-toolkit-test-pane-slot');
  await expect(pane).toBeAttached({ timeout: 60_000 });

  const picker = pane.getByRole('combobox').first();
  await expect(picker, 'the Test settings pane must offer a tool picker').toBeVisible({ timeout: 60_000 });
  await picker.click();
  // By the tool's own runtime name as the picker spells it: the label is the
  // name humanised (`toToolOption`, `TestToolSettings.tsx`).
  const option = page.getByRole('option', { name: new RegExp(provider.probeTool.replaceAll('_', '[ _]'), 'i') });
  await expect(option, `the picker must offer ${provider.probeTool}`).toBeVisible({ timeout: 30_000 });
  await option.click();

  for (const [field, value] of Object.entries(provider.probeToolArgs())) {
    const input = pane.getByLabel(new RegExp(field.replaceAll('_', '[ _]'), 'i')).first();
    await expect(input, `the argument form must offer ${field}`).toBeVisible({ timeout: 30_000 });
    await input.fill(value);
  }

  const runButton = pane.getByRole('button', { name: /run tool/i });
  await expect(runButton, 'Run Tool must be offered once the tool’s arguments are filled').toBeEnabled({ timeout: 30_000 });
  await runButton.click();

  const result = pane.getByTestId('test-tool-result');
  await expect(result).toBeVisible({ timeout: 60_000 });
  await expect
    .poll(async () => result.getAttribute('data-status'), {
      timeout: 300_000,
      message: 'the Test settings pane never settled on an outcome',
    })
    .not.toBe('running');

  const status = (await result.getAttribute('data-status')) ?? '';
  const payloadCell = pane.getByTestId('test-tool-result-payload');
  const payload = (await payloadCell.count()) > 0 ? await payloadCell.innerText() : '';
  return { status, payload, text: await result.innerText() };
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

  test(`LIVE-TK-${provider.id}-1: the ${provider.displayName} toolkit's own tool runs from the Test settings pane and brings real data back (legacy ${provider.id === 'github' ? 'test_github_toolkit_test_settings + ' : ''}test_toolkit_test_settings[${provider.id}])`, async ({
    page,
  }) => {
    // A real provider round trip on top of two API creates and one page load.
    // Every wait below is bounded well under this.
    test.setTimeout(420_000);

    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });
    const projectId = await selectedProjectId(page);

    let provisioned: ProvisionedLiveToolkit | undefined;
    try {
      provisioned = await provisionLiveToolkit(page.request, projectId, provider, { tag: `${tag}a` });

      const evidence = provider.probeEvidence();
      const typed = Object.values(provider.probeToolArgs()).join(' ').toLowerCase();
      expect(
        typed.includes(evidence.toLowerCase()),
        'the evidence string must not be typeable into the form, or a run that reached no ' +
          'provider could satisfy this journey with the value this journey supplied',
      ).toBe(false);

      const outcome = await runToolFromTestPane(page, provisioned.toolkitId, provider);

      // The RAW RESULT, exactly what the legacy panel read — and it carries
      // something only the provider knows.
      expect(
        outcome.status,
        `the ${provider.displayName} tool run settled as "${outcome.status}": ${outcome.text.slice(0, 300)}`,
      ).toBe('ok');
      expect(
        outcome.payload.toLowerCase().includes(evidence.toLowerCase()),
        `the ${provider.displayName} tool ran but returned nothing containing ${evidence}: ${outcome.payload.slice(0, 300)}`,
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
