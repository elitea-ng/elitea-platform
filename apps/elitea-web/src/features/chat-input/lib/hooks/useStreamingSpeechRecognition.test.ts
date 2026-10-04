/**
 * useStreamingSpeechRecognition — server recognition over HTTPS.
 *
 * The microphone (`../helpers/speechCapture.ts`) and the transcription route
 * (`../../api/voiceTransport.ts`) are replaced with doubles: the capture
 * double hands the test the frame callback, so a test "speaks" by pushing
 * loud frames and "pauses" by pushing silent ones, and every upload is a
 * promise the test answers. Everything between them — the segmenter, the
 * one-request-at-a-time uploader and the callbacks — is the real code.
 */
import { act, renderHook } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { speechCaptureMock } from '../__mocks__/speechCapture.mock';
import { voiceTransportMock } from '../__mocks__/voiceTransport.mock';

import type { ModelListItem } from '../../api/models';
import { VoiceTransportError } from '../../api/voiceTransport';
import type { TranscriptionRequest } from '../../api/voiceTransport';

import { useStreamingSpeechRecognition } from './useStreamingSpeechRecognition';

const mocks = { startSpeechCapture: speechCaptureMock.startSpeechCapture, transcribeAudio: voiceTransportMock.transcribeAudio };

interface Upload {
  readonly request: TranscriptionRequest;
  readonly signal: AbortSignal | undefined;
  readonly resolve: (text: string) => void;
  readonly reject: (err: unknown) => void;
}

let onFrame: ((frame: Float32Array) => void) | null;
let release: ReturnType<typeof vi.fn<() => void>>;
let uploads: Upload[];

const MODEL: ModelListItem = { id: '1_whisper-1', name: 'whisper-1', project_id: 1 };
const LOUD = (): Float32Array => new Float32Array(7200).fill(0.5);
const QUIET = (): Float32Array => new Float32Array(7200);

beforeEach(() => {
  onFrame = null;
  release = vi.fn<() => void>();
  uploads = [];
  mocks.startSpeechCapture.mockReset();
  mocks.startSpeechCapture.mockImplementation((cb: (frame: Float32Array) => void) => {
    onFrame = cb;
    return Promise.resolve({ release });
  });
  mocks.transcribeAudio.mockReset();
  mocks.transcribeAudio.mockImplementation(
    (request: TranscriptionRequest, signal?: AbortSignal) =>
      new Promise<string>((resolve, reject) => {
        uploads.push({ request, signal, resolve, reject });
      }),
  );
});

function push(frame: Float32Array): void {
  act(() => onFrame?.(frame));
}

/** One utterance: speech, then the two silent frames that end it. */
function speakUtterance(): void {
  push(LOUD());
  push(QUIET());
  push(QUIET());
}

async function settle(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
}

function renderRecognition(extra: Partial<Parameters<typeof useStreamingSpeechRecognition>[0]> = {}) {
  const callbacks = {
    onTranscript: vi.fn(),
    onTranscriptDone: vi.fn(),
    onSpeechStarted: vi.fn(),
    onVadFlush: vi.fn(),
    onError: vi.fn(),
  };
  const hook = renderHook(() => useStreamingSpeechRecognition({ ...callbacks, projectId: 'p-9', asrModel: MODEL, ...extra }));
  return { ...hook, callbacks };
}

describe('useStreamingSpeechRecognition', () => {
  it('isSupported needs both a transcription model and a project', () => {
    const initialProps: { asrModel: ModelListItem | undefined; projectId: string | undefined } = { asrModel: undefined, projectId: 'p' };
    const { result, rerender } = renderHook(
      ({ asrModel, projectId }) => useStreamingSpeechRecognition({ asrModel, projectId }),
      { initialProps },
    );
    expect(result.current.isSupported).toBe(false);
    rerender({ asrModel: MODEL, projectId: undefined });
    expect(result.current.isSupported).toBe(false);
    rerender({ asrModel: MODEL, projectId: 'p' });
    expect(result.current.isSupported).toBe(true);
  });

  it('startRecording opens the microphone and flips isRecording', async () => {
    const { result } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });
    expect(mocks.startSpeechCapture).toHaveBeenCalledOnce();
    expect(result.current.isRecording).toBe(true);
  });

  it('one utterance: speech-start, then a flush on silence, then one WAV upload to the project with the model', async () => {
    const { result, callbacks } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });

    push(LOUD());
    expect(callbacks.onSpeechStarted).toHaveBeenCalledOnce();
    expect(uploads).toHaveLength(0);

    push(QUIET());
    push(QUIET());
    expect(callbacks.onVadFlush).toHaveBeenCalledOnce();
    expect(uploads).toHaveLength(1);
    expect(uploads[0]?.request).toMatchObject({ projectId: 'p-9', model: 'whisper-1' });
    expect(uploads[0]?.request.audio.type).toBe('audio/wav');

    uploads[0]?.resolve('hello there');
    await settle();
    expect(callbacks.onTranscript).toHaveBeenCalledWith({ interim: '', final: 'hello there' });
    expect(callbacks.onTranscriptDone).toHaveBeenCalledOnce();
  });

  it('silence before any speech uploads nothing', async () => {
    const { result } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });
    push(QUIET());
    push(QUIET());
    push(QUIET());
    expect(uploads).toHaveLength(0);
  });

  it('an empty transcript still calls onTranscriptDone, so the speaking-mode counter stays balanced', async () => {
    const { result, callbacks } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });
    speakUtterance();
    uploads[0]?.resolve('');
    await settle();
    expect(callbacks.onTranscript).not.toHaveBeenCalled();
    expect(callbacks.onTranscriptDone).toHaveBeenCalledOnce();
  });

  it('utterances that end while an upload is out are held and sent together, with one onTranscriptDone each', async () => {
    const { result, callbacks } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });
    speakUtterance();
    speakUtterance();
    speakUtterance();
    expect(uploads).toHaveLength(1);

    uploads[0]?.resolve('one');
    await settle();
    expect(uploads).toHaveLength(2);
    expect(callbacks.onTranscriptDone).toHaveBeenCalledTimes(1);

    uploads[1]?.resolve('two three');
    await settle();
    expect(callbacks.onTranscriptDone).toHaveBeenCalledTimes(3);
    expect(callbacks.onTranscript).toHaveBeenLastCalledWith({ interim: '', final: 'two three' });
  });

  it('stopRecording ends the utterance in progress and still delivers its transcript', async () => {
    const { result, callbacks } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });
    push(LOUD());
    act(() => result.current.stopRecording());

    expect(release).toHaveBeenCalled();
    expect(result.current.isRecording).toBe(false);
    expect(uploads).toHaveLength(1);
    uploads[0]?.resolve('last words');
    await settle();
    expect(callbacks.onTranscript).toHaveBeenCalledWith({ interim: '', final: 'last words' });
  });

  it('stopRecording is a no-op when not recording', () => {
    const { result } = renderRecognition();
    act(() => result.current.stopRecording());
    expect(release).not.toHaveBeenCalled();
    expect(result.current.isRecording).toBe(false);
  });

  it('a new session discards the previous session’s late transcripts', async () => {
    const { result, callbacks } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });
    speakUtterance();
    act(() => result.current.stopRecording());
    await act(async () => {
      await result.current.startRecording();
    });

    expect(uploads[0]?.signal?.aborted).toBe(true);
    uploads[0]?.resolve('stale');
    await settle();
    expect(callbacks.onTranscript).not.toHaveBeenCalled();
  });

  it('a refused upload is reported by code; a rate limit is dropped quietly', async () => {
    const { result, callbacks } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });
    speakUtterance();
    uploads[0]?.reject(new VoiceTransportError('limit'));
    await settle();
    expect(callbacks.onError).not.toHaveBeenCalled();
    expect(callbacks.onTranscriptDone).toHaveBeenCalledOnce();

    speakUtterance();
    uploads[1]?.reject(new VoiceTransportError('model-unavailable'));
    await settle();
    expect(callbacks.onError).toHaveBeenCalledWith('model-unavailable');
    expect(callbacks.onTranscriptDone).toHaveBeenCalledTimes(2);
  });

  it('maps a microphone failure to its getUserMedia class', async () => {
    mocks.startSpeechCapture.mockRejectedValueOnce(new DOMException('denied', 'NotAllowedError'));
    const { result, callbacks } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });
    expect(callbacks.onError).toHaveBeenCalledWith('not-allowed');
    expect(result.current.isRecording).toBe(false);
  });

  it('a stop while the microphone is still opening releases it once it opens', async () => {
    let open: ((capture: { release: () => void }) => void) | undefined;
    mocks.startSpeechCapture.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          open = resolve;
        }),
    );
    const { result } = renderRecognition();
    let starting: Promise<void> | undefined;
    act(() => {
      starting = result.current.startRecording();
    });
    act(() => result.current.stopRecording());
    await act(async () => {
      open?.({ release });
      await starting;
    });
    expect(release).toHaveBeenCalled();
    expect(result.current.isRecording).toBe(false);
  });

  it('unmount releases the microphone and aborts pending uploads', async () => {
    const { result, unmount } = renderRecognition();
    await act(async () => {
      await result.current.startRecording();
    });
    speakUtterance();
    unmount();
    expect(release).toHaveBeenCalled();
    expect(uploads[0]?.signal?.aborted).toBe(true);
  });
});
