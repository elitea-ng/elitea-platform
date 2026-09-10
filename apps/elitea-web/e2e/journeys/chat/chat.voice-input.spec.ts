/**
 * Voice input (dictation) — `widgets/chat`'s `VoiceButton`, the ASR mic that
 * inserts a transcript into the composer. Ported by use case from the
 * onetest voice package (`voice/voice-input`), against this stack's DEFAULT
 * state: the member persona's project has no ASR model configured for
 * `section: 'asr'` (same assumption `admin.features.spec.ts`'s J36f states),
 * so `useSpeechRecognition` (the browser Web Speech API wrapper) is what
 * actually drives the button — see that hook and
 * `useSpeakingModeLoop.ts`'s `serverHook.isSupported ? serverHook :
 * clientHook` selection.
 *
 * ## Why a stub, not a real microphone
 *
 * A headless Chromium ships `webkitSpeechRecognition` but has no working
 * audio device and no network path to a real speech backend, so `start()`
 * either hangs or errors for reasons that have nothing to do with this
 * product. `useSpeechRecognition.ts`'s whole surface — `onresult`/`onerror`/
 * `onend` — is replaced with a bare stub class BEFORE the page loads
 * (`addInitScript`), same technique `settings.voice.spec.ts` uses for
 * `speechSynthesis` and `admin.features.spec.ts`'s J36f uses for this exact
 * constructor. Firing a transcript is then calling the hook's own callback
 * directly, which is the same call the real engine would have made.
 *
 * None of the tests below sends a chat message: dictation only inserts text,
 * it never submits, so no conversation row is ever created and no cleanup is
 * needed.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

/** Installs a controllable `webkitSpeechRecognition` before any script runs. */
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
      start(): void {
        /* the stub never actually opens a mic */
      }
      stop(): void {
        /* isRecording flips client-side; the hook does not wait for onend */
      }
      abort(): void {
        /* no-op */
      }
    }
    // This Chromium build exposes an UNPREFIXED `window.SpeechRecognition`
    // natively (measured — not merely `webkitSpeechRecognition`), and
    // `getSpeechRecognitionConstructor()` reads `SpeechRecognition ??
    // webkitSpeechRecognition`, so overriding only the prefixed name left the
    // hook constructing the REAL, hardware-backed engine. Both names must
    // point at the stub.
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

/** Fires one transcript event on the MOST RECENTLY constructed recognition instance. */
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

/** Fires an error event (e.g. permission denial) on the latest instance. */
async function fireError(page: Page, error: string): Promise<void> {
  await page.evaluate((error) => {
    interface Rec {
      onerror: ((event: unknown) => void) | null;
    }
    const list = (window as unknown as { __autotestRecognitions: Rec[] }).__autotestRecognitions;
    const rec = list[list.length - 1];
    rec?.onerror?.({ error });
  }, error);
}

async function openChat(page: Page): Promise<void> {
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 20_000 });
}

test.beforeEach(async ({ page }) => {
  await stubSpeechRecognition(page);
});

/* onetest: ELITEA-1326 — mic tooltip/aria state idle vs recording, Stop dictation control appears only while recording */
test('the mic control reflects idle/recording state in its tooltip, aria-pressed and Stop control', async ({
  page,
}) => {
  await openChat(page);

  const mic = page.locator('button[aria-pressed]');
  await expect(mic).toBeVisible({ timeout: 20_000 });
  await expect(mic).toHaveAttribute('aria-pressed', 'false');
  await mic.hover();
  await expect(page.getByRole('tooltip', { name: 'Start voice input' })).toBeVisible();

  // No separate "Stop dictation" control while idle.
  await expect(page.getByRole('button', { name: 'stop voice input' })).toHaveCount(0);

  await mic.click();
  await expect(mic).toHaveAttribute('aria-pressed', 'true');
  // The mic itself becomes DISABLED while recording (`deriveVoiceButtonUiState`:
  // `micDisabled: disabled || isRecording || isAdminDisabled` — the Stop
  // button is the live control once capture starts), and `Tooltip` does not
  // fire a real hover on a disabled child; its accessible-name mirror on the
  // wrapping `<span>` is read directly instead.
  await expect(mic).toHaveAttribute('aria-label', 'voice input active');
  await expect(page.locator('span[aria-label="Voice input active"]')).toBeVisible();

  const stop = page.getByRole('button', { name: 'stop voice input' });
  await expect(stop, 'a distinct Stop control must appear once recording starts').toBeVisible();

  await stop.click();
  await expect(mic).toHaveAttribute('aria-pressed', 'false');
  await expect(page.getByRole('button', { name: 'stop voice input' })).toHaveCount(0);
});

/* onetest: ELITEA-1323, ELITEA-1325 — core smoke on Chat: mic starts capture, transcript lands in the input, stop reverts the icon (Agent/Pipeline pages not independently exercised — see the ledger) */
test('clicking the mic starts capture and the transcript lands in the input; stopping reverts it', async ({
  page,
}) => {
  await openChat(page);

  const input = page.getByTestId('chat-message-input');
  await expect(input).toHaveValue('');

  const mic = page.locator('button[aria-pressed]');
  await mic.click();
  await expect(mic).toHaveAttribute('aria-pressed', 'true');

  await fireTranscript(page, 'Testing microphone functionality');
  await expect(input).toHaveValue('Testing microphone functionality');

  await page.getByRole('button', { name: 'stop voice input' }).click();
  await expect(mic).toHaveAttribute('aria-pressed', 'false');
  // The transcribed text remains — stopping does not clear the composer.
  await expect(input).toHaveValue('Testing microphone functionality');
});

/* onetest: ELITEA-1321 — stopping dictation without sending submits nothing; a new session appends rather than overwriting or duplicating */
test('stopping dictation without sending submits nothing, and a new session appends to the existing text', async ({
  page,
}) => {
  await openChat(page);

  const input = page.getByTestId('chat-message-input');
  const mic = page.locator('button[aria-pressed]');

  await mic.click();
  await fireTranscript(page, 'First sentence');
  await page.getByRole('button', { name: 'stop voice input' }).click();

  await expect(input).toHaveValue('First sentence');
  // Nothing was submitted: no user-message bubble, composer still holds the draft.
  await expect(page.getByTestId('user-message')).toHaveCount(0);

  await mic.click();
  await fireTranscript(page, 'Second sentence');
  await page.getByRole('button', { name: 'stop voice input' }).click();

  const value = await input.inputValue();
  expect(value, 'the first phrase must survive, once, alongside the second').toContain('First sentence');
  expect(value).toContain('Second sentence');
  expect(value.match(/First sentence/g)).toHaveLength(1);
});

/* onetest: ELITEA-1320 — dictated text and a file attachment combine on the same composed message */
test('a dictated transcript and an attached file both stay on the composer', async ({ page }) => {
  await openChat(page);

  const plus = page.getByTestId('plus-menu-button');
  await expect(plus).toBeEnabled({ timeout: 20_000 });
  await plus.click();
  const attachRow = page.getByTestId('plus-menu-attachments');
  await expect(attachRow).toBeVisible();
  const fileInput = attachRow.locator('xpath=ancestor::div[1]').locator('input[type="file"]');
  await fileInput.setInputFiles({
    name: 'e2e-voice-input.txt',
    mimeType: 'text/plain',
    buffer: Buffer.from('autotest voice+attachment\n'),
  });
  // Close the menu so it stops overlapping the composer (same reason
  // `chat.attachments.spec.ts` closes it before typing).
  await plus.click();
  await expect(attachRow).toHaveCount(0);

  await expect(page.getByTestId('chat-attachment-chip-0')).toBeVisible({ timeout: 15_000 });

  const mic = page.locator('button[aria-pressed]');
  await mic.click();
  await fireTranscript(page, 'Please analyze this file');
  await page.getByRole('button', { name: 'stop voice input' }).click();

  await expect(page.getByTestId('chat-message-input')).toHaveValue('Please analyze this file');
  await expect(page.getByTestId('chat-attachment-chip-0')).toBeVisible();
  await expect(page.getByTestId('chat-send-button')).toBeEnabled();
});

/*
 * onetest: ELITEA-1295 — Speaking Mode cannot be entered while dictation
 * (Voice Input) is active, and is available again once it stops.
 *
 * PRODUCT GAP. `ChatBoxInputSlots.tsx` wires `VoiceButton`'s
 * `onRecordingChange` to `() => {}` — a no-op — and `ChatBox.tsx`'s own
 * `voice={{ isSpeakingMode, onSpeakingModeToggle, isTTSPlaying }}` object
 * carries no `isRecording` field at all. `NewChatInput.tsx`'s
 * `finalIsRecording = voice.isRecording || isSpeakingModeRecording` is
 * therefore always driven by `isSpeakingModeRecording` alone: dictation
 * starting never reaches `disabledSend`, so the wave icon never disables.
 * Written as the case should pass; it fails for the reason above, not a
 * selector mistake.
 */
test('the Speaking Mode control is disabled while dictation is recording', async ({ page }) => {
  await openChat(page);

  const mic = page.locator('button[aria-pressed]');
  const speakingModeButton = page.getByTestId('chat-speaking-mode-button');
  await expect(speakingModeButton).toBeEnabled({ timeout: 20_000 });

  await mic.click();
  await expect(mic).toHaveAttribute('aria-pressed', 'true');

  test.fail(
    true,
    'ELITEA-1295 (#932): product gap — VoiceButton.onRecordingChange is wired to a no-op ' +
      '(ChatBoxInputSlots.tsx) and voice.isRecording is never supplied (ChatBox.tsx), ' +
      'so the Speaking Mode wave icon never disables while dictation is recording',
  );
  await expect(
    speakingModeButton,
    'the wave icon must be blocked while Voice Input owns the composer',
  ).toBeDisabled();

  await page.getByRole('button', { name: 'stop voice input' }).click();
  await expect(mic).toHaveAttribute('aria-pressed', 'false');
  await expect(speakingModeButton).toBeEnabled();
});

/*
 * onetest: ELITEA-1324 — microphone permission denied must surface a clear,
 * user-readable toast and leave typed input working.
 *
 * `VoiceButton.tsx`'s own module doc: "no toast/snackbar primitive exists yet
 * in `shared/ui`... the caller decides how to surface the message until one
 * lands." There is no caller that shows one — `onError` is wired to nothing
 * visible in `ChatBoxInputSlots.tsx` (`onRecordingChange: () => {}`, no
 * `onError` passed at all). This is written as the case should pass and
 * marked as a product gap rather than skipped.
 */
test('a denied microphone permission surfaces a readable error toast', async ({ page }) => {
  await openChat(page);

  const mic = page.locator('button[aria-pressed]');
  await mic.click();
  await fireError(page, 'not-allowed');

  test.fail(true, 'ELITEA-1324 (#934): product gap — no toast/snackbar surfaces a mic-permission error; VoiceButton.onError has no visible caller');
  await expect(page.getByRole('alert')).toBeVisible({ timeout: 5_000 });
});

/* onetest: ELITEA-1318 — with no ASR model configured, Chrome/Chromium still offers the mic (browser Speech API fallback), and text arrives progressively rather than as one flushed sentence */
test('with no project ASR model, the mic is still offered and transcribes progressively', async ({
  page,
}) => {
  await openChat(page);

  const mic = page.locator('button[aria-pressed]');
  await expect(mic, 'Chrome/Chromium must offer the browser fallback mic even with no ASR model').toBeVisible();

  await mic.click();
  const input = page.getByTestId('chat-message-input');
  await fireTranscript(page, 'Hello', false);
  await expect(input).toHaveValue('Hello');
  await fireTranscript(page, 'Hello world', false);
  await expect(input).toHaveValue('Hello world');
  await fireTranscript(page, 'Hello world test', true);
  await expect(input).toHaveValue('Hello world test');
});

/*
 * onetest: ELITEA-1317 — voice dictation inserts transcribed text at the
 * LAST EDITED position, not wherever the user last moved the visual caret
 * with a click.
 *
 * `VoiceButton.tsx`'s `handleStartRecording` captures
 * `handle.getCursorPosition()` at the moment the mic is CLICKED — whatever
 * that position is, including one reached by a plain mouse click with no
 * typing. Written as the case should behave (insert after "world", where
 * the user last EDITED); expected to fail if the click alone moved the
 * insertion point.
 */
test('voice dictation inserts at the last edited position, not a bare click', async ({ page }) => {
  await openChat(page);
  const input = page.getByTestId('chat-message-input');

  await input.fill('Hello world');
  // Move the caret between "Hello" and "world" with a CLICK, not a keystroke.
  await input.click({ position: { x: 0, y: 0 } });
  await input.press('Home');
  await input.press('ArrowRight', { delay: 0 });
  for (let i = 0; i < 4; i++) await input.press('ArrowRight');
  // Caret now sits right after "Hello" (before the space).

  const mic = page.locator('button[aria-pressed]');
  await mic.click();
  await fireTranscript(page, 'beautiful');
  await page.getByRole('button', { name: 'stop voice input' }).click();

  test.fail(
    true,
    'ELITEA-1317 (#933): product gap — VoiceButton captures the cursor position at the moment ' +
      'the mic is clicked (VoiceButton.tsx handleStartRecording), including a position ' +
      'reached by a bare mouse click with no edit, so dictation lands mid-text instead ' +
      'of at the last EDITED position',
  );
  await expect(input).toHaveValue('Hello world beautiful');
});
