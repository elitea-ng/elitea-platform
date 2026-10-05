/**
 * The HTTP voice transport, through the real `eliteaFetch` + HTTP client, with
 * `fetch` itself replaced: these tests read the exact request the browser
 * sends to the `/llm` edge and the error each refusal becomes.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MockInstance } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { SPEECH_PATH, TRANSCRIPTION_PATH, VoiceTransportError, isVoiceAbort, synthesizeSpeech, transcribeAudio } from './voiceTransport';

const ORIGIN = window.location.origin;

let fetchSpy: MockInstance<typeof fetch>;

function lastCall(): { url: string; init: RequestInit } {
  const [url, init] = fetchSpy.mock.calls.at(-1) as [string, RequestInit];
  return { url, init };
}

function jsonError(status: number, code: string): Response {
  return new Response(JSON.stringify({ error: { code, message: 'x', type: 'invalid_request_error' } }), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  fetchSpy = vi.spyOn(globalThis, 'fetch');
});

afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('synthesizeSpeech', () => {
  it('POSTs JSON to /llm/v1/audio/speech at the origin root with the project header, and returns the audio bytes', async () => {
    const audio = new Uint8Array([0x49, 0x44, 0x33, 0x04]);
    fetchSpy.mockResolvedValue(new Response(audio, { status: 200, headers: { 'Content-Type': 'audio/mpeg' } }));

    const result = await synthesizeSpeech({ projectId: 12, model: 'tts-1', input: 'Hello.', voice: 'alloy', speed: 1.2 });

    const { url, init } = lastCall();
    expect(url).toBe(`${ORIGIN}${SPEECH_PATH}`);
    expect(init.method).toBe('POST');
    const headers = new Headers(init.headers);
    expect(headers.get('x-project-id')).toBe('12');
    expect(headers.get('content-type')).toBe('application/json');
    expect(JSON.parse(init.body as string)).toEqual({ model: 'tts-1', input: 'Hello.', voice: 'alloy', speed: 1.2 });
    expect(new Uint8Array(result)).toEqual(audio);
  });

  it.each([
    [404, 'model_not_found', 'model-unavailable'],
    [501, 'unsupported_operation', 'model-unavailable'],
    [503, 'llm_not_configured', 'model-unavailable'],
    [402, 'insufficient_quota', 'budget'],
    [429, 'rate_limit_exceeded', 'limit'],
    [413, 'request_too_large', 'too-large'],
    [500, 'api_error', 'failed'],
  ])('maps a %i %s refusal to %s', async (status, code, expected) => {
    fetchSpy.mockResolvedValue(jsonError(status, code));
    await expect(synthesizeSpeech({ projectId: 1, model: 'm', input: 'x' })).rejects.toMatchObject({ code: expected });
  });

  it('maps a network failure to failed', async () => {
    fetchSpy.mockRejectedValue(new TypeError('offline'));
    const err = await synthesizeSpeech({ projectId: 1, model: 'm', input: 'x' }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(VoiceTransportError);
    expect((err as VoiceTransportError).code).toBe('failed');
  });

  it('re-throws an abort unchanged, so a stop is not reported as a failure', async () => {
    const abort = new AbortController();
    fetchSpy.mockImplementation(() => Promise.reject(new DOMException('aborted', 'AbortError')));
    abort.abort();
    const err = await synthesizeSpeech({ projectId: 1, model: 'm', input: 'x' }, abort.signal).catch((e: unknown) => e);
    expect(err).not.toBeInstanceOf(VoiceTransportError);
    expect(isVoiceAbort(err)).toBe(true);
  });
});

describe('transcribeAudio', () => {
  it('POSTs multipart with model, file, format and language to /llm/v1/audio/transcriptions', async () => {
    fetchSpy.mockResolvedValue(
      new Response(JSON.stringify({ text: '  hello world ' }), { status: 200, headers: { 'Content-Type': 'application/json' } }),
    );
    const audio = new Blob([new Uint8Array(8)], { type: 'audio/wav' });

    const text = await transcribeAudio({ projectId: 'p-4', model: 'whisper-1', audio, language: 'en' });

    const { url, init } = lastCall();
    expect(url).toBe(`${ORIGIN}${TRANSCRIPTION_PATH}`);
    expect(new Headers(init.headers).get('x-project-id')).toBe('p-4');
    const form = init.body as FormData;
    expect(form.get('model')).toBe('whisper-1');
    expect(form.get('language')).toBe('en');
    expect(form.get('response_format')).toBe('json');
    expect((form.get('file') as File).name).toBe('speech.wav');
    expect(text).toBe('hello world');
  });

  it('omits the language when none is known, and reads a missing text as empty', async () => {
    fetchSpy.mockResolvedValue(new Response(JSON.stringify({}), { status: 200, headers: { 'Content-Type': 'application/json' } }));
    const text = await transcribeAudio({ projectId: 1, model: 'whisper-1', audio: new Blob([]), language: undefined });
    expect((lastCall().init.body as FormData).has('language')).toBe(false);
    expect(text).toBe('');
  });

  it('maps a refusal to its voice error code', async () => {
    fetchSpy.mockResolvedValue(jsonError(413, 'request_too_large'));
    await expect(transcribeAudio({ projectId: 1, model: 'm', audio: new Blob([]) })).rejects.toMatchObject({ code: 'too-large' });
  });
});
