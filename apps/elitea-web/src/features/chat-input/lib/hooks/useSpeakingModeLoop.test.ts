/**
 * useSpeakingModeLoop.test.ts
 *
 * `useSpeakingModeLoop` reads `useSelectedProjectId()` (this slice's own
 * `api/useSelectedProjectId.ts`), which reads TanStack Router's root
 * context — `useRouteContext` throws outside ANY `<RouterProvider>`
 * ancestor, so every scenario needs a real router (never `vi.mock` — R-M1
 * bans mocking application modules outright), following the same
 * `createRootRoute`/`createRouter` technique already proven at
 * `features/toolkits/lib/hooks/useSelectedProjectId.test.tsx`.
 *
 * Changing `isSpeakingMode`/`isStreaming`/`isTTSPlaying` mid-test does NOT
 * use `renderHook`'s own `rerender()` (which works by swapping which
 * `children` element the wrapper renders under the route). Verified the
 * hard way: TanStack Router's matched-route element render is memoized by
 * match identity, not re-derived from a fresh `children` closure on every
 * parent re-render, so `rerender()`-driven prop changes were silently
 * never observed by the hook (`isRecording` never became `true` in ANY
 * scenario, including ones that don't even touch the router-swap path
 * directly, because `useSpeakingModeLoop` itself sits inside the memoized
 * route element). Instead, a small in-tree harness component
 * (`LoopHarness`) holds `{isSpeakingMode, isStreaming, isTTSPlaying}` as
 * its OWN `useState`, calls `useSpeakingModeLoop` with it, and publishes
 * both the hook's result and its own setter through a stable `apiRef` —
 * driving changes via `apiRef.current.setLoopProps(...)` triggers a normal
 * re-render of the SAME already-mounted component, never a route-element
 * swap, so the router's memoization is a non-issue.
 *
 * `useSpeakingModeLoop` unconditionally mounts BOTH
 * `useStreamingSpeechRecognition` (microphone + HTTPS upload doubles) and
 * `useSpeechRecognition` (needs `window.SpeechRecognition`) — old-app
 * parity. Every scenario starts with `isSpeakingMode: false`, waits for the
 * harness to be ready, THEN calls `setLoopProps({isSpeakingMode: true,
 * ...})` — never starts with `isSpeakingMode` already `true`. This isn't
 * just test hygiene: `useSpeechRecognition`'s own `isSupported` starts
 * `false` and only flips `true` via its OWN mount effect one render later,
 * and `useSpeakingModeLoop`'s "start/stop" effect deliberately depends on
 * `[isSpeakingMode]` ONLY (old-app parity — see that effect's own doc
 * comment) — so starting with `isSpeakingMode` ALREADY `true` captures
 * `isSupported=false` in that first effect run and never retries. Real
 * usage never hits this: `isSpeakingMode` only ever flips true well after
 * mount, in response to a user click.
 *
 * Most scenarios below run with an EMPTY ASR model list
 * (`selectAsrModel([]) === undefined`), which selects the native-browser
 * fallback and needs no Web Audio mocking; one scenario proves model
 * availability actually switches to the server path, using the
 * `__mocks__` doubles for the microphone and the transcription route.
 *
 * Fake timers: engaged only AFTER every real-time `waitFor` has already
 * settled — `@testing-library/dom`'s `waitFor` polls via real `setTimeout`,
 * so calling `vi.useFakeTimers()` before it (verified the hard way, every
 * such test hung to its 5000ms outer timeout) freezes its own polling
 * clock too. Once fake timers are active, assertions are synchronous
 * (`act(() => vi.advanceTimersByTime(...))`, then a plain `expect`), never
 * `waitFor`.
 */
import { createElement, useEffect, useState } from 'react';
import type { RefObject } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { act, render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '../../../../test/setup';
import { VoiceTransportError } from '@/shared/api/voiceTransport';
import { speechCaptureMock } from '../__mocks__/speechCapture.mock';
import { voiceTransportMock } from '../__mocks__/voiceTransport.mock';

import { selectAsrModel, useSpeakingModeLoop } from './useSpeakingModeLoop';
import type { SpeakingModeInputHandle, UseSpeakingModeLoopParams, UseSpeakingModeLoopResult } from './useSpeakingModeLoop';

const BASE = '/api/v2';

/**
 * Server-path doubles: the microphone hands the test its frame callback, and
 * every transcription upload is a promise the test answers
 * (`../__mocks__/speechCapture.mock.ts`, `../__mocks__/voiceTransport.mock.ts`).
 * The segmenter, the uploader and this hook stay real.
 */
interface ServerAsrDoubles {
  readonly frames: { push: ((frame: Float32Array) => void) | null };
  readonly uploads: Array<{ readonly resolve: (text: string) => void }>;
}

function installServerAsrDoubles(): ServerAsrDoubles {
  const doubles: ServerAsrDoubles = { frames: { push: null }, uploads: [] };
  speechCaptureMock.startSpeechCapture.mockReset();
  speechCaptureMock.startSpeechCapture.mockImplementation((onFrame) => {
    doubles.frames.push = onFrame;
    return Promise.resolve({ release: vi.fn() });
  });
  voiceTransportMock.transcribeAudio.mockReset();
  voiceTransportMock.transcribeAudio.mockImplementation(
    () =>
      new Promise<string>((resolve) => {
        doubles.uploads.push({ resolve });
      }),
  );
  return doubles;
}

/** One spoken utterance: a loud frame, then the two silent frames that end it (the segmenter's 600 ms window). */
function speak(doubles: ServerAsrDoubles): void {
  act(() => {
    doubles.frames.push?.(new Float32Array(7200).fill(0.5));
    doubles.frames.push?.(new Float32Array(7200));
    doubles.frames.push?.(new Float32Array(7200));
  });
}

/** MSW handler returning a single default batch (whisper) ASR model — selects the server path. */
function mockBatchAsrModel(): void {
  server.use(
    http.get(`${BASE}/configurations/models/:projectId`, () =>
      HttpResponse.json({ items: [{ name: 'whisper-1', project_id: 1, default: true }], total: 1 }),
    ),
  );
}

/* ── fake native SpeechRecognition (see useSpeechRecognition.test.ts) ─────── */
interface FakeResultAlternative {
  readonly transcript: string;
}
interface FakeResult extends Array<FakeResultAlternative> {
  isFinal: boolean;
}
class FakeSpeechRecognition {
  static instances: FakeSpeechRecognition[] = [];
  continuous = false;
  interimResults = false;
  lang = '';
  onresult: ((event: { resultIndex: number; results: FakeResult[] }) => void) | null = null;
  onerror: ((event: { error: string }) => void) | null = null;
  onend: (() => void) | null = null;
  start = vi.fn();
  stop = vi.fn();
  abort = vi.fn();
  constructor() {
    FakeSpeechRecognition.instances.push(this);
  }
  emitResult(resultIndex: number, results: FakeResult[]): void {
    this.onresult?.({ resultIndex, results });
  }
}
function finalResult(transcript: string): FakeResult {
  const r = [{ transcript }] as FakeResult;
  r.isFinal = true;
  return r;
}
function interimResult(transcript: string): FakeResult {
  const r = [{ transcript }] as FakeResult;
  r.isFinal = false;
  return r;
}

/* ── in-tree harness — see module doc for why this replaces renderHook's own rerender ── */
type LoopProps = Omit<UseSpeakingModeLoopParams, 'inputRef'>;

interface HarnessApi {
  readonly result: UseSpeakingModeLoopResult;
  readonly setLoopProps: (props: LoopProps) => void;
}

function LoopHarness({
  inputRef,
  apiRef,
  onError,
}: {
  inputRef: RefObject<SpeakingModeInputHandle | null>;
  apiRef: RefObject<HarnessApi | null>;
  onError?: ((message: string) => void) | undefined;
}) {
  const [props, setProps] = useState<LoopProps>({ isSpeakingMode: false, isStreaming: false, isTTSPlaying: false });
  const result = useSpeakingModeLoop({ ...props, inputRef, onError });
  // oxlint-disable-next-line react/exhaustive-deps -- intentionally no deps: this must re-run on EVERY render to keep apiRef pointing at the latest `result`/`setProps` closure, not a one-time capture.
  useEffect(() => {
    apiRef.current = { result, setLoopProps: setProps };
  });
  return null;
}

function makeInputHandle(): SpeakingModeInputHandle & { value: string; cursor: number } {
  const handle = {
    value: '',
    cursor: 0,
    getInputContent: () => handle.value,
    getCursorPosition: () => handle.cursor,
    setValue: (value: string, cursorPosition: number) => {
      handle.value = value;
      handle.cursor = cursorPosition;
    },
    sendQuestion: vi.fn(),
    reset: vi.fn(() => {
      handle.value = '';
      handle.cursor = 0;
    }),
  };
  return handle;
}

function setup(projectId = 'proj-1', onError?: (message: string) => void) {
  const inputHandle = makeInputHandle();
  const inputRef = { current: inputHandle as SpeakingModeInputHandle | null };
  const apiRef: RefObject<HarnessApi | null> = { current: null };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const rootRoute = createRootRoute({ component: () => createElement(LoopHarness, { inputRef, apiRef, onError }) });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => projectId } },
  });
  render(
    createElement(
      QueryClientProvider,
      { client: queryClient },
      createElement(RouterProvider, { router }),
    ),
  );
  return { apiRef, inputHandle, queryClient, projectId };
}

async function waitForReady(apiRef: RefObject<HarnessApi | null>): Promise<void> {
  await waitFor(() => expect(apiRef.current).not.toBeNull());
}

/** Mount idle, wait for the router match, then flip isSpeakingMode true — see module doc for why this two-step sequence (never starting with isSpeakingMode:true) is required. */
async function setupSpeaking(projectId?: string) {
  const rendered = setup(projectId);
  await waitForReady(rendered.apiRef);
  act(() => rendered.apiRef.current?.setLoopProps({ isSpeakingMode: true, isStreaming: false, isTTSPlaying: false }));
  return rendered;
}

const ASR_MODELS_QUERY_KEY = (projectId: string) => ['chat-input', 'models', projectId, 'asr', true];

/**
 * Same two-step sequence as {@link setupSpeaking}, PLUS one more real-time
 * wait in between: `useModelsList`'s query must have actually SETTLED
 * (`queryClient`'s cache entry reaches `status: 'success'`) before flipping
 * `isSpeakingMode`. This is the SAME class of race `setupSpeaking`'s own
 * "mount idle first" sequencing already solves for `isSupported` — the
 * "start/stop" effect's `[isSpeakingMode]`-only deps capture whatever
 * `asrModel` THAT render saw, and won't retry once the model list arrives a
 * tick later. It's invisible for the empty-list (native-fallback) scenarios
 * above (`selectAsrModel([])` is `undefined` whether the query is still
 * loading or has resolved to an empty list — same result either way), which
 * is why only the server-path scenarios below need this extra wait.
 */
async function setupSpeakingWithAsrModelReady(projectId = 'proj-1') {
  const rendered = setup(projectId);
  await waitForReady(rendered.apiRef);
  await waitFor(() => expect(rendered.queryClient.getQueryState(ASR_MODELS_QUERY_KEY(projectId))?.status).toBe('success'));
  act(() => rendered.apiRef.current?.setLoopProps({ isSpeakingMode: true, isStreaming: false, isTTSPlaying: false }));
  return rendered;
}

beforeEach(() => {
  FakeSpeechRecognition.instances = [];
  vi.stubGlobal('SpeechRecognition', FakeSpeechRecognition);
  configureGeneratedClient({ baseUrl: BASE });
  // Empty ASR model list by default — selects the native fallback.
  server.use(http.get(`${BASE}/configurations/models/:projectId`, () => HttpResponse.json({ items: [], total: 0 })));
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  resetGeneratedClient();
});

describe('selectAsrModel', () => {
  it('returns undefined for an empty list', () => {
    expect(selectAsrModel([])).toBeUndefined();
  });

  it('picks the whisper model over a realtime model, even when the realtime model is the default', () => {
    const items = [
      { id: '1', name: 'gpt-4o-realtime-preview', default: true },
      { id: '2', name: 'whisper-1' },
    ];
    expect(selectAsrModel(items)?.name).toBe('whisper-1');
  });

  it('returns undefined (the browser engine) when only realtime models exist', () => {
    const items = [{ id: '1', name: 'gpt-4o-realtime-preview', default: true }, { id: '2', name: 'gpt-realtime-mini' }];
    expect(selectAsrModel(items)).toBeUndefined();
  });

  it('prefers the default batch model, and counts *-transcribe as batch', () => {
    const items = [
      { id: '1', name: 'whisper-1' },
      { id: '2', name: 'gpt-4o-mini-transcribe', default: true },
      { id: '3', name: 'some-asr', default: true },
    ];
    expect(selectAsrModel(items)?.name).toBe('gpt-4o-mini-transcribe');
  });

  it('falls back to any batch model before an unknown model', () => {
    const items = [{ id: '1', name: 'some-asr', default: true }, { id: '2', name: 'whisper-1' }];
    expect(selectAsrModel(items)?.name).toBe('whisper-1');
  });

  it('falls back to the default unknown (non-realtime) model, then any', () => {
    expect(selectAsrModel([{ id: '1', name: 'asr-a' }, { id: '2', name: 'asr-b', default: true }])?.name).toBe('asr-b');
    expect(selectAsrModel([{ id: '1', name: 'asr-a' }])?.name).toBe('asr-a');
  });
});

describe('useSpeakingModeLoop (native-fallback path — empty ASR model list)', () => {
  it('starts recording via the native SpeechRecognition when isSpeakingMode becomes true', async () => {
    const { apiRef } = await setupSpeaking();

    await waitFor(() => expect(apiRef.current?.result.isRecording).toBe(true));
    expect(FakeSpeechRecognition.instances[0]?.start).toHaveBeenCalledOnce();
  });

  it('stops recording when isSpeakingMode toggles back off', async () => {
    const { apiRef } = await setupSpeaking();
    await waitFor(() => expect(apiRef.current?.result.isRecording).toBe(true));

    act(() => apiRef.current?.setLoopProps({ isSpeakingMode: false, isStreaming: false, isTTSPlaying: false }));

    await waitFor(() => expect(apiRef.current?.result.isRecording).toBe(false));
    expect(FakeSpeechRecognition.instances[0]?.stop).toHaveBeenCalled();
  });

  it('writes an interim transcript into the input at the correct cursor position', async () => {
    const { inputHandle } = await setupSpeaking();
    await waitFor(() => expect(FakeSpeechRecognition.instances).toHaveLength(1));

    act(() => FakeSpeechRecognition.instances[0]?.emitResult(0, [interimResult('hello')]));

    expect(inputHandle.value).toBe('hello');
    expect(inputHandle.cursor).toBe('hello'.length);
  });

  it('accumulates final segments separated by a space', async () => {
    const { inputHandle } = await setupSpeaking();
    await waitFor(() => expect(FakeSpeechRecognition.instances).toHaveLength(1));
    const recognizer = FakeSpeechRecognition.instances[0];

    act(() => recognizer?.emitResult(0, [finalResult('hello')]));
    act(() => recognizer?.emitResult(0, [finalResult('world')]));

    expect(inputHandle.value).toBe('hello world');
  });

  it('resyncs pre/post cursor around a manual edit detected between transcript events', async () => {
    const { inputHandle } = await setupSpeaking();
    await waitFor(() => expect(FakeSpeechRecognition.instances).toHaveLength(1));
    const recognizer = FakeSpeechRecognition.instances[0];

    act(() => recognizer?.emitResult(0, [finalResult('hello')]));
    expect(inputHandle.value).toBe('hello');

    // Manual edit: user clears the field while still "recording".
    inputHandle.value = '';
    inputHandle.cursor = 0;

    act(() => recognizer?.emitResult(0, [finalResult('world')]));
    // Re-synced base is the (now-empty) current content, not the stale 'hello'.
    expect(inputHandle.value).toBe('world');
  });

  it('issue 930: a FINAL transcript on the browser-fallback path arms the auto-send timer on its own', async () => {
    const { inputHandle } = await setupSpeaking();
    await waitFor(() => expect(FakeSpeechRecognition.instances).toHaveLength(1));
    const recognizer = FakeSpeechRecognition.instances[0];

    // Fake timers BEFORE the emit: the timer this test is about is armed by
    // the emit itself, and a timer scheduled on the real clock is not
    // advanced by `advanceTimersByTime` (see this file's module doc).
    vi.useFakeTimers();
    act(() => recognizer?.emitResult(0, [finalResult('send me please')]));
    expect(inputHandle.value).toBe('send me please');
    // Nothing has been sent yet — the silence window has not elapsed.
    expect(inputHandle.sendQuestion).not.toHaveBeenCalled();

    void act(() => vi.advanceTimersByTime(3600));

    expect(inputHandle.sendQuestion).toHaveBeenCalledOnce();
  });

  it('issue 930: a second utterance inside the silence window re-arms the timer instead of sending twice', async () => {
    const { inputHandle } = await setupSpeaking();
    await waitFor(() => expect(FakeSpeechRecognition.instances).toHaveLength(1));
    const recognizer = FakeSpeechRecognition.instances[0];

    vi.useFakeTimers();
    act(() => recognizer?.emitResult(0, [finalResult('Tell me about computers.')]));
    void act(() => vi.advanceTimersByTime(1200));
    expect(inputHandle.sendQuestion).not.toHaveBeenCalled();

    act(() => recognizer?.emitResult(0, [finalResult('And include milestones.')]));
    void act(() => vi.advanceTimersByTime(3600));

    expect(inputHandle.value).toBe('');
    expect(inputHandle.sendQuestion).toHaveBeenCalledOnce();
  });

  it('notifyManualEdit reschedules an auto-send while speaking mode is active and nothing has been sent yet', async () => {
    const { apiRef, inputHandle } = await setupSpeaking();
    await waitFor(() => expect(apiRef.current?.result.isRecording).toBe(true));
    inputHandle.value = 'typed by hand';

    vi.useFakeTimers();
    act(() => apiRef.current?.result.notifyManualEdit());
    void act(() => vi.advanceTimersByTime(3600));

    expect(inputHandle.sendQuestion).toHaveBeenCalledOnce();
  });

  it('notifyManualEdit is a no-op when speaking mode is off', async () => {
    const { apiRef, inputHandle } = setup();
    await waitForReady(apiRef);

    vi.useFakeTimers();
    act(() => apiRef.current?.result.notifyManualEdit());
    void act(() => vi.advanceTimersByTime(5000));

    expect(inputHandle.sendQuestion).not.toHaveBeenCalled();
  });

  it('pauseForRegeneration stops recording and prevents an immediate auto-send', async () => {
    const { apiRef } = await setupSpeaking();
    await waitFor(() => expect(apiRef.current?.result.isRecording).toBe(true));

    act(() => apiRef.current?.result.pauseForRegeneration());

    await waitFor(() => expect(apiRef.current?.result.isRecording).toBe(false));
  });

  it('restarts recording once isStreaming/isTTSPlaying both clear after a send', async () => {
    const { apiRef } = await setupSpeaking();
    await waitFor(() => expect(apiRef.current?.result.isRecording).toBe(true));

    // AI starts responding — recording pauses.
    act(() => apiRef.current?.setLoopProps({ isSpeakingMode: true, isStreaming: true, isTTSPlaying: false }));
    await waitFor(() => expect(apiRef.current?.result.isRecording).toBe(false));

    // AI finishes — recording resumes for the next turn.
    act(() => apiRef.current?.setLoopProps({ isSpeakingMode: true, isStreaming: false, isTTSPlaying: false }));
    await waitFor(() => expect(apiRef.current?.result.isRecording).toBe(true));
  });
});

describe('useSpeakingModeLoop (server path is selected once an ASR model is available)', () => {
  it('opens the microphone (the server ASR path) instead of the native recognizer when a model resolves', async () => {
    mockBatchAsrModel();
    installServerAsrDoubles();

    await setupSpeakingWithAsrModelReady();

    await waitFor(() => expect(speechCaptureMock.startSpeechCapture).toHaveBeenCalled());
    // The native recognizer must NOT have been used for this session.
    expect(FakeSpeechRecognition.instances).toHaveLength(0);
  });

  /**
   * `handleTranscriptDone` (this hook's own `scheduleSend` trigger) is ONLY
   * ever invoked by `useStreamingSpeechRecognition`'s `onTranscriptDone` —
   * the native `useSpeechRecognition` fallback has no equivalent "done"
   * signal. So the silence-timeout scenarios below run through the server
   * path: an utterance is cut on silence (`onVadFlush`), uploaded, and its
   * transcript answered by the test (`onTranscript` + `onTranscriptDone`).
   */
  async function setupSpeakingServer(): Promise<ReturnType<typeof setup> & { readonly doubles: ServerAsrDoubles }> {
    mockBatchAsrModel();
    const doubles = installServerAsrDoubles();
    const rendered = await setupSpeakingWithAsrModelReady();
    await waitFor(() => expect(doubles.frames.push).not.toBeNull());
    return { ...rendered, doubles };
  }

  async function answer(doubles: ServerAsrDoubles, index: number, text: string): Promise<void> {
    await act(async () => {
      doubles.uploads[index]?.resolve(text);
      for (let i = 0; i < 5; i++) await Promise.resolve();
    });
  }

  it('auto-sends SILENCE_TIMEOUT_MS after the user stopped speaking, once the transcript arrives, then stops recording', async () => {
    const { inputHandle, doubles } = await setupSpeakingServer();

    vi.useFakeTimers();
    speak(doubles);
    expect(doubles.uploads).toHaveLength(1);
    await answer(doubles, 0, 'hello world');
    expect(inputHandle.value).toBe('hello world');

    // The flush back-dates "stopped speaking" by VAD_SILENCE_MS (600), so the
    // send fires about 2400 ms after the transcript, not 3000.
    void act(() => vi.advanceTimersByTime(2300));
    expect(inputHandle.sendQuestion).not.toHaveBeenCalled();
    void act(() => vi.advanceTimersByTime(200));

    expect(inputHandle.sendQuestion).toHaveBeenCalledOnce();
    expect(inputHandle.reset).toHaveBeenCalledOnce();
  });

  it('does not auto-send when the accumulated content is only whitespace', async () => {
    const { inputHandle, doubles } = await setupSpeakingServer();

    vi.useFakeTimers();
    speak(doubles);
    await answer(doubles, 0, '   ');
    void act(() => vi.advanceTimersByTime(3600));

    expect(inputHandle.sendQuestion).not.toHaveBeenCalled();
  });

  it('backdates the silence timer by the time the transcription took (Whisper/VAD path)', async () => {
    const { inputHandle, doubles } = await setupSpeakingServer();

    vi.useFakeTimers();
    speak(doubles);
    // 1000 ms of transcription time between the flush and the transcript.
    void act(() => vi.advanceTimersByTime(1000));
    await answer(doubles, 0, 'hello world');

    // adjustedDelay = max(200, 3000 - (1000 + 600)) = 1400 ms.
    void act(() => vi.advanceTimersByTime(1500));

    expect(inputHandle.sendQuestion).toHaveBeenCalledOnce();
  });

  it('a server transcription failure reaches onError as a readable message', async () => {
    mockBatchAsrModel();
    const doubles = installServerAsrDoubles();
    voiceTransportMock.transcribeAudio.mockImplementation(() => Promise.reject(new VoiceTransportError('model-unavailable')));
    const onError = vi.fn();
    const rendered = setup('proj-1', onError);
    await waitForReady(rendered.apiRef);
    await waitFor(() => expect(rendered.queryClient.getQueryState(ASR_MODELS_QUERY_KEY('proj-1'))?.status).toBe('success'));
    act(() => rendered.apiRef.current?.setLoopProps({ isSpeakingMode: true, isStreaming: false, isTTSPlaying: false }));
    await waitFor(() => expect(doubles.frames.push).not.toBeNull());

    speak(doubles);

    await waitFor(() => expect(onError).toHaveBeenCalledWith('The speech model is not available. Check the AI configuration.'));
  });

  it('a denied microphone on the server path reaches onError as a readable message', async () => {
    mockBatchAsrModel();
    installServerAsrDoubles();
    speechCaptureMock.startSpeechCapture.mockImplementation(() => Promise.reject(new DOMException('denied', 'NotAllowedError')));
    const onError = vi.fn();
    const rendered = setup('proj-1', onError);
    await waitForReady(rendered.apiRef);
    await waitFor(() => expect(rendered.queryClient.getQueryState(ASR_MODELS_QUERY_KEY('proj-1'))?.status).toBe('success'));
    act(() => rendered.apiRef.current?.setLoopProps({ isSpeakingMode: true, isStreaming: false, isTTSPlaying: false }));

    await waitFor(() =>
      expect(onError).toHaveBeenCalledWith(
        'Microphone access denied. Please allow microphone access in your browser settings.',
      ),
    );
  });
});
