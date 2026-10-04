import { beforeEach, describe, expect, it, vi } from 'vitest';

import { voiceTransportMock } from '../__mocks__/voiceTransport.mock';

import { VoiceTransportError } from '../../api/voiceTransport';
import type { TranscriptionRequest } from '../../api/voiceTransport';

import { createSpeechTranscriber } from './speechTranscriber';

const transcribeAudio = voiceTransportMock.transcribeAudio;

interface Upload {
  readonly request: TranscriptionRequest;
  readonly resolve: (text: string) => void;
  readonly reject: (err: unknown) => void;
}

let uploads: Upload[];

beforeEach(() => {
  uploads = [];
  transcribeAudio.mockReset();
  transcribeAudio.mockImplementation(
    (request: TranscriptionRequest) =>
      new Promise<string>((resolve, reject) => {
        uploads.push({ request, resolve, reject });
      }),
  );
});

async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await Promise.resolve();
}

function make(signal = new AbortController().signal) {
  const callbacks = { onText: vi.fn(), onDone: vi.fn(), onError: vi.fn() };
  const transcriber = createSpeechTranscriber({
    projectId: 3,
    model: 'whisper-1',
    language: 'en',
    sampleRate: 24000,
    signal,
    ...callbacks,
  });
  return { transcriber, ...callbacks };
}

describe('createSpeechTranscriber', () => {
  it('uploads one utterance as WAV with the project, model and language', async () => {
    const { transcriber, onText, onDone } = make();
    transcriber.add(new Float32Array(2400));

    expect(uploads).toHaveLength(1);
    expect(uploads[0]?.request).toMatchObject({ projectId: 3, model: 'whisper-1', language: 'en' });
    // 44-byte header + 2 bytes per sample.
    expect(uploads[0]?.request.audio.size).toBe(44 + 4800);

    uploads[0]?.resolve('hi');
    await settle();
    expect(onText).toHaveBeenCalledWith('hi');
    expect(onDone).toHaveBeenCalledOnce();
  });

  it('holds utterances while one upload is out, then sends them merged', async () => {
    const { transcriber, onDone } = make();
    transcriber.add(new Float32Array(100));
    transcriber.add(new Float32Array(200));
    transcriber.add(new Float32Array(300));
    expect(uploads).toHaveLength(1);

    uploads[0]?.resolve('a');
    await settle();
    expect(uploads).toHaveLength(2);
    expect(uploads[1]?.request.audio.size).toBe(44 + 2 * 500);
    expect(onDone).toHaveBeenCalledTimes(1);

    uploads[1]?.resolve('b c');
    await settle();
    expect(onDone).toHaveBeenCalledTimes(3);
  });

  it('drops a rate-limited utterance quietly but still counts it done', async () => {
    const { transcriber, onError, onDone } = make();
    transcriber.add(new Float32Array(10));
    uploads[0]?.reject(new VoiceTransportError('limit'));
    await settle();
    expect(onError).not.toHaveBeenCalled();
    expect(onDone).toHaveBeenCalledOnce();
  });

  it('reports any other refusal by code, and an unknown failure as failed', async () => {
    const { transcriber, onError } = make();
    transcriber.add(new Float32Array(10));
    uploads[0]?.reject(new VoiceTransportError('too-large'));
    await settle();
    transcriber.add(new Float32Array(10));
    uploads[1]?.reject(new Error('boom'));
    await settle();
    expect(onError.mock.calls).toEqual([['too-large'], ['failed']]);
  });

  it('after abort, results are discarded and nothing new is sent', async () => {
    const abort = new AbortController();
    const { transcriber, onText, onDone, onError } = make(abort.signal);
    transcriber.add(new Float32Array(10));
    transcriber.add(new Float32Array(10));
    abort.abort();
    uploads[0]?.resolve('late');
    await settle();
    transcriber.add(new Float32Array(10));

    expect(onText).not.toHaveBeenCalled();
    expect(onDone).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
    expect(uploads).toHaveLength(1);
  });

  it('an abort error is not reported', async () => {
    const { transcriber, onError } = make();
    transcriber.add(new Float32Array(10));
    uploads[0]?.reject(new DOMException('aborted', 'AbortError'));
    await settle();
    expect(onError).not.toHaveBeenCalled();
  });
});
