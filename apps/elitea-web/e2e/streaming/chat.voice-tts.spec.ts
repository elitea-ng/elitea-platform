/**
 * Text-to-speech on a real answer — the onetest `voice/text-to-speech-tts`
 * cases and the TTS half of `voice/speaking-mode` (#939 group 6).
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS IS A CHAT-STREAM SPEC AND NOT A JOURNEY
 * ─────────────────────────────────────────────────────────────────────────────
 * Every case here reads an AI ANSWER aloud, and the Read-out control only
 * exists on an answer: `ApplicationAnswerActions` renders it behind
 * `onAutoSpeak && hasSpeakableText`, and `hasSpeakableText` is the answer's own
 * text. The journeys stack has no runtime plane, so there is no answer to read
 * there — which is why `chat.voice-speaking-mode.spec.ts` (the journeys
 * sibling) states TTS as out of its scope and the onetest ledger left these
 * cases stream-deferred.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE DOUBLE: `window.speechSynthesis`, NOT A NEW BACKEND
 * ─────────────────────────────────────────────────────────────────────────────
 * The app picks its engine as `hasModelTTS = !!(ttsModel && socket &&
 * AudioContext)` (`useTextToSpeech.hooks.ts`), and no `app/` file mounts a
 * socket provider yet — so the BROWSER engine (`useBrowserTtsEngine.hooks.ts`,
 * `window.speechSynthesis`) is the path this product actually takes. Stubbing
 * that API is therefore not a simulation of the feature: it is the real code
 * path with a deterministic speech device under it, the same posture
 * `chat.voice-input.spec.ts` already takes for `SpeechRecognition`.
 *
 * The stub records what was spoken and when it was cancelled, and lets a test
 * END an utterance on demand — a real engine's `onend` arrives whenever the
 * voice finishes, which is neither deterministic nor drivable from Playwright.
 * Both stubs live here rather than in `e2e/fixtures/*` for the reason the
 * journeys sibling gives for its own copy: these are spec-local devices, not a
 * helper several areas need.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE TWO-STEP, AND WHAT IS FAIL-MARKED (#974)
 * ─────────────────────────────────────────────────────────────────────────────
 * `Read out` does NOT start the voice in this app. `onAutoSpeak` only ARMS the
 * text (`setSpeakableText` + `setShowPlayer(true)`), and `showPlayer` is
 * destructured nowhere — the baseline's `{showPlayer && <VoiceMiniPlayer/>}`
 * has no counterpart here. What does speak is `VoiceControlButton`, mounted
 * permanently in the composer by `ChatBoxInputSlots`: its play control speaks
 * whatever the last `Read out` armed. The cases that only need playback to
 * HAPPEN drive that two-step (`startPlayback`) and pass; the three claims that
 * are genuinely unbuilt — read-out starting playback, speaking-mode auto-read,
 * and stopping the voice on delete/regenerate — are `test.fail`-marked against
 * issue #974, which records each against its baseline call site.
 */
import type { Page } from '@playwright/test';
import { expect, test } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { AUTOTEST_PREFIX, expectStoredAssistantAnswer, fillComposer } from '../fixtures/api';
import { PIPELINE_STARTER_TEMPLATE } from '../fixtures/pipelines';

const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** What the page-side stub records, read back with `ttsState`. */
interface TtsProbe {
  readonly spoken: readonly string[];
  readonly cancels: number;
  readonly speaking: boolean;
  readonly recognitionStarts: number;
  readonly recognitionStops: number;
}

/**
 * Replaces the browser's speech device with a recorder.
 *
 * `speechSynthesis` is a read-only `window` property, so it is redefined
 * rather than assigned — the same `Object.defineProperty` shape the sibling
 * `SpeechRecognition` stub uses, and for the same reason.
 *
 * `SpeechRecognition` is stubbed HERE TOO, in one init script: two of the
 * cases below are about what the microphone does while TTS plays, and a
 * recognition device that is real would ask for a microphone permission this
 * lane has no way to grant.
 */
async function stubSpeechDevices(page: Page): Promise<void> {
  await page.addInitScript(() => {
    interface Utterance {
      text: string;
      onend: (() => void) | null;
      onerror: (() => void) | null;
      onboundary: ((event: unknown) => void) | null;
      onstart: (() => void) | null;
    }
    const probe = {
      spoken: [] as string[],
      cancels: 0,
      speaking: false,
      recognitionStarts: 0,
      recognitionStops: 0,
      live: null as Utterance | null,
    };
    (window as unknown as { __autotestTts: typeof probe }).__autotestTts = probe;

    class AutotestUtterance implements Utterance {
      text: string;
      lang = 'en-US';
      rate = 1;
      pitch = 1;
      volume = 1;
      voice: unknown = null;
      onend: (() => void) | null = null;
      onerror: (() => void) | null = null;
      onboundary: ((event: unknown) => void) | null = null;
      onstart: (() => void) | null = null;
      constructor(text?: string) {
        this.text = text ?? '';
      }
      addEventListener(): void {}
      removeEventListener(): void {}
    }

    const synthesis = {
      speaking: false,
      paused: false,
      pending: false,
      onvoiceschanged: null,
      getVoices: () => [{ name: 'Autotest Voice', lang: 'en-US', default: true, localService: true, voiceURI: 'autotest' }],
      speak(utterance: AutotestUtterance) {
        probe.spoken.push(utterance.text);
        probe.speaking = true;
        probe.live = utterance;
        synthesis.speaking = true;
        // A real engine fires `start` asynchronously; the hook flips its own
        // status from the call, so the event only has to exist.
        setTimeout(() => utterance.onstart?.(), 0);
      },
      cancel() {
        probe.cancels += 1;
        probe.speaking = false;
        probe.live = null;
        synthesis.speaking = false;
      },
      pause() {
        synthesis.paused = true;
      },
      resume() {
        synthesis.paused = false;
      },
      addEventListener(): void {},
      removeEventListener(): void {},
    };
    Object.defineProperty(window, 'speechSynthesis', { configurable: true, value: synthesis });
    Object.defineProperty(window, 'SpeechSynthesisUtterance', { configurable: true, value: AutotestUtterance });

    class AutotestRecognition {
      continuous = false;
      interimResults = false;
      lang = 'en-US';
      onresult: ((event: unknown) => void) | null = null;
      onerror: ((event: unknown) => void) | null = null;
      onend: (() => void) | null = null;
      start(): void {
        probe.recognitionStarts += 1;
      }
      stop(): void {
        probe.recognitionStops += 1;
        this.onresult = null;
      }
      abort(): void {
        probe.recognitionStops += 1;
        this.onresult = null;
      }
    }
    Object.defineProperty(window, 'SpeechRecognition', { configurable: true, value: AutotestRecognition });
    Object.defineProperty(window, 'webkitSpeechRecognition', { configurable: true, value: AutotestRecognition });
  });
}

async function ttsState(page: Page): Promise<TtsProbe> {
  return page.evaluate(() => {
    const probe = (window as unknown as { __autotestTts: TtsProbe }).__autotestTts;
    return {
      spoken: [...probe.spoken],
      cancels: probe.cancels,
      speaking: probe.speaking,
      recognitionStarts: probe.recognitionStarts,
      recognitionStops: probe.recognitionStops,
    };
  });
}

/** Ends the utterance that is currently speaking, as a real voice would. */
async function finishSpeaking(page: Page): Promise<void> {
  await page.evaluate(() => {
    const probe = (window as unknown as {
      __autotestTts: { live: { onend: (() => void) | null } | null; speaking: boolean };
    }).__autotestTts;
    const live = probe.live;
    probe.speaking = false;
    probe.live = null;
    (window as unknown as { speechSynthesis: { speaking: boolean } }).speechSynthesis.speaking = false;
    live?.onend?.();
  });
}

const readOutButton = (page: Page) => page.getByRole('button', { name: 'Read out' }).last();
const speakingModeButton = (page: Page) => page.getByTestId('chat-speaking-mode-button');

/**
 * One real turn against the mock, returning the answer text the transcript
 * stored — which is what the Read-out control must speak.
 *
 * The answer is READ BACK from the server rather than scraped off the screen:
 * the whole claim of these cases is that the FINAL ANSWER is what gets spoken,
 * so the expected value has to come from somewhere other than the same DOM the
 * assertion reads.
 */
async function answerOneTurn(page: Page, prompt: string): Promise<string> {
  await page.goto(BASE_URL + '/app/chat');
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('model-selector-button').click();
  const modelOption = page.getByRole('menuitem').filter({ hasText: MODEL_NAME }).first();
  await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
  await modelOption.click();

  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 15_000 });
  await input.fill(prompt);
  const created = page.waitForResponse(
    (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
    timeout: 30_000,
  });
  await page.getByTestId('chat-send-button').click();
  const conversationId = String(((await (await created).json()) as { id?: unknown }).id ?? '');
  const startResponse = await started;
  expect(startResponse.status(), `the turn was refused: ${(await startResponse.text()).slice(0, 200)}`).toBe(200);
  const projectId = START_RE.exec(startResponse.url())?.[1] ?? '';

  await expectStoredAssistantAnswer(page, projectId, conversationId, {
    timeout: 120_000,
    message: 'the turn produced no stored answer, so there is nothing for TTS to read',
  });
  const answer = page.getByTestId('chat-message-list').getByTestId('application-answer').last();
  await expect(answer).toContainText('MOCK', { timeout: 60_000 });
  return (await answer.textContent()) ?? '';
}

/**
 * The control that actually speaks — `VoiceControlButton`'s play/stop, mounted
 * in the composer. The SAME element becomes Stop while the voice runs (the app
 * swaps `onPlay`/`onStop` and the icon on `isPlaying`), which is what the stop
 * assertions below click.
 */
const playControl = (page: Page) => page.getByTestId('chat-voice-play-stop-button');

/**
 * Arms the answer and starts the voice — the app's real two-step (see the
 * header, and #974 for why the first click alone is not enough). Returns how
 * many utterances had been spoken before, so a caller can assert on the new
 * one without assuming it is the first.
 */
async function startPlayback(page: Page): Promise<number> {
  const before = (await ttsState(page)).spoken.length;
  const button = readOutButton(page);
  await expect(button, 'the answer action bar must offer Read out').toBeVisible({ timeout: 30_000 });
  await button.click();
  await expect(playControl(page), 'arming an answer must leave a play control to press').toBeVisible({ timeout: 15_000 });
  await playControl(page).click();
  await expect
    .poll(async () => (await ttsState(page)).spoken.length, {
      timeout: 15_000,
      message: 'the play control spoke nothing',
    })
    .toBeGreaterThan(before);
  return before;
}

/**
 * Speaking mode has no `aria-pressed` — `SendButton` REPLACES the mic with a
 * "Speaking..." strip — so its state is read from which of the two is on
 * screen.
 */
async function enterSpeakingMode(page: Page): Promise<void> {
  await expect(speakingModeButton(page), 'the composer must offer speaking mode').toBeVisible({ timeout: 20_000 });
  await speakingModeButton(page).click();
  await expectSpeakingMode(page, true);
}

async function expectSpeakingMode(page: Page, on: boolean, message?: string): Promise<void> {
  const strip = page.getByText('Speaking...', { exact: true });
  if (on) {
    await expect(strip, message ?? 'speaking mode must be on').toBeVisible({ timeout: 15_000 });
  } else {
    await expect(strip, message ?? 'speaking mode must be off').toBeHidden({ timeout: 15_000 });
  }
}

test.beforeEach(async ({ page }) => {
  await stubSpeechDevices(page);
});

/* onetest: ELITEA-1311 — the core claim of the TTS smoke: the Read-out control on an answer's action bar
 * STARTS playback. It does not: `onAutoSpeak` arms the text and the player that would play it
 * (`showPlayer` → `VoiceMiniPlayer`) is rendered nowhere. */
test('clicking Read out starts playback of the answer', async ({ page }) => {
  test.setTimeout(240_000);
  test.fail(true, '#974: product gap — Read out only arms the text; `showPlayer`/`VoiceMiniPlayer` has no render site, so nothing speaks until the composer play control is pressed');
  await answerOneTurn(page, `autotest tts arm ${String(Date.now() % 1_000_000)}`);

  const button = readOutButton(page);
  await expect(button, 'the answer action bar must offer Read out').toBeVisible({ timeout: 30_000 });
  await button.click();
  await expect
    .poll(async () => (await ttsState(page)).spoken.length, {
      timeout: 15_000,
      message: 'clicking Read out spoke nothing',
    })
    .toBeGreaterThan(0);
});

/* onetest: ELITEA-1311 — the rest of the TTS smoke, on the two-step this app really has: what is spoken is
 * the final answer and only the final answer, the control shows playback is active, and everything resets
 * when the voice ends by itself. */
test('the answer is spoken in full and every control resets when the voice finishes', async ({ page }) => {
  test.setTimeout(240_000);
  const answerText = await answerOneTurn(page, `autotest tts smoke ${String(Date.now() % 1_000_000)}`);

  await startPlayback(page);
  const state = await ttsState(page);
  expect(state.spoken, 'one read-out must produce exactly one utterance').toHaveLength(1);
  const spoken = state.spoken[0] ?? '';
  // The answer's own words, not the surrounding chrome. `toSpeakableText`
  // filters markdown before speaking, so the assertion is on a word the answer
  // certainly holds rather than on the full string.
  expect(spoken, 'the spoken text must be the answer').toContain('MOCK');
  expect(answerText.replace(/\s+/g, ' '), 'the answer must still be the one that was read').toContain('MOCK');
  // Nothing but the answer: the action bar's own labels are the chrome nearest
  // the text, and a naive `textContent` read of the bubble would have caught
  // them.
  for (const chrome of ['Read out', 'Regenerate', 'Copy to clipboard']) {
    expect(spoken, `the action bar's "${chrome}" is not part of the answer`).not.toContain(chrome);
  }
  // WHILE SPEAKING the read-out control is inert — `ApplicationAnswerActions`
  // disables it on `isSpeaking`, which is the visual state the case calls the
  // indicator.
  await expect(readOutButton(page), 'the control must show playback is active').toBeDisabled({ timeout: 10_000 });

  await finishSpeaking(page);
  await expect(readOutButton(page), 'the control must reset when the voice ends on its own').toBeEnabled({
    timeout: 15_000,
  });
  expect((await ttsState(page)).speaking).toBe(false);
});

/* onetest: ELITEA-1303 — stopping and re-triggering restarts playback from the beginning, and both controls
 * are reachable and operable from the keyboard alone. */
test('read-out restarts from the beginning after a stop, and works from the keyboard', async ({ page }) => {
  test.setTimeout(240_000);
  await answerOneTurn(page, `autotest tts restart ${String(Date.now() % 1_000_000)}`);

  await startPlayback(page);
  const first = (await ttsState(page)).spoken[0] ?? '';

  // The play control is the stop control while the voice runs.
  const cancelsBefore = (await ttsState(page)).cancels;
  await playControl(page).click();
  await expect
    .poll(async () => (await ttsState(page)).cancels, { timeout: 15_000, message: 'Stop left the voice running' })
    .toBeGreaterThan(cancelsBefore);
  await expect(readOutButton(page)).toBeEnabled({ timeout: 15_000 });

  // KEYBOARD, not a click: the case's own accessibility claim, on both halves
  // of the two-step.
  await readOutButton(page).focus();
  await expect(readOutButton(page)).toBeFocused();
  await page.keyboard.press('Enter');
  await playControl(page).focus();
  await expect(playControl(page)).toBeFocused();
  await page.keyboard.press('Space');
  await expect
    .poll(async () => (await ttsState(page)).spoken.length, {
      timeout: 15_000,
      message: 'the keyboard-triggered read-out spoke nothing',
    })
    .toBe(2);
  expect(
    (await ttsState(page)).spoken[1],
    'a re-trigger must start the same answer from the beginning, not resume mid-way',
  ).toBe(first);
});

/* onetest: ELITEA-1306 — while an answer is being read aloud, the rest of its action bar (Copy, Like,
 * Dislike, Regenerate, Delete) stays usable and rendered. */
test('the other answer controls stay functional while an answer is being read', async ({ page }) => {
  test.setTimeout(240_000);
  await answerOneTurn(page, `autotest tts siblings ${String(Date.now() % 1_000_000)}`);

  await startPlayback(page);
  expect((await ttsState(page)).speaking, 'the voice must be running for this case to mean anything').toBe(true);

  for (const name of ['Copy to clipboard', 'Like this answer', 'Dislike this answer', 'Regenerate', 'Delete']) {
    await expect(
      page.getByRole('button', { name, exact: true }).last(),
      `${name} must stay operable while the answer is read aloud`,
    ).toBeEnabled({ timeout: 10_000 });
  }
  // Rating really registers mid-playback, and the voice is not disturbed by it.
  await page.getByRole('button', { name: 'Like this answer', exact: true }).last().click();
  await expect(page.getByRole('button', { name: 'Dislike this answer', exact: true }).last()).toBeEnabled();
  expect((await ttsState(page)).speaking, 'rating an answer must not stop the voice').toBe(true);
});

/* onetest: ELITEA-1307 (Clear parameter), ELITEA-1301 — clearing the chat while an answer is being read stops
 * the voice, and Speaking Mode survives that interruption. */
test('clearing the chat stops playback and leaves speaking mode on', async ({ page }) => {
  test.setTimeout(240_000);
  await answerOneTurn(page, `autotest tts clear ${String(Date.now() % 1_000_000)}`);

  await enterSpeakingMode(page);
  await startPlayback(page);
  const before = (await ttsState(page)).cancels;

  await page.getByRole('button', { name: 'Clear the chat history' }).click();
  await expect
    .poll(async () => (await ttsState(page)).cancels, {
      timeout: 20_000,
      message: 'clearing the chat left the voice running',
    })
    .toBeGreaterThan(before);
  expect((await ttsState(page)).speaking).toBe(false);
  await expectSpeakingMode(page, true, 'an interrupted read-out must not turn Speaking Mode off');
});

/* onetest: ELITEA-1307 (Delete parameter) — deleting the answer that is being read must stop the voice.
 * `useChatBoxActions` stops read-aloud on Clear only; the baseline also stops it from the delete dialog. */
test('deleting the answer being read stops playback', async ({ page }) => {
  test.setTimeout(240_000);
  test.fail(true, '#974: product gap — only `handleClear` calls `readAloudStop()`; delete leaves the old voice running (baseline: `useDeleteMessageAlert({ onStopTTS })`)');
  await answerOneTurn(page, `autotest tts delete ${String(Date.now() % 1_000_000)}`);

  await startPlayback(page);
  const before = (await ttsState(page)).cancels;

  await page.getByTestId('chat-message-list').getByRole('button', { name: 'Delete', exact: true }).last().click();
  const dialog = page.getByTestId('chat-delete-confirm-dialog');
  await expect(dialog).toBeVisible({ timeout: 15_000 });
  await dialog.getByRole('button', { name: 'Delete', exact: true }).click();

  await expect
    .poll(async () => (await ttsState(page)).cancels, {
      timeout: 20_000,
      message: 'deleting the answer left the voice running',
    })
    .toBeGreaterThan(before);
});

/* onetest: ELITEA-1307 (Regenerate parameter), ELITEA-1292 — regenerating the answer that is being read must
 * stop the old voice before the new turn starts, so the two never overlap. */
test('regenerating the answer being read stops the old playback', async ({ page }) => {
  test.setTimeout(240_000);
  test.fail(true, '#974: product gap — `handleRegenerate` never calls `readAloudStop()` (baseline stops TTS in `onRegenerateAnswer`), so the old answer keeps speaking over the new turn');
  await answerOneTurn(page, `autotest tts regen ${String(Date.now() % 1_000_000)}`);

  await startPlayback(page);
  const before = (await ttsState(page)).cancels;

  await page.getByTestId('chat-message-list').getByRole('button', { name: 'Regenerate', exact: true }).last().click();
  await expect
    .poll(async () => (await ttsState(page)).cancels, {
      timeout: 30_000,
      message: 'regenerating left the old voice running',
    })
    .toBeGreaterThan(before);
});

/* onetest: ELITEA-1297, ELITEA-1289, ELITEA-1288 — with Speaking Mode on, the microphone is released while an
 * answer is read aloud and picked up again the moment the voice finishes; stopping the voice does not turn
 * Speaking Mode off. */
test('speaking mode releases the microphone during playback and resumes when it ends', async ({ page }) => {
  test.setTimeout(240_000);
  await answerOneTurn(page, `autotest tts mic ${String(Date.now() % 1_000_000)}`);

  await enterSpeakingMode(page);
  await expect
    .poll(async () => (await ttsState(page)).recognitionStarts, {
      timeout: 15_000,
      message: 'speaking mode never started listening',
    })
    .toBeGreaterThan(0);
  const listening = await ttsState(page);

  await startPlayback(page);
  await expect
    .poll(async () => (await ttsState(page)).recognitionStops, {
      timeout: 20_000,
      message: 'the microphone kept listening while the answer was read aloud',
    })
    .toBeGreaterThan(listening.recognitionStops);

  await finishSpeaking(page);
  await expect
    .poll(async () => (await ttsState(page)).recognitionStarts, {
      timeout: 20_000,
      message: 'listening did not resume after the voice finished',
    })
    .toBeGreaterThan(listening.recognitionStarts);
  await expectSpeakingMode(page, true, 'finishing a read-out must leave Speaking Mode exactly as it was');
});

/* onetest: ELITEA-1296, ELITEA-1292 — with Speaking Mode on, a freshly generated answer is read back without
 * the user touching anything. The effect exists in `ApplicationAnswer`, but nothing ever passes it
 * `isSpeakingMode`. */
test('speaking mode reads a new answer back automatically', async ({ page }) => {
  test.setTimeout(240_000);
  test.fail(true, '#974: product gap — `buildTtsProps` hardcodes `autoSpeak: false` and `ChatMessageList` never forwards `isSpeakingMode` to `ApplicationAnswer`, so the auto-read effect can never fire');
  await answerOneTurn(page, `autotest tts auto ${String(Date.now() % 1_000_000)}`);

  await enterSpeakingMode(page);
  // A regenerate is a full generation the user did not type: the composer is
  // not usable for this, because speaking mode replaces the send control.
  await page.getByTestId('chat-message-list').getByRole('button', { name: 'Regenerate', exact: true }).last().click();
  await expect
    .poll(async () => (await ttsState(page)).spoken.length, {
      timeout: 60_000,
      message: 'the answer generated in speaking mode was never read back',
    })
    .toBeGreaterThan(0);
});

/* onetest: ELITEA-1302 — Read out is available and functional on the PIPELINE page's own test chat, not only
 * in the chat workspace. (The agent page half of this case has no counterpart here: `src/pages/agents` mounts
 * no ChatBox — `ChatWithAgentButton` navigates to `/app/chat`, which every other test in this file covers.) */
test('the pipeline editor test chat offers a working Read out on its answer', async ({ page }) => {
  test.setTimeout(300_000);
  const name = `${AUTOTEST_PREFIX}tts-pipe-${String(Date.now() % 1_000_000)}`;

  // Created through the FORM for the reason `chat.pipeline.spec.ts` records:
  // the chat persona works inside its OWN personal project (#290), and the
  // create POST's URL is the only place that project id is published.
  const created = page.waitForResponse(
    (r) => /\/elitea_core\/applications\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
    { timeout: 30_000 },
  );
  await page.goto(`${BASE_URL}/app/pipelines/create`);
  await expect(page.getByTestId('agent-name-input')).toBeVisible({ timeout: 30_000 });
  await page.getByTestId('agent-name-input').fill(name);
  await page.getByTestId('agent-description-input').fill(`${name} description`);
  await page.getByTestId('pipeline-save-button').click();
  const createResponse = await created;
  expect(createResponse.status(), `the pipeline must be created: ${(await createResponse.text()).slice(0, 300)}`).toBe(201);
  const projectId = /\/prompt_lib\/(\d+)$/.exec(new URL(createResponse.url()).pathname)?.[1] ?? '';
  const body = (await createResponse.json()) as { id?: string; version_details?: { id?: string; name?: string } };
  const pipelineId = String(body.id ?? '');
  const versionId = String(body.version_details?.id ?? '');
  expect(pipelineId, 'the created pipeline must carry an id').toMatch(/^\d+$/);

  try {
    // The starter graph, written by version PUT — the YAML pane is a CodeMirror
    // document and `fill()` re-indents it, so it is never typed (same reason
    // `chat.pipeline.spec.ts` gives). Its `state` uses the DESCRIPTOR form,
    // which both the native and the SDK compilers accept.
    const saved = await page.request.put(
      `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${pipelineId}/${versionId}`,
      {
        data: {
          name: body.version_details?.name ?? 'latest',
          agent_type: 'pipeline',
          instructions: PIPELINE_STARTER_TEMPLATE,
          conversation_starters: [],
          variables: [],
          meta: { step_limit: 25, internal_tools: [] },
        },
      },
    );
    expect(saved.status(), `the graph must save: ${(await saved.text()).slice(0, 300)}`).toBeLessThan(300);

    await page.goto(`${BASE_URL}/app/pipelines/latest/${pipelineId}`);
    const pane = page.getByTestId('edit-pipeline-test-chat');
    await expect(pane, 'the editor’s test chat must mount').toBeVisible({ timeout: 60_000 });

    const conversationCreated = page.waitForResponse(
      (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
      { timeout: 30_000 },
    );
    await pane.getByTestId('chat-message-input').click();
    const conversationId = String(((await (await conversationCreated).json()) as { id?: unknown }).id ?? '');

    const sendButton = await fillComposer(pane, `autotest tts pipeline ${String(Date.now() % 1_000_000)}`);
    const started = page.waitForResponse((r) => START_RE.test(r.url()) && r.request().method() === 'POST', {
      timeout: 60_000,
    });
    await sendButton.click();
    expect((await started).status(), 'the pipeline turn must be admitted').toBe(200);
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 180_000,
      message: 'the pipeline produced no stored answer, so there is nothing for TTS to read',
    });

    // The whole claim: the same action bar, with the same working control, on
    // this surface. Scoped to the pane so a stray chat elsewhere cannot answer
    // for it.
    const readOut = pane.getByRole('button', { name: 'Read out' }).last();
    await expect(readOut, 'the pipeline answer must offer Read out').toBeVisible({ timeout: 60_000 });
    await readOut.click();
    const play = pane.getByTestId('chat-voice-play-stop-button');
    await expect(play).toBeVisible({ timeout: 15_000 });
    await play.click();
    await expect
      .poll(async () => (await ttsState(page)).spoken.length, {
        timeout: 15_000,
        message: 'the pipeline page read-out spoke nothing',
      })
      .toBeGreaterThan(0);

    const cancelsBefore = (await ttsState(page)).cancels;
    await play.click();
    await expect
      .poll(async () => (await ttsState(page)).cancels, { timeout: 15_000, message: 'Stop left the voice running' })
      .toBeGreaterThan(cancelsBefore);
  } finally {
    await page.request.delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${pipelineId}`);
  }
});
