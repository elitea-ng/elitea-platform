/**
 * Speaking Mode — `features/chat-input/lib/hooks/useSpeakingModeLoop.ts`, the
 * hands-free voice loop wired through `widgets/chat`'s `SendButton` (the
 * wave-icon strip that replaces Send while the composer is empty). Ported by
 * use case from the onetest voice package (`voice/speaking-mode`).
 *
 * ## What can be asserted on this stack
 *
 * A real turn (an AI answer, TTS auto-read of that answer) needs a worker and
 * a model backend this stack's `journeys` project does not have — the same
 * boundary `support.widget.spec.ts` states for the Support Assistant widget.
 * What IS testable without one: the auto-send mechanism itself (silence
 * timer, manual-edit reset, mid-sentence pause tolerance) fires a REAL send —
 * `sendQuestion()` creates a genuine conversation row, the same one
 * `chat.composer.spec.ts`'s Shift+Enter test reads back over the API without
 * ever waiting for an assistant reply — and the state Speaking Mode is in
 * the MOMENT that happens (recording paused, mode still on) is a client fact.
 * TTS-dependent cases (auto-read, TTS interruption, resuming after TTS) are
 * therefore out of scope here — see the ledger for the stream-deferred list.
 *
 * ## The stub
 *
 * Same `webkitSpeechRecognition` replacement `chat.voice-input.spec.ts` uses,
 * duplicated rather than imported: these are sibling spec files, not a shared
 * module, and the PREAMBLE reserves `e2e/fixtures/*` for helpers multiple
 * areas need.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';
import {
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  clickCreateButton,
  createConversation,
  deleteConversation,
} from '../../fixtures/api';

async function stubSpeechRecognition(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const w = window as unknown as { __autotestRecognitions: unknown[] };
    w.__autotestRecognitions = [];
    class AutotestSpeechRecognition {
      continuous = false;
      interimResults = false;
      lang = 'en-US';
      onresult: ((event: unknown) => void) | null = null;
      onerror: ((event: unknown) => void) | null = null;
      onend: (() => void) | null = null;
      constructor() {
        w.__autotestRecognitions.push(this);
      }
      start(): void {}
      stop(): void {
        // A real engine fires no more events once stopped; detaching the
        // handler here matches that instead of letting a manual
        // fireTranscript() reach a "stopped" instance.
        this.onresult = null;
      }
      abort(): void {
        this.onresult = null;
      }
    }
    // Both names: this Chromium build exposes an unprefixed
    // `window.SpeechRecognition` natively, and the hook prefers it over
    // `webkitSpeechRecognition` — see `chat.voice-input.spec.ts`'s sibling
    // comment, measured the same way.
    Object.defineProperty(window, 'SpeechRecognition', {
      configurable: true,
      value: AutotestSpeechRecognition,
    });
    Object.defineProperty(window, 'webkitSpeechRecognition', {
      configurable: true,
      value: AutotestSpeechRecognition,
    });
  });
}

async function fireTranscript(page: Page, transcript: string, isFinal = true): Promise<void> {
  await page.evaluate(
    ({ transcript, isFinal }) => {
      interface Rec {
        onresult: ((event: unknown) => void) | null;
      }
      const list = (window as unknown as { __autotestRecognitions: Rec[] }).__autotestRecognitions;
      const rec = list[list.length - 1];
      rec?.onresult?.({
        resultIndex: 0,
        results: [{ isFinal, length: 1, 0: { transcript } }],
      });
    },
    { transcript, isFinal },
  );
}

const speakingModeButton = (page: Page) => page.getByTestId('chat-speaking-mode-button');
/**
 * The strip's Stop control (`SendButton.tsx`'s speaking-mode branch).
 *
 * Unlike `MicControl`/`SendControl` in the same file, this `IconButton` is
 * `Tooltip`'s DIRECT child (no wrapping `<span>`), so MUI clones the
 * button itself and `aria-label="Stop speaking"` lands there (verified live —
 * a `span[aria-label=...]` wrapper, the right shape for the file's OTHER two
 * controls, matches nothing here).
 */
const stopSpeakingButton = (page: Page) => page.getByRole('button', { name: 'Stop speaking' });

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}voice_${tag}_${Date.now()}`;
}

const CONVERSATIONS_PATH = `/elitea_core/conversations/prompt_lib/${DEFAULT_PROJECT_ID}`;

test.beforeEach(async ({ page }) => {
  await stubSpeechRecognition(page);
});

/* onetest: ELITEA-1286 — Speaking Mode resets to disabled after a full page refresh */
test('Speaking Mode does not survive a page refresh', async ({ page }) => {
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  await expect(speakingModeButton(page)).toBeEnabled({ timeout: 20_000 });
  await speakingModeButton(page).click();
  await expect(stopSpeakingButton(page)).toBeVisible();

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  // Back to the idle wave icon, not the "Speaking..." strip — component
  // state, not a persisted preference.
  await expect(speakingModeButton(page)).toBeVisible({ timeout: 20_000 });
  await expect(stopSpeakingButton(page)).toHaveCount(0);
});

/*
 * onetest: ELITEA-1281 — disabling Speaking Mode stops all associated
 * functionality: voice is no longer captured, and typing + Send still work
 * normally afterwards. (The "TTS does NOT auto-read" and "no orphaned
 * websocket" halves of the legacy case need a real answer / DevTools network
 * inspection respectively and are not asserted here.)
 */
test('disabling Speaking Mode stops voice capture; typed Send keeps working', async ({ page }) => {
  const name = uniqueName('disable');
  const conversationId = await createConversation(page.request, name);
  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`, { waitUntil: 'domcontentloaded' });
    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });

    await speakingModeButton(page).click();
    await expect(stopSpeakingButton(page)).toBeVisible();
    await stopSpeakingButton(page).click();
    await expect(speakingModeButton(page)).toBeVisible({ timeout: 10_000 });

    // Speaking into the (stubbed) engine now reaches nobody: the loop tore
    // its recognition down when the mode turned off.
    await fireTranscript(page, 'should not appear');
    await expect(input).toHaveValue('');

    // Ordinary typed Send still works.
    const text = uniqueName('typed');
    await input.fill(text);
    await page.getByTestId('chat-send-button').click();
    await expect(page.getByTestId('user-message').last()).toContainText(text, { timeout: 15_000 });
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

/*
 * onetest: ELITEA-1294 — Speaking Mode is available in a brand-new, empty
 * chat, and its first utterance auto-sends.
 *
 * Closed in #930/#931. `useSpeakingModeLoop.ts` used to wire `scheduleSend`
 * from exactly two places: `handleTranscriptDone` (the SERVER/streaming-ASR
 * hook's callback) and `notifyManualEdit` (a real DOM edit) — the CLIENT
 * hook (`useSpeechRecognition`, the one this stack uses with no ASR model
 * configured) was given only `onTranscript`, so a spoken utterance alone
 * never armed the silence timer. A FINAL result now arms it on that path
 * (the browser fallback emits no `transcript_done` of its own). #931 was the
 * other half: the scheduled `sendQuestion()` was swallowed because
 * `NewChatInput` folded the recording flag into `UserInput`'s `disabledSend`
 * PROP, which `useUserInputSendQuestion` guards on — so the loop's `reset()`
 * cleared the composer and nothing was posted. The recording flag now
 * disables the send CONTROL only.
 */
test('Speaking Mode works in a brand new, empty chat and its first utterance auto-sends', async ({
  page,
}) => {
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await clickCreateButton(page);
  await expect(page.getByTestId('chat-message-input')).toHaveValue('', { timeout: 15_000 });
  await expect(page.getByTestId('user-message')).toHaveCount(0);

  await expect(speakingModeButton(page)).toBeEnabled({ timeout: 20_000 });
  await speakingModeButton(page).click();
  await expect(stopSpeakingButton(page)).toBeVisible();

  const text = uniqueName('first');
  await fireTranscript(page, text);


  const created = page.waitForResponse(
    (response) => response.url().includes(CONVERSATIONS_PATH) && response.request().method() === 'POST',
    { timeout: 15_000 },
  );
  const response = await created;
  expect(response.status()).toBe(201);
  const body = (await response.json()) as { id?: string; name?: string };
  expect(body.id).toBeTruthy();

  await expect(page.getByTestId('user-message').first()).toContainText(text, { timeout: 15_000 });

  await deleteConversation(page.request, body.id as string);
});

/* onetest: ELITEA-1298 — Speaking Mode auto-sends after the user stops speaking, with no manual Send click */
test('a message auto-sends after silence, with no click on Send', async ({ page }) => {
  const name = uniqueName('autosend');
  const conversationId = await createConversation(page.request, name);
  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`, { waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 20_000 });

    await speakingModeButton(page).click();
    const text = uniqueName('spoken');
    await fireTranscript(page, text);

    // No Send click anywhere in this test — the auto-send timer is what must
    // fire it. `SILENCE_TIMEOUT_MS` is 3000ms + a latency estimate.
    await expect(page.getByTestId('user-message').last(), 'silence must auto-send with no Send click').toContainText(
      text,
      { timeout: 15_000 },
    );
    await expect(page.getByTestId('chat-message-input')).toHaveValue('');
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

/*
 * onetest: ELITEA-1291, ELITEA-1299 — a natural mid-sentence pause does not
 * trigger a premature send (both sentences arrive as one message), and along
 * the way this is also the Chat half of ELITEA-1299's core claim: clicking
 * Speaking Mode activates capture and live speech is transcribed into the
 * input field (the two `fireTranscript` calls below and the `toHaveValue`
 * check ARE that transcription, mid-test). Agent/Pipeline pages are not
 * independently exercised — see the ledger.
 *
 * Both halves are real now (#930): the pause does not send early AND the
 * full utterance sends as one message, because a FINAL transcript re-arms
 * the silence timer rather than adding a second send.
 */
test('a mid-sentence pause does not auto-send early; the full utterance sends as one message', async ({
  page,
}) => {
  const name = uniqueName('pause');
  const conversationId = await createConversation(page.request, name);
  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`, { waitUntil: 'domcontentloaded' });
    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });

    await speakingModeButton(page).click();
    await fireTranscript(page, 'Tell me about computers.');

    // A pause shorter than the 3s silence timeout — must NOT have sent yet.
    await page.waitForTimeout(1_200);
    await expect(page.getByTestId('user-message')).toHaveCount(0);

    await fireTranscript(page, 'And include milestones.');
    await expect(input).toHaveValue('Tell me about computers. And include milestones.');

    await expect(page.getByTestId('user-message').last()).toContainText(
      'Tell me about computers. And include milestones.',
      { timeout: 15_000 },
    );
    // Exactly one message, not two.
    await expect(page.getByTestId('user-message')).toHaveCount(1);
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

/*
 * onetest: ELITEA-1290 — manually editing the transcribed text resets the
 * auto-send timer, so the edit is not cut off.
 *
 * Closed in #931, a different defect from #930's missing wiring.
 * `notifyManualEdit` IS wired to every composer keystroke
 * (`useNewChatInputInputChange`) and does call `scheduleSend` — but the
 * imperative `sendQuestion()` that fired when the timer elapsed was
 * swallowed by `useUserInputSendQuestion`'s `disabledSend` guard, which
 * `NewChatInput` had made true for the whole of a recording. The loop's
 * unconditional `reset()` then cleared the composer, so the edit was lost
 * with no request ever made. `UserInput` now applies the recording flag to
 * the send CONTROL, not to the send itself.
 */
test('a manual edit while Speaking Mode is pending resets the auto-send timer', async ({ page }) => {
  const name = uniqueName('edit');
  const conversationId = await createConversation(page.request, name);
  try {
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`, { waitUntil: 'domcontentloaded' });
    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });

    await speakingModeButton(page).click();
    await fireTranscript(page, 'Schedule a meeting for tomorrow afternoon');
    await expect(input).toHaveValue('Schedule a meeting for tomorrow afternoon');

    // Edit BEFORE the 3s timer would fire — this keystroke must reset it.
    await page.waitForTimeout(1_500);
    await input.click();
    await input.press('End');
    await input.pressSequentially(' at 2pm');
    await expect(input).toHaveValue('Schedule a meeting for tomorrow afternoon at 2pm');

    // Must not have sent yet, immediately after the edit.
    await expect(page.getByTestId('user-message')).toHaveCount(0);

    await expect(page.getByTestId('user-message').last()).toContainText(
      'Schedule a meeting for tomorrow afternoon at 2pm',
      { timeout: 15_000 },
    );
  } finally {
    await deleteConversation(page.request, conversationId);
  }
});

/* onetest: ELITEA-1285 — switching to another browser tab and back does not disable Speaking Mode */
test('switching tabs and back does not disable Speaking Mode', async ({ page, context }) => {
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });

  await expect(speakingModeButton(page)).toBeEnabled({ timeout: 20_000 });
  await speakingModeButton(page).click();
  await expect(stopSpeakingButton(page)).toBeVisible();

  // A second tab, brought to the front — this page goes into the background
  // the same way a real tab switch does.
  const other = await context.newPage();
  await other.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await other.waitForTimeout(2_000);
  await page.bringToFront();

  await expect(stopSpeakingButton(page), 'Speaking Mode must still show ON after the tab switch').toBeVisible();
  await fireTranscript(page, 'still listening after the switch');
  await expect(page.getByTestId('chat-message-input')).toHaveValue('still listening after the switch');

  await other.close();
});

/*
 * onetest: ELITEA-1341 — regression test for #5235: the Voice Mini Player / read-out controls must
 * NOT be visible in a default chat conversation before Voice/Read-out has been explicitly activated.
 *
 * The onetest source's precondition is a completed AI response; this file's own header states why
 * that never happens on this stack's `journeys` project (no worker). The claim asserted here is
 * STRICTER, not weaker: the player must be absent on load AND after a real send that this deployment
 * cannot answer — a build that renders it only once a response streams in would still fail the
 * moment one eventually arrived, so proving it is absent with no response yet in flight is the
 * meaningful, checkable half of the regression.
 */
test('the Voice Mini Player is not rendered by default in a chat conversation (regression #5235)', async ({
  page,
}) => {
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
  await expect(page.getByTestId('chat-voice-mini-player')).toHaveCount(0);

  const text = uniqueName('minicheck');
  await page.getByTestId('chat-message-input').fill(text);
  const sendButton = page.getByTestId('chat-send-button');
  await expect(sendButton).toBeEnabled({ timeout: 5_000 });
  const created = page.waitForResponse(
    (response) => response.url().includes(CONVERSATIONS_PATH) && response.request().method() === 'POST',
    { timeout: 15_000 },
  );
  await sendButton.click();
  const response = await created;
  const body = (await response.json()) as { id?: string };

  try {
    await expect(page.getByTestId('user-message').first()).toContainText(text, { timeout: 15_000 });
    await expect(page.getByTestId('chat-voice-mini-player')).toHaveCount(0);
  } finally {
    if (body.id) await deleteConversation(page.request, body.id);
  }
});
