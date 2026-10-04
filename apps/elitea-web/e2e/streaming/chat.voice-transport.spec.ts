/**
 * VOICE WITHOUT SOCKET.IO — dictation and read-aloud over the `/llm` audio routes.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS FILE PROVES
 * ─────────────────────────────────────────────────────────────────────────────
 * elitea-main runs no socket.io server (removed in #126), and voice used to
 * talk to one: dictation streamed PCM as `asr_audio_chunk` events and
 * read-aloud waited for `tts_audio_chunk` events that never came. Both now go
 * over plain HTTPS through the session-authenticated `/llm` edge and the
 * gateway (`features/chat-input/api/voiceTransport.ts`):
 *
 *  1. DICTATION. A project with a transcription (`asr`) model: the mic opens,
 *     the browser cuts the utterance on silence, uploads ONE WAV to
 *     `POST /llm/v1/audio/transcriptions`, and the transcript the provider
 *     returned lands in the composer. The mock journal shows the upload
 *     reached the provider under this test's own credential, and no request
 *     went to `/socket.io`.
 *  2. A MODEL THAT CANNOT SPEAK. bifrost's vLLM adapter — the only class that
 *     can reach this offline mock (see `deploy/mock-llm/server.py`'s module
 *     docstring) — implements transcription but refuses speech. The gateway
 *     answers that refusal with 501 `unsupported_operation`, and "Read out"
 *     must SAY the model is not available instead of arming a silent player.
 *     That is the UX the regression found missing: voice failed with no
 *     message at all.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE MICROPHONE
 * ─────────────────────────────────────────────────────────────────────────────
 * Headless Chromium has no microphone. `getUserMedia` is replaced before the
 * page loads with a real Web Audio graph: an oscillator behind a gain the
 * test turns up ("speaking") and down ("silence"). Everything after that is
 * the product — the AudioWorklet, the silence segmenter, the WAV encoder and
 * the upload.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext, Page } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  deleteConversation,
  expectStoredAssistantAnswer,
  readMockLlmJournal,
} from '../fixtures/api';

/** The offline mock `standalone-stack.sh` points the chat model at, reached as a vLLM-class credential. */
const MOCK_UPSTREAM_BASE = 'http://llm-mock:8090';
/** The chat model the stack seeds (the read-aloud journey needs one answer to read). */
const CHAT_MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';
/** What `deploy/mock-llm/server.py` answers for every transcription (`TRANSCRIPT_TEXT`). */
const MOCK_TRANSCRIPT = 'MOCK TRANSCRIPT from the speech model';

const ASR_MODELS_RE = /\/configurations\/models\/(\d+)\?.*section=asr/;
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;

interface VoiceModel {
  readonly credentialId: string;
  readonly modelId: string;
  readonly mockKey: string;
}

/**
 * A credential plus one speech model row linked to it, through the product
 * API — the same write route `deploy/scripts/seed-llm-api.py` uses. The mock
 * key names this run in the mock journal.
 */
async function createVoiceModel(
  request: APIRequestContext,
  projectId: string,
  kind: 'asr' | 'tts',
  providerModel: string,
): Promise<VoiceModel> {
  const suffix = `${kind}-${Date.now().toString(36)}`;
  const credentialTitle = `${AUTOTEST_PREFIX}voice-cred-${suffix}`;
  const mockKey = `mock-key-voice-${suffix}`;
  const credential = await request.post(`${API_BASE}/configurations/configurations/${projectId}`, {
    data: {
      elitea_title: credentialTitle,
      label: credentialTitle,
      type: 'vllm',
      section: 'ai_credentials',
      data: { api_key: mockKey, api_base: MOCK_UPSTREAM_BASE },
      shared: false,
    },
  });
  expect(credential.status(), `the ${kind} credential must be created: ${(await credential.text()).slice(0, 300)}`).toBeLessThan(300);
  const credentialId = String(((await credential.json()) as { id?: string | number }).id ?? '');

  const title = `${AUTOTEST_PREFIX}voice-${suffix}`;
  const model = await request.post(`${API_BASE}/configurations/configurations/${projectId}`, {
    data: {
      elitea_title: title,
      label: title,
      type: `${kind}_model`,
      section: kind,
      data: { name: providerModel, ai_credentials: { elitea_title: credentialTitle, private: false } },
      shared: false,
    },
  });
  expect(model.status(), `the ${kind} model row must be created: ${(await model.text()).slice(0, 300)}`).toBeLessThan(300);
  const modelBody = (await model.json()) as { id?: string | number; status_ok?: boolean };
  expect(modelBody.status_ok, `the ${kind} model row must be admitted (status_ok) — the gateway selects on it`).toBe(true);
  return { credentialId, modelId: String(modelBody.id ?? ''), mockKey };
}

async function deleteVoiceModel(request: APIRequestContext, projectId: string, model: VoiceModel | undefined): Promise<void> {
  if (!model || !projectId) return;
  for (const id of [model.modelId, model.credentialId]) {
    if (id) await request.delete(`${API_BASE}/configurations/configuration/${projectId}/${id}`).catch(() => {});
  }
}

/** Replaces the microphone with an oscillator behind a gain the test controls (see the file header). */
async function installFakeMicrophone(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const w = window as unknown as { __autotestMic?: { setLevel: (level: number) => void } };
    navigator.mediaDevices.getUserMedia = async () => {
      const ctx = new AudioContext();
      await ctx.resume();
      const oscillator = ctx.createOscillator();
      const gain = ctx.createGain();
      gain.gain.value = 0;
      const destination = ctx.createMediaStreamDestination();
      oscillator.connect(gain);
      gain.connect(destination);
      oscillator.start();
      w.__autotestMic = {
        setLevel: (level: number) => {
          gain.gain.value = level;
        },
      };
      return destination.stream;
    };
  });
}

async function setMicLevel(page: Page, level: number): Promise<void> {
  await page.evaluate((value) => {
    (window as unknown as { __autotestMic?: { setLevel: (level: number) => void } }).__autotestMic?.setLevel(value);
  }, level);
}

/** Opens the chat and answers the project the composer's ASR lookup is scoped to. */
async function openChatAndReadProject(page: Page): Promise<string> {
  const asrLookup = page.waitForResponse((r) => ASR_MODELS_RE.test(r.url()), { timeout: 30_000 });
  await page.goto(`${BASE_URL}/app/chat`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
  const projectId = ASR_MODELS_RE.exec((await asrLookup).url())?.[1] ?? '';
  expect(projectId, 'the composer must look up transcription models for a real project').toMatch(/^\d+$/);
  return projectId;
}

test('dictation transcribes through the project speech model over HTTPS — no socket.io', async ({ page }) => {
  test.setTimeout(180_000);
  const socketRequests: string[] = [];
  page.on('request', (r) => {
    if (r.url().includes('/socket.io')) socketRequests.push(r.url());
  });
  await installFakeMicrophone(page);

  let projectId = '';
  let model: VoiceModel | undefined;
  try {
    projectId = await openChatAndReadProject(page);
    model = await createVoiceModel(page.request, projectId, 'asr', 'whisper-1');

    // Reload so the composer's model lookup sees the new transcription model.
    const asrLookup = page.waitForResponse((r) => ASR_MODELS_RE.test(r.url()), { timeout: 30_000 });
    await page.reload({ waitUntil: 'domcontentloaded' });
    expect((await asrLookup).status(), 'the transcription model lookup must answer').toBe(200);

    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });
    const mic = page.locator('button[aria-pressed]');
    await expect(mic).toBeVisible({ timeout: 20_000 });

    const upload = page.waitForResponse(
      (r) => new URL(r.url()).pathname === '/llm/v1/audio/transcriptions' && r.request().method() === 'POST',
      { timeout: 60_000 },
    );
    await mic.click();
    await expect(mic).toHaveAttribute('aria-pressed', 'true');

    // Speak for a second, then go quiet: the segmenter ends the utterance
    // after 600 ms of silence and uploads it.
    await setMicLevel(page, 0.5);
    await page.waitForTimeout(1_200);
    await setMicLevel(page, 0);

    const uploaded = await upload;
    expect(uploaded.status(), `the transcription upload was refused: ${(await uploaded.text()).slice(0, 300)}`).toBe(200);
    expect(uploaded.request().headers()['x-project-id'], 'the upload names the project the edge bills').toBe(projectId);
    await expect(input, 'the provider transcript must land in the composer').toHaveValue(new RegExp(MOCK_TRANSCRIPT), {
      timeout: 20_000,
    });

    await page.getByRole('button', { name: 'stop voice input' }).click();
    await expect(mic).toHaveAttribute('aria-pressed', 'false');

    const journal = await readMockLlmJournal(page);
    const transcriptions = journal.filter((e) => e.path === '/v1/audio/transcriptions' && e.credential === model?.mockKey);
    expect(transcriptions.length, 'the upload must reach the provider under this run’s own credential').toBeGreaterThan(0);
    expect(socketRequests, 'voice must not touch the removed socket.io server').toEqual([]);
  } finally {
    await deleteVoiceModel(page.request, projectId, model);
  }
});

test('read-aloud with a speech model whose provider cannot speak says so, instead of staying silent', async ({ page }) => {
  test.setTimeout(300_000);
  const token = `AUTOTESTTTS${Date.now().toString(36).toUpperCase()}`;
  const prompt = `autotest echo exactly: ${token}`;

  let projectId = '';
  let conversationId = '';
  let model: VoiceModel | undefined;
  try {
    await page.goto(`${BASE_URL}/app/chat`);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
    await page.getByTestId('model-selector-button').click();
    const modelOption = page.getByRole('menuitem').filter({ hasText: CHAT_MODEL_NAME }).first();
    await expect(modelOption, `the seeded model ${CHAT_MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
    await modelOption.click();

    // ── one answer to read ──────────────────────────────────────────────
    const created = page.waitForResponse(
      (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
      { timeout: 45_000 },
    );
    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });
    await input.fill(prompt);
    await page.getByTestId('chat-send-button').click();
    const createdResponse = await created;
    projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
    conversationId = ((await createdResponse.json()) as { id?: string }).id ?? '';
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 120_000,
      contains: token,
      message: 'the turn must store its answer before it can be read aloud',
    });

    // ── a TTS model the provider cannot serve ───────────────────────────
    model = await createVoiceModel(page.request, projectId, 'tts', 'tts-1');
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`, { waitUntil: 'domcontentloaded' });
    const answer = page.getByTestId('application-answer').last();
    await expect(answer).toContainText(token, { timeout: 30_000 });

    const speech = page.waitForResponse(
      (r) => new URL(r.url()).pathname === '/llm/v1/audio/speech' && r.request().method() === 'POST',
      { timeout: 30_000 },
    );
    await answer.getByRole('button', { name: 'Read out' }).click();

    const refused = await speech;
    expect(refused.status(), 'the vLLM-class provider cannot synthesise speech; the gateway must say so with 501').toBe(501);
    const alert = page.getByTestId('chat-voice-error');
    await expect(alert, 'read-aloud must explain the failure, not stay silent').toBeVisible({ timeout: 10_000 });
    await expect(alert).toContainText('The speech model is not available');
    await expect(page.getByTestId('chat-voice-mini-player'), 'no silent player may stay armed').toHaveCount(0);
  } finally {
    await deleteVoiceModel(page.request, projectId, model);
    if (projectId && conversationId) await deleteConversation(page.request, conversationId, projectId).catch(() => {});
  }
});
