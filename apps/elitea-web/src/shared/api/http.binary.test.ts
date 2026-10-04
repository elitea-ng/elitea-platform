/**
 * The two request options the `/llm` audio routes need: `originRoot` (the data
 * plane sits beside `/api/v2`, not under it) and `binary` (a speech answer is
 * raw audio, which the text-first result path would corrupt).
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { MockInstance } from 'vitest';

import { createHttpClient } from './http';

const ORIGIN = window.location.origin;

afterEach(() => {
  vi.restoreAllMocks();
});

function lastUrl(spy: MockInstance<typeof fetch>): string {
  const call = spy.mock.calls.at(-1) as [string, RequestInit] | undefined;
  return call?.[0] ?? '';
}

describe('originRoot', () => {
  it('resolves the path against the base ORIGIN, dropping the /api/v2 path', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, { status: 204 }));
    await createHttpClient({ baseUrl: '/api/v2' }).post('/llm/v1/audio/speech', { body: '{}', originRoot: true });
    expect(lastUrl(spy)).toBe(`${ORIGIN}/llm/v1/audio/speech`);
  });

  it('keeps an absolute cross-origin base host', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, { status: 204 }));
    await createHttpClient({ baseUrl: 'https://api.example.test/api/v2' }).post('/llm/v1/audio/speech', {
      body: '{}',
      originRoot: true,
    });
    expect(lastUrl(spy)).toBe('https://api.example.test/llm/v1/audio/speech');
  });

  it('leaves every other request under the base path', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, { status: 204 }));
    await createHttpClient({ baseUrl: '/api/v2' }).get('/llm/v1/models');
    expect(lastUrl(spy)).toBe(`${ORIGIN}/api/v2/llm/v1/models`);
  });
});

describe('binary', () => {
  it('returns the success body as the exact bytes, not text', async () => {
    const bytes = new Uint8Array([0x52, 0x49, 0x46, 0x46, 0x00, 0xff, 0x80]);
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(bytes, { status: 200, headers: { 'Content-Type': 'audio/wav' } }),
    );
    const result = await createHttpClient({ baseUrl: '/api/v2' }).post<ArrayBuffer>('/llm/v1/audio/speech', {
      body: '{}',
      originRoot: true,
      binary: true,
    });
    if (!result.ok) throw new Error('expected ok');
    expect(new Uint8Array(result.data)).toEqual(bytes);
  });

  it('still parses a failure body as JSON, so the caller can read the error code', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ error: { code: 'unsupported_operation' } }), {
        status: 501,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
    const result = await createHttpClient({ baseUrl: '/api/v2' }).post('/llm/v1/audio/speech', {
      body: '{}',
      binary: true,
    });
    if (result.ok) throw new Error('expected a failure');
    expect(result.error).toMatchObject({ kind: 'http', status: 501, body: { error: { code: 'unsupported_operation' } } });
  });
});
