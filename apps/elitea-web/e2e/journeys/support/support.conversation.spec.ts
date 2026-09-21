/**
 * WHAT A SUPPORT TURN DOES — the eight onetest cases of #939 group 7 that
 * need a real one (the SUPPORT-LANE rows of `ledger-Z2.tsv`).
 *
 * ELITEA-0612, 0614, 0615, 0627, 0639, 0642, 0645 and 0646 are all about the
 * widget while a turn is running or after one has run: what the client puts on
 * the wire, which agent answers, what the composer does while it waits, and
 * how the answer is rendered. None of them can be asked on a stack with no
 * worker, so this file runs in the `support-stack` project beside
 * `support.spec.ts` (`scripts/support-e2e.sh`).
 *
 * ## THE MOCK MODEL IS WHAT MAKES FOUR OF THEM ASSERTABLE
 *
 * `deploy/mock-llm/server.py` echoes the last user message back, prefixed, and
 * takes per-request MARKERS in the prompt. Three source cases are written
 * against a real assistant's prose ("response is visibly different", "the
 * response includes bold text", "the loading indicator is still visible") and
 * would be unassertable against it. The echo turns each into a fact:
 *
 *   - MARKDOWN (0642): send markdown, and the answer IS that markdown, so the
 *     rendered bubble can be read for `<strong>`, `<code>` and `<li>` instead
 *     of hoping a model produced some.
 *   - HTML/SCRIPT (0639): the same string comes back through the ASSISTANT
 *     renderer as well as the user bubble, so both directions of the
 *     sanitize-before-render boundary are exercised by one send.
 *   - LOCKED COMPOSER (0645): `[[mock:slow]]` holds the stream open for about
 *     twenty seconds (80 words, 250 ms apart), so "while it is generating" is
 *     a window a test can act inside rather than a race.
 *   - WHICH AGENT ANSWERED (0614, 0615): the reply is an echo and can never
 *     show the persona, but the mock's JOURNAL records the SYSTEM PROMPT of
 *     every request. Two agents with different instructions are therefore
 *     told apart by what the runtime SENT, which is the actual claim — "a
 *     visibly different answer" is a proxy for it and a weaker one.
 *
 * ## ONE AGENT PER CLAIM, AND THE SWITCH IS AN ADMIN WRITE
 *
 * The active support agent is one row of the platform-wide admin section, so
 * these tests are serial and restore the switch in `afterAll`, exactly as the
 * other two support files do.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  clearMockLlmJournal,
  deleteAgent,
  DEFAULT_PROJECT_ID,
  readMockLlmJournal,
} from '../../fixtures/api';

import { disableSupportAssistant, enableSupportAssistant, waitForSupportTurnSettled } from './helpers';
import type { RawConversationDetails } from './helpers';

const SUPPORT_PROJECT_ID = Number(DEFAULT_PROJECT_ID);
const RUN_ID = Date.now();

/**
 * The two agents the switch moves between. Their INSTRUCTIONS are the
 * discriminator: the mock journals the system prompt of every request, so a
 * turn names the agent that ran it even though the reply is an echo.
 */
const AGENT_A_NAME = `${AUTOTEST_PREFIX}support_agent_a_${RUN_ID}`;
const AGENT_B_NAME = `${AUTOTEST_PREFIX}support_agent_b_${RUN_ID}`;
const AGENT_A_MARKER = `AGENTAMARKER${RUN_ID}`;
const AGENT_B_MARKER = `AGENTBMARKER${RUN_ID}`;

/**
 * The model the two agents are PINNED to.
 *
 * Pinned, not inherited: a version with no model falls back to the project
 * default, and on this stack that default is the bare `E2E-MOCK-MODEL`, which
 * the gateway refuses with "could not auto resolve a provider for the request"
 * — the turn then answers an ERROR bubble and every assertion below would be
 * about that rather than about the case. The same `vllm/`-prefixed name every
 * streaming spec pins, for the same reason.
 */
const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

const WELCOME = `${AUTOTEST_PREFIX}Welcome. Ask me about ELITEA.`;
const PLACEHOLDER = `${AUTOTEST_PREFIX}Type your question...`;
const ASSISTANT_NAME = `${AUTOTEST_PREFIX}Support ${RUN_ID}`;

/** A settled turn's stored text, newest last. */
function storedTexts(details: RawConversationDetails): string[] {
  const texts: string[] = [];
  for (const group of details.message_groups ?? []) {
    for (const item of group.message_items ?? []) {
      if (item.item_type === 'text_message' && item.item_details?.content) {
        texts.push(item.item_details.content);
      }
    }
  }
  return texts;
}

test.describe('a support turn, end to end', () => {
  test.describe.configure({ mode: 'serial' });

  let agentA = '';
  let agentB = '';

  test.beforeAll(async ({ request }) => {
    const a = await createAgentWithVersion(request, AGENT_A_NAME, {
      instructions: `You are support agent A. ${AGENT_A_MARKER}`,
      model: { modelName: MOCK_MODEL },
    });
    const b = await createAgentWithVersion(request, AGENT_B_NAME, {
      instructions: `You are support agent B. ${AGENT_B_MARKER}`,
      model: { modelName: MOCK_MODEL },
    });
    agentA = a.id;
    agentB = b.id;
    await enableSupportAssistant(request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentA),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME,
      placeholder: PLACEHOLDER,
    });
  });

  test.afterAll(async ({ request }) => {
    await disableSupportAssistant(request).catch(() => {});
    if (agentA) await deleteAgent(request, agentA).catch(() => {});
    if (agentB) await deleteAgent(request, agentB).catch(() => {});
  });

  /** Land on the app and open the widget on a FRESH conversation. */
  async function openWidget(page: Page): Promise<ReturnType<Page['locator']>> {
    await page.goto(`${BASE_URL}/app/`, { waitUntil: 'domcontentloaded' });
    const launcher = page.locator('.elitea-assistant-button');
    await expect(launcher, 'the launcher must render while the assistant is on').toBeVisible({
      timeout: 30_000,
    });
    await launcher.click();
    const chatWindow = page.locator('.elitea-assistant-window');
    await expect(chatWindow).toBeVisible({ timeout: 15_000 });
    // A kept stack may already hold history for this session, and the widget
    // opens on the most recent conversation rather than on the welcome
    // screen when it has one.
    const newChat = chatWindow.getByRole('button', { name: 'New chat' });
    await expect(newChat).toBeEnabled({ timeout: 15_000 });
    await newChat.click();
    await expect(chatWindow.locator('.elitea-assistant-message--assistant')).toHaveCount(1, {
      timeout: 15_000,
    });
    return chatWindow;
  }

  /**
   * Send one message and wait for the ANSWER — a second assistant bubble.
   *
   * The count is the discriminator, not `.last()`: the welcome message is
   * itself a non-empty assistant bubble, so an assertion on the last one
   * passes the instant the panel opens whether or not a turn ever ran (the
   * measured trap `support.spec.ts` records).
   */
  async function sendAndWait(
    page: Page,
    chatWindow: ReturnType<Page['locator']>,
    text: string,
    options: { readonly expectedBubbles?: number; readonly timeout?: number } = {},
  ): Promise<void> {
    const { expectedBubbles = 2, timeout = 180_000 } = options;
    await chatWindow.locator('#elitea-assistant-message-input').fill(text);
    await chatWindow.locator('.elitea-assistant-send-button').click();
    await expect(chatWindow.locator('.elitea-assistant-message--assistant')).toHaveCount(expectedBubbles, {
      timeout,
    });
  }

  /** The uuid of the conversation the widget created on its first send. */
  function armConversationCreated(page: Page): Promise<string> {
    return page
      .waitForResponse(
        (response) =>
          response.url().includes('/support_assistant/conversations') &&
          response.request().method() === 'POST',
        { timeout: 60_000 },
      )
      .then(async (response) => {
        const body = (await response.json()) as { uuid?: string };
        return body.uuid ?? '';
      });
  }

  /*
   * onetest: ELITEA-0646 — Enter sends the message, with the same outcome as
   * the Send button: the question becomes a user bubble, the input clears,
   * the assistant answers, and the composer comes back.
   */
  test('SUP-C1: pressing Enter sends the message and the answer arrives', async ({ page }) => {
    test.setTimeout(240_000);
    const chatWindow = await openWidget(page);
    const question = `${AUTOTEST_PREFIX}enter sends ${RUN_ID}`;

    const input = chatWindow.locator('#elitea-assistant-message-input');
    await input.fill(question);
    await expect(chatWindow.locator('.elitea-assistant-send-button')).toBeEnabled();
    await input.press('Enter');

    await expect(chatWindow.locator('.elitea-assistant-message--user').last()).toContainText(question);
    await expect(input, 'sending must clear the composer').toHaveValue('');
    await expect(chatWindow.locator('.elitea-assistant-message--assistant')).toHaveCount(2, {
      timeout: 180_000,
    });
    await expect(input, 'the composer must be interactive again once the answer has landed').toBeEnabled({
      timeout: 30_000,
    });
  });

  /*
   * onetest: ELITEA-0612 — the client sends NO project_id, participant_id or
   * agent_id on any support call. Those are resolved server-side from the
   * admin section (`requireSupportProject`), and a client that sent its own
   * would be choosing the tenant a support turn runs in.
   *
   * Read off the REQUESTS the browser really made, not off the source: a
   * field added by a later refactor would pass a source review and fail here.
   */
  test('SUP-C2: no support request carries a project, participant or agent id', async ({ page }) => {
    test.setTimeout(240_000);
    const bodies: { url: string; payload: string }[] = [];
    page.on('request', (request) => {
      if (request.method() !== 'POST' || !request.url().includes('/support_assistant/')) return;
      bodies.push({ url: request.url(), payload: request.postData() ?? '' });
    });

    const chatWindow = await openWidget(page);
    await sendAndWait(page, chatWindow, `${AUTOTEST_PREFIX}request shape ${RUN_ID}`);

    expect(
      bodies.map((entry) => entry.url).join('\n'),
      'the widget must have created a conversation and asked for a prediction',
    ).toMatch(/support_assistant\/(conversations|predict)/);
    const creates = bodies.filter((entry) => entry.url.includes('/support_assistant/conversations'));
    const predicts = bodies.filter((entry) => entry.url.includes('/support_assistant/predict'));
    expect(creates.length, 'the conversation POST must have been observed').toBeGreaterThan(0);
    expect(predicts.length, 'the message POST must have been observed').toBeGreaterThan(0);

    for (const entry of bodies) {
      const parsed = entry.payload === '' ? {} : (JSON.parse(entry.payload) as Record<string, unknown>);

      // THE TOP LEVEL is where a routing id would be: those are the fields the
      // handler would read to decide what runs and for whom. None of the three
      // may be there, on any support call.
      for (const forbidden of ['project_id', 'participant_id', 'agent_id']) {
        expect(
          Object.keys(parsed),
          `${entry.url} carried a top-level \`${forbidden}\` — the support surface resolves the project, ` +
            'the participant and the agent server-side, and a client that sends one is choosing which ' +
            'tenant a support turn runs in',
        ).not.toContain(forbidden);
      }

      // `participant_id` and `agent_id` may not appear ANYWHERE, nested or
      // not: neither is a thing a user looks at, so there is no honest reason
      // for the client to know one.
      for (const forbidden of ['participant_id', 'agent_id']) {
        expect(entry.payload, `${entry.url} carried \`${forbidden}\` somewhere in its body`).not.toContain(
          forbidden,
        );
      }

      // `project_id` INSIDE `support_assistant_context` is a different thing
      // and is deliberately kept: that block is the PAGE CONTEXT the widget
      // collects — which screen the user is on and which project they are
      // looking at (`AssistantContext`, `predict.go`) — and the server fences
      // it into the question rather than routing on it. Measured on the first
      // run of this test, which is why the assertion above is scoped to the
      // top level instead of to the whole payload.
      const context = parsed['support_assistant_context'];
      if (context !== undefined) {
        expect(
          Object.keys(context as Record<string, unknown>),
          'the page-context block may describe the project the user is looking at, and nothing else ' +
            'that identifies an agent or a participant',
        ).not.toContain('agent_id');
      }
    }
  });

  /*
   * onetest: ELITEA-0645 — while the assistant is generating, the composer is
   * locked: no typing, no second send by button or by Enter, and exactly one
   * turn in flight. Then it comes back.
   *
   * `[[mock:slow]]` is what makes "while it is generating" a window rather
   * than a race (see the file header).
   */
  test('SUP-C3: the composer is locked while the assistant generates, and restored after', async ({
    page,
  }) => {
    test.setTimeout(300_000);
    const chatWindow = await openWidget(page);
    const input = chatWindow.locator('#elitea-assistant-message-input');
    const send = chatWindow.locator('.elitea-assistant-send-button');

    await input.fill(`${AUTOTEST_PREFIX}slow question ${RUN_ID} [[mock:slow]]`);
    await send.click();

    // The lock is the textarea's own `disabled` attribute
    // (`MessageInput.tsx`: `disabled={isLoading || isStreaming}`), which is
    // what makes "no text can be entered" true of a real keyboard and not
    // only of `fill()`.
    await expect(input, 'the composer must lock while the turn is open').toBeDisabled({ timeout: 60_000 });
    await expect(send, 'the send control must lock with it').toBeDisabled();

    const userBubbles = chatWindow.locator('.elitea-assistant-message--user');
    await expect(userBubbles).toHaveCount(1);
    // Enter and a click while locked must produce NOTHING — no second user
    // bubble, no second turn.
    await input.press('Enter', { force: true }).catch(() => {});
    await send.click({ force: true }).catch(() => {});
    await expect(userBubbles, 'a locked composer must not be able to send a second message').toHaveCount(1);

    await expect(chatWindow.locator('.elitea-assistant-message--assistant')).toHaveCount(2, {
      timeout: 240_000,
    });
    await expect(input, 'the composer must be interactive again once the turn has finished').toBeEnabled({
      timeout: 60_000,
    });
    await expect(input).toHaveValue('');
  });

  /*
   * onetest: ELITEA-0639 — HTML and script-like content is never executed.
   *
   * BOTH directions are exercised by one send, because the mock echoes: the
   * string is rendered once by the USER bubble (React text, escaped) and once
   * by the ASSISTANT bubble, which goes through `shared/ui/Markdown` — this
   * app's single sanitize-before-render boundary. The assertion on the
   * assistant side is therefore "nothing executed and no <script> element
   * survives", not "the raw string is shown verbatim": the renderer
   * deliberately renders SAFE html, and asserting otherwise would pin a
   * behaviour the product does not have.
   */
  test('SUP-C4: script-like content is neither executed nor injected', async ({ page }) => {
    test.setTimeout(240_000);
    const dialogs: string[] = [];
    page.on('dialog', (dialog) => {
      dialogs.push(dialog.message());
      void dialog.dismiss();
    });

    const chatWindow = await openWidget(page);
    const payload = `${AUTOTEST_PREFIX}<script>alert('XSS')</script> <b>bold</b> <img src=x onerror=alert(1)> Hello <World> & "Test" 🎉`;
    await sendAndWait(page, chatWindow, payload);

    // The user's own bubble shows what they typed, character for character.
    const userBubble = chatWindow.locator('.elitea-assistant-message--user').last();
    await expect(userBubble, 'the user bubble must show the typed text literally').toContainText(
      "<script>alert('XSS')</script>",
    );
    await expect(userBubble, 'angle brackets, ampersands, quotes and emoji must survive').toContainText(
      'Hello <World> & "Test" 🎉',
    );

    // Nothing executed, on either side.
    expect(dialogs, 'a dialog was raised — script content reached the page as script').toEqual([]);
    expect(
      await chatWindow.locator('script').count(),
      'a <script> element survived into the widget',
    ).toBe(0);
    const injected = await page.evaluate(() => (window as unknown as Record<string, unknown>)['__pwned']);
    expect(injected, 'the payload executed and wrote to window').toBeUndefined();
  });

  /*
   * onetest: ELITEA-0642 — markdown in an answer renders as formatted
   * content: bold as bold, inline code as code, a list as list items. The
   * echo is what puts known markdown in the ANSWER (see the file header).
   */
  test('SUP-C5: markdown in the answer renders as formatted content', async ({ page }) => {
    test.setTimeout(240_000);
    const chatWindow = await openWidget(page);
    const marker = `${AUTOTEST_PREFIX}md${RUN_ID}`;
    await sendAndWait(
      page,
      chatWindow,
      `${marker} **bold text** and \`inline_code\` and a list:\n\n- first item\n- second item`,
    );

    const answer = chatWindow.locator('.elitea-assistant-message--assistant').last();
    await expect(answer).toContainText(marker, { timeout: 60_000 });
    await expect(answer.locator('strong'), 'bold must render as bold, not as asterisks').toHaveCount(1);
    await expect(answer.locator('strong')).toHaveText('bold text');
    await expect(answer.locator('code'), 'inline code must render as code, not as backticks').toHaveCount(1);
    await expect(answer.locator('code')).toHaveText('inline_code');
    await expect(answer.locator('li'), 'a bullet list must render as list items').toHaveCount(2);
    await expect(answer, 'the raw markdown must not be shown as text').not.toContainText('**bold text**');
  });

  /*
   * onetest: ELITEA-0627 — a message WITH a file attachment produces a valid
   * answer and both halves are in the stored history afterwards. The stored
   * read is the assertion the case's last step asks for ("open the chat
   * history and locate the conversation"), and it is what a DOM-only check
   * could not give: a rendered bubble proves the client drew something, not
   * that the turn was persisted.
   */
  test('SUP-C6: a message with an attachment answers, and both halves are stored', async ({ page }) => {
    test.setTimeout(300_000);
    const chatWindow = await openWidget(page);

    const attach = chatWindow.getByRole('button', { name: 'Attach a file' });
    await expect(attach).toBeEnabled({ timeout: 20_000 });
    const [chooser] = await Promise.all([
      page.waitForEvent('filechooser', { timeout: 20_000 }),
      attach.click(),
    ]);
    const fileName = `${AUTOTEST_PREFIX}support_note_${RUN_ID}.txt`;
    await chooser.setFiles([
      { name: fileName, mimeType: 'text/plain', buffer: Buffer.from('autotest support attachment\n') },
    ]);
    await expect(chatWindow.locator('.elitea-assistant-file-chip')).toHaveCount(1, { timeout: 15_000 });

    const question = `${AUTOTEST_PREFIX}please review this file ${RUN_ID}`;
    const created = armConversationCreated(page);
    await sendAndWait(page, chatWindow, question, { timeout: 240_000 });
    const conversationUuid = await created;
    expect(conversationUuid, 'the widget must create a conversation before it can start a turn').toBeTruthy();

    const answer = chatWindow.locator('.elitea-assistant-message--assistant').last();
    await expect(answer, 'an attachment turn must not answer with an error').not.toHaveClass(
      /elitea-assistant-message--error/,
    );

    const details = await waitForSupportTurnSettled(page.request, conversationUuid, { timeout: 240_000 });
    const texts = storedTexts(details);
    expect(
      texts.some((text) => text.includes(question)),
      'the question is not in the stored transcript',
    ).toBe(true);
    // TWO stored texts at least — the question and the answer. The answer is
    // NOT identified by the absence of the fenced page-context block: the mock
    // echoes the whole user message, fence included, so filtering on it
    // removes the answer as well (measured on the first run of this test).
    expect(texts.length, 'the transcript holds fewer than two texts — one half of the turn is missing').
      toBeGreaterThanOrEqual(2);
    // And the turn settled WITHOUT an error, which is the case's "bot returns
    // a valid response. No socket error is displayed".
    const settled = (details.message_groups ?? []).filter(
      (group) => group.meta !== undefined && 'is_error' in group.meta,
    );
    expect(settled.length, 'no message group carries a finalising flag').toBeGreaterThan(0);
    expect(
      settled.some((group) => group.meta?.['is_error'] === false),
      'every settled group of an attachment turn is flagged as an error',
    ).toBe(true);
  });

  /*
   * onetest: ELITEA-0615 — the message is processed by the agent that is
   * CONFIGURED NOW. Asserted on the mock's journal, which records the system
   * prompt of every request: agent A's instructions before the switch, agent
   * B's after it, in a new session.
   */
  test('SUP-C7: a new session is answered by the agent configured at that moment', async ({ page }) => {
    test.setTimeout(300_000);
    await clearMockLlmJournal(page);

    let chatWindow = await openWidget(page);
    await sendAndWait(page, chatWindow, `${AUTOTEST_PREFIX}who are you A ${RUN_ID}`);

    const afterA = await readMockLlmJournal(page);
    expect(
      afterA.some((entry) => entry.instructions.includes(AGENT_A_MARKER)),
      'the turn did not carry agent A’s instructions — the configured agent is not the one that ran',
    ).toBe(true);
    expect(
      afterA.some((entry) => entry.instructions.includes(AGENT_B_MARKER)),
      'agent B answered while agent A was configured',
    ).toBe(false);

    await enableSupportAssistant(page.request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentB),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME,
      placeholder: PLACEHOLDER,
    });
    await clearMockLlmJournal(page);

    chatWindow = await openWidget(page);
    await sendAndWait(page, chatWindow, `${AUTOTEST_PREFIX}who are you B ${RUN_ID}`);

    const afterB = await readMockLlmJournal(page);
    expect(
      afterB.some((entry) => entry.instructions.includes(AGENT_B_MARKER)),
      'the new session did not carry agent B’s instructions after the switch',
    ).toBe(true);
    expect(
      afterB.some((entry) => entry.instructions.includes(AGENT_A_MARKER)),
      'the superseded agent answered after the switch',
    ).toBe(false);
  });

  /*
   * onetest: ELITEA-0614 — switching the agent while a conversation is OPEN
   * takes effect on the NEXT message, and that message answers normally
   * rather than erroring. The case's own expectation is "a valid response is
   * received; no error shown in the chat"; the journal then says which agent
   * it was, which is the part the case words as "reflects Agent-B's
   * behaviour".
   */
  test('SUP-C8: switching the agent mid-conversation is picked up by the next message', async ({ page }) => {
    test.setTimeout(300_000);
    // Start from A, whatever the previous test left configured.
    await enableSupportAssistant(page.request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentA),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME,
      placeholder: PLACEHOLDER,
    });

    const chatWindow = await openWidget(page);
    const created = armConversationCreated(page);
    await sendAndWait(page, chatWindow, `${AUTOTEST_PREFIX}first question ${RUN_ID}`);
    const conversationUuid = await created;
    expect(conversationUuid).toBeTruthy();

    // WAIT FOR THE FIRST TURN TO SETTLE IN THE STORE before switching.
    // Without it the switch, and the second send behind it, race the first
    // turn's own stream: measured once under host load, where the first
    // answer never painted and the test failed on a precondition rather than
    // on the case. The store is the authoritative signal that the turn is
    // over; the bubble is the client's view of it.
    await waitForSupportTurnSettled(page.request, conversationUuid, { timeout: 180_000 });

    // The switch, with the same conversation still open on screen.
    await enableSupportAssistant(page.request, {
      projectId: SUPPORT_PROJECT_ID,
      agentId: Number(agentB),
      name: ASSISTANT_NAME,
      welcomeMessage: WELCOME,
      placeholder: PLACEHOLDER,
    });
    await clearMockLlmJournal(page);

    // The SECOND message of the SAME conversation: three assistant bubbles
    // now (welcome, first answer, second answer).
    await sendAndWait(page, chatWindow, `${AUTOTEST_PREFIX}second question ${RUN_ID}`, {
      expectedBubbles: 3,
    });
    const answer = chatWindow.locator('.elitea-assistant-message--assistant').last();
    await expect(answer, 'the message after the switch must not error').not.toHaveClass(
      /elitea-assistant-message--error/,
    );
    await expect(answer).toContainText(/\S/);

    const journal = await readMockLlmJournal(page);
    expect(
      journal.some((entry) => entry.instructions.includes(AGENT_B_MARKER)),
      'the message after the switch was not answered by the newly configured agent',
    ).toBe(true);

    // The conversation kept both exchanges: a switch must not orphan the
    // session it was made during.
    const details = await waitForSupportTurnSettled(page.request, conversationUuid, { timeout: 240_000 });
    const texts = storedTexts(details);
    expect(
      texts.some((text) => text.includes(`${AUTOTEST_PREFIX}first question ${RUN_ID}`)),
      'the first question is gone from the conversation the switch happened in',
    ).toBe(true);
    expect(
      texts.some((text) => text.includes(`${AUTOTEST_PREFIX}second question ${RUN_ID}`)),
      'the question sent after the switch is not in the stored transcript',
    ).toBe(true);
  });
});
