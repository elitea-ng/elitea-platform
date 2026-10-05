/**
 * `eliteaFetch`'s transport flags: each one reaches the HTTP client only when
 * the caller set it. `originRoot` + `binary` are what the `/llm` audio routes
 * use (`shared/api/voiceTransport.ts`).
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, eliteaFetch, resetGeneratedClient } from './mutator';

afterEach(() => {
  resetGeneratedClient();
  vi.restoreAllMocks();
});

describe('eliteaFetch transport flags', () => {
  it('forwards originRoot and binary to the client', async () => {
    const bytes = new Uint8Array([1, 2, 3]);
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(bytes, { status: 200 }));
    configureGeneratedClient({ baseUrl: '/api/v2' });

    const envelope = await eliteaFetch<{ data: ArrayBuffer }>(
      '/llm/v1/audio/speech',
      { method: 'POST', body: '{}' },
      { originRoot: true, binary: true },
    );

    expect(spy.mock.calls[0]?.[0] as string).toBe(`${window.location.origin}/llm/v1/audio/speech`);
    expect(new Uint8Array(envelope.data)).toEqual(bytes);
  });

  it('sends nothing extra when no flag is set', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(null, { status: 204 }));
    configureGeneratedClient({ baseUrl: '/api/v2' });

    await eliteaFetch('/llm/v1/models', { method: 'GET' }, { background: true });

    expect(spy.mock.calls[0]?.[0] as string).toBe(`${window.location.origin}/api/v2/llm/v1/models`);
  });
});
