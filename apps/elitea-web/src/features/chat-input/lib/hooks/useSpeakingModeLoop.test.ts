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
 * `useStreamingSpeechRecognition` (needs `useSocketClient()`) and
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
 * availability actually switches to the server path, reusing
 * `useStreamingSpeechRecognition.test.ts`'s own Web Audio fake technique.
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
import { SocketClientContext, createSocketClient } from '@/shared/api/socket/client';
import type { SocketIoFactory } from '@/shared/api/socket/client';
import { server } from '../../../../test/setup';

import { selectAsrModel, useSpeakingModeLoop } from './useSpeakingModeLoop';
import type { SpeakingModeInputHandle, UseSpeakingModeLoopParams, UseSpeakingModeLoopResult } from './useSpeakingModeLoop';

const BASE = '/api/v2';

/* ── socket double (see useStreamingSpeechRecognition.test.ts for the full rationale) ── */
function createFakeSocket() {
  const listeners = new Map<string, Set<(...args: unknown[]) => void>>();
  const emittedCalls: Array<{ event: string; payload: unknown }> = [];
  const fake = {
    on: (event: string, handler: (...args: unknown[]) => void) => {
      let set = listeners.get(event);
      if (!set) {
        set = new Set();
        listeners.set(event, set);
      }
      set.add(handler);
    },
    off: (event: string, handler: (...args: unknown[]) => void) => listeners.get(event)?.delete(handler),
    emit: (event: string, payload: unknown) => {
      emittedCalls.push({ event, payload });
      return fake;
    },
    disconnect: vi.fn(),
    io: { on: vi.fn(), off: vi.fn() },
    trigger(event: string, ...args: unknown[]) {
      for (const h of listeners.get(event) ?? []) h(...args);
    },
    emittedCalls,
  };
  return fake;
}

function makeSocketClient() {
  const fakeSocket = createFakeSocket();
  // `ReturnType<SocketIoFactory>`, not `import type { Socket } from
  // 'socket.io-client'` — R-A3's `no-restricted-imports` bans that import
  // outside `shared/api/socket/**`.
  const ioFactory = vi.fn(() => fakeSocket as unknown as ReturnType<SocketIoFactory>) as unknown as SocketIoFactory;
  const client = createSocketClient({ url: 'http://socket.test', ioFactory });
  return { client, fakeSocket };
}

/**
 * Minimal Web Audio / getUserMedia fakes (see
 * useStreamingSpeechRecognition.test.ts for the fuller version and its own
 * "no established pattern" rationale) — just enough for `startRecording()`
 * to resolve without throwing, so the server-path scenarios below can drive
 * the REST of the flow through the socket double.
 */
function installServerAsrEnvironment(): { readonly getUserMedia: ReturnType<typeof vi.fn> } {
  const getUserMedia = vi.fn(() =>
    Promise.resolve({ getTracks: () => [{ stop: vi.fn() }] } as unknown as MediaStream),
  );
  Object.defineProperty(navigator, 'mediaDevices', { value: { getUserMedia }, configurable: true });
  class FakeAudioContext {
    sampleRate = 44100;
    state: 'running' | 'closed' = 'running';
    destination = {};
    audioWorklet = { addModule: vi.fn(() => Promise.resolve()) };
    close = vi.fn(() => Promise.resolve());
    createMediaStreamSource = vi.fn(() => ({ connect: vi.fn() }));
    createGain = vi.fn(() => ({ gain: { value: 0 }, connect: vi.fn() }));
  }
  class FakeAudioWorkletNode {
    port = { postMessage: vi.fn(), onmessage: null };
    connect = vi.fn();
    disconnect = vi.fn();
    constructor(_ctx: unknown, _name: string) {}
  }
  vi.stubGlobal('AudioContext', FakeAudioContext);
  vi.stubGlobal('AudioWorkletNode', FakeAudioWorkletNode);
  // Extends the REAL URL constructor (not a plain object spread of it — that
  // silently strips constructibility and broke `shared/api/http.ts`'s own
  // `new URL(...)` call with 'URL is not a constructor', the hard way) purely
  // to add the two static methods Node/jsdom's URL doesn't implement.
  vi.stubGlobal(
    'URL',
    class extends URL {
      static override createObjectURL = vi.fn(() => 'blob:fake');
      static override revokeObjectURL = vi.fn();
    },
  );
  return { getUserMedia };
}

/** MSW handler returning a single default streaming (non-whisper) ASR model — selects the server path. */
function mockStreamingAsrModel(): void {
  server.use(
    http.get(`${BASE}/configurations/models/:projectId`, () =>
      HttpResponse.json({ items: [{ name: 'gpt-4o-realtime', project_id: 1, default: true }], total: 1 }),
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
}: {
  inputRef: RefObject<SpeakingModeInputHandle | null>;
  apiRef: RefObject<HarnessApi | null>;
}) {
  const [props, setProps] = useState<LoopProps>({ isSpeakingMode: false, isStreaming: false, isTTSPlaying: false });
  const result = useSpeakingModeLoop({ ...props, inputRef });
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

function setup(projectId = 'proj-1') {
  const { client, fakeSocket } = makeSocketClient();
  const inputHandle = makeInputHandle();
  const inputRef = { current: inputHandle as SpeakingModeInputHandle | null };
  const apiRef: RefObject<HarnessApi | null> = { current: null };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const rootRoute = createRootRoute({ component: () => createElement(LoopHarness, { inputRef, apiRef }) });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => projectId } },
  });
  render(
    createElement(
      QueryClientProvider,
      { client: queryClient },
      createElement(SocketClientContext.Provider, { value: client }, createElement(RouterProvider, { router })),
    ),
  );
  return { apiRef, inputHandle, client, fakeSocket, queryClient, projectId };
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

  it('prefers the default streaming model over any other', () => {
    const items = [
      { id: '1', name: 'gpt-realtime-a' },
      { id: '2', name: 'gpt-realtime-b', default: true },
      { id: '3', name: 'whisper-1', default: true },
    ];
    expect(selectAsrModel(items)?.name).toBe('gpt-realtime-b');
  });

  it('falls back to any streaming model when none is marked default', () => {
    const items = [{ id: '1', name: 'gpt-realtime-a' }, { id: '2', name: 'whisper-1', default: true }];
    expect(selectAsrModel(items)?.name).toBe('gpt-realtime-a');
  });

  it('falls back to the default whisper model when no streaming model exists', () => {
    const items = [{ id: '1', name: 'whisper-1' }, { id: '2', name: 'whisper-2', default: true }];
    expect(selectAsrModel(items)?.name).toBe('whisper-2');
  });

  it('falls back to any whisper model as a last resort', () => {
    const items = [{ id: '1', name: 'whisper-1' }];
    expect(selectAsrModel(items)?.name).toBe('whisper-1');
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
  it('calls getUserMedia (the server-side ASR path) instead of the native recognizer when a model resolves', async () => {
    mockStreamingAsrModel();
    const { getUserMedia } = installServerAsrEnvironment();

    await setupSpeakingWithAsrModelReady();

    await waitFor(() => expect(getUserMedia).toHaveBeenCalled());
    // The native recognizer must NOT have been used for this session.
    expect(FakeSpeechRecognition.instances).toHaveLength(0);
  });

  /**
   * `handleTranscriptDone` (this hook's own `scheduleSend` trigger) is ONLY
   * ever invoked by `useStreamingSpeechRecognition`'s `onTranscriptDone` —
   * the native `useSpeechRecognition` fallback has no equivalent "done"
   * signal at all (old-app parity: `handleTranscript`'s own `if (final)`
   * branch never calls `scheduleSend`). So the silence-timeout/EWMA
   * scenarios below MUST run through the server path — asserting them
   * against the native-fallback path (an earlier draft of this suite did)
   * silently passed for the wrong reason, since `scheduleSend` never fires
   * there regardless of transcript content.
   */
  async function setupSpeakingServer(): Promise<ReturnType<typeof setup>> {
    mockStreamingAsrModel();
    installServerAsrEnvironment();
    const rendered = await setupSpeakingWithAsrModelReady();
    // acceptEventsRef flips true (and asr_start is emitted) once
    // getUserMedia/audioWorklet.addModule's mocked promises resolve —
    // wait for that real async settling before driving fake-socket events.
    await waitFor(() => expect(rendered.fakeSocket.emittedCalls.some((c) => c.event === 'asr_start')).toBe(true));
    return rendered;
  }

  it('auto-sends after SILENCE_TIMEOUT_MS + the EWMA latency estimate once transcript_done fires, then stops recording', async () => {
    const { inputHandle, fakeSocket } = await setupSpeakingServer();

    vi.useFakeTimers();
    act(() => fakeSocket.trigger('asr_transcript_done', { transcript: 'hello world' }));
    // SILENCE_TIMEOUT_MS (3000) + latency estimate (500 initial, no vad_flush was fired).
    void act(() => vi.advanceTimersByTime(3500));

    expect(inputHandle.sendQuestion).toHaveBeenCalledOnce();
    expect(inputHandle.reset).toHaveBeenCalledOnce();
  });

  it('does not auto-send when the accumulated content is only whitespace', async () => {
    const { inputHandle, fakeSocket } = await setupSpeakingServer();

    vi.useFakeTimers();
    act(() => fakeSocket.trigger('asr_transcript_done', { transcript: '   ' }));
    void act(() => vi.advanceTimersByTime(3600));

    expect(inputHandle.sendQuestion).not.toHaveBeenCalled();
  });

  it('backdates the silence timer by VAD_SILENCE_MS when a vad_flush preceded transcript_done (Whisper/VAD path)', async () => {
    const { inputHandle, fakeSocket } = await setupSpeakingServer();

    vi.useFakeTimers();
    act(() => fakeSocket.trigger('asr_vad_flush', {}));
    // Simulate 1000ms of processing time between the flush and the transcript arriving.
    void act(() => vi.advanceTimersByTime(1000));
    act(() => fakeSocket.trigger('asr_transcript_done', { transcript: 'hello world' }));

    // adjustedDelay = max(200, SILENCE_TIMEOUT_MS(3000) - elapsed(~1000)) ≈ 2000ms,
    // NOT the realtime-path's 3500ms — advancing only 2100ms must already fire it.
    void act(() => vi.advanceTimersByTime(2100));

    expect(inputHandle.sendQuestion).toHaveBeenCalledOnce();
  });
});
