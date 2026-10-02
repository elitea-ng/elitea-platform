/**
 * HTTP core — §5.4 behaviours 1 (credentials), 6 (dev token), 8 (abort),
 * 9 (traceparent), §3.6 Result discipline, and request assembly.
 * Re-auth behaviours 2/3 + replay live in http.reauth.test.ts.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import {
  TRANSPORT_PROBE_PATH,
  probeAny,
  probeLyingJson,
  probeNetworkError,
  probeNeverending,
  probeNoContent,
  probeNotFound,
  probeOk,
} from '../../test/msw/handlers/transport';
import type { CapturedRequest } from '../../test/msw/handlers/transport';

import { createHttpClient, generateTraceparent, resolveCredentialsMode } from './http';

const ORIGIN = window.location.origin; // http://localhost:3000 under jsdom
const CROSS_ORIGIN_BASE = 'https://api.cross-origin.example/api/v2';

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllEnvs();
});

describe('behaviour 1 — credentials mode', () => {
  it('resolves same-origin for a relative base URL', () => {
    expect(resolveCredentialsMode('/api/v2', ORIGIN)).toBe('same-origin');
  });

  it('resolves same-origin for an absolute base on the page origin', () => {
    expect(resolveCredentialsMode(`${ORIGIN}/api/v2`, ORIGIN)).toBe('same-origin');
  });

  it("resolves 'include' when the base URL is cross-origin", () => {
    expect(resolveCredentialsMode(CROSS_ORIGIN_BASE, ORIGIN)).toBe('include');
    expect(resolveCredentialsMode('https://localhost:3000/api/v2', ORIGIN)).toBe('include'); // scheme counts
  });

  it('exposes the derived mode on the client and honours an explicit override', () => {
    expect(createHttpClient({ baseUrl: '/api/v2' }).credentials).toBe('same-origin');
    expect(createHttpClient({ baseUrl: CROSS_ORIGIN_BASE }).credentials).toBe('include');
    expect(createHttpClient({ baseUrl: '/api/v2', credentialsMode: 'omit' }).credentials).toBe('omit');
  });

  it('passes the resolved mode into every fetch init — both branches', async () => {
    const spy = vi.spyOn(globalThis, 'fetch');
    server.use(probeOk());

    await createHttpClient({ baseUrl: '/api/v2' }).get(TRANSPORT_PROBE_PATH.replace('/api/v2', ''));
    await createHttpClient({ baseUrl: CROSS_ORIGIN_BASE }).get(TRANSPORT_PROBE_PATH.replace('/api/v2', ''));

    const inits = spy.mock.calls.map((call) => call[1] as RequestInit);
    expect(inits.map((init) => init.credentials)).toEqual(['same-origin', 'include']);
  });
});

describe('§3.6 — discriminated Result, never a throw for 4xx', () => {
  const client = createHttpClient({ baseUrl: '/api/v2' });
  const path = '/__transport__/probe';

  it('resolves ok:true with parsed JSON for a 200', async () => {
    server.use(probeOk());
    const result = await client.get<{ message: string }>(path);
    if (!result.ok) throw new Error('unreachable');
    expect(result.headers).toBeInstanceOf(Headers);
    expect(result).toEqual({
      ok: true,
      status: 200,
      data: { message: 'transport probe ok' },
      headers: result.headers,
    });
  });

  it('resolves — does not throw — with a kind:http failure for a 404', async () => {
    server.use(probeNotFound());
    const result = await client.get(path);
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    expect(result.error).toMatchObject({ kind: 'http', status: 404, body: { error: 'not found' } });
  });

  it('returns a kind:network failure with context for a network error', async () => {
    server.use(probeNetworkError());
    const result = await client.get(path);
    if (result.ok) throw new Error('expected failure');
    expect(result.error.kind).toBe('network');
    if (result.error.kind !== 'network') throw new Error('unreachable');
    expect(result.error.message).toContain('http: request to');
    expect(result.error.cause).toBeDefined();
  });

  it('surfaces raw text when a server lies about application/json', async () => {
    server.use(probeLyingJson());
    const result = await client.get<string>(path);
    expect(result.ok).toBe(true);
    if (!result.ok) throw new Error('unreachable');
    expect(result.data).toContain('edge-auth login page');
  });

  it('returns undefined data for 204 No Content', async () => {
    server.use(probeNoContent());
    const result = await client.get(path);
    if (!result.ok) throw new Error('unreachable');
    expect(result.headers).toBeInstanceOf(Headers);
    expect(result).toEqual({ ok: true, status: 204, data: undefined, headers: result.headers });
  });

  it('serves non-GET sugar methods through the same Result path', async () => {
    server.use(probeAny());
    expect((await client.post(path, { body: { a: 1 } })).ok).toBe(true);
    expect((await client.put(path, { body: { a: 1 } })).ok).toBe(true);
    expect((await client.patch(path, { body: { a: 1 } })).ok).toBe(true);
    expect((await client.delete(path)).ok).toBe(true);
  });
});

describe('behaviour 8 — AbortSignal on every request', () => {
  const client = createHttpClient({ baseUrl: '/api/v2' });

  it('aborting mid-flight resolves to a kind:aborted failure', async () => {
    server.use(probeNeverending());
    const controller = new AbortController();
    const pending = client.get('/__transport__/probe', { signal: controller.signal });
    controller.abort();
    const result = await pending;
    if (result.ok) throw new Error('expected abort');
    expect(result.error.kind).toBe('aborted');
  });

  it('a pre-aborted signal short-circuits to kind:aborted', async () => {
    server.use(probeOk());
    const controller = new AbortController();
    controller.abort();
    const result = await client.get('/__transport__/probe', { signal: controller.signal });
    if (result.ok) throw new Error('expected abort');
    expect(result.error.kind).toBe('aborted');
  });
});

describe('behaviour 9 — traceparent (W3C shape from TraceService.js:55-61)', () => {
  const W3C = /^00-[0-9a-f]{32}-[0-9a-f]{16}-01$/;

  it('generateTraceparent emits the exact old shape, fresh per call', () => {
    const first = generateTraceparent();
    expect(first).toMatch(W3C);
    expect(generateTraceparent()).not.toBe(first);
  });

  it('attaches a fresh traceparent per request when tracing is enabled', async () => {
    const sink: CapturedRequest[] = [];
    server.use(probeOk(sink));
    const client = createHttpClient({ baseUrl: '/api/v2', tracingEnabled: true });
    await client.get('/__transport__/probe');
    await client.get('/__transport__/probe');
    expect(sink).toHaveLength(2);
    for (const seen of sink) expect(seen.traceparent).toMatch(W3C);
    expect(sink[0]?.traceparent).not.toBe(sink[1]?.traceparent);
  });

  it('sends NO traceparent when tracing is disabled', async () => {
    const sink: CapturedRequest[] = [];
    server.use(probeOk(sink));
    await createHttpClient({ baseUrl: '/api/v2' }).get('/__transport__/probe');
    expect(sink[0]?.traceparent).toBeNull();
  });
});

describe('behaviour 6 — dev token only under import.meta.env.DEV', () => {
  it('attaches the bearer + Cache-Control in dev when a token is configured', async () => {
    const sink: CapturedRequest[] = [];
    server.use(probeOk(sink));
    await createHttpClient({ baseUrl: '/api/v2', devToken: 'dev-secret' }).get('/__transport__/probe');
    expect(sink[0]?.authorization).toBe('Bearer dev-secret');
    expect(sink[0]?.cacheControl).toBe('no-cache'); // parity: eliteaApi.js:63
  });

  it('attaches NOTHING outside DEV even when a token is configured', async () => {
    vi.stubEnv('DEV', false);
    const sink: CapturedRequest[] = [];
    server.use(probeOk(sink));
    await createHttpClient({ baseUrl: '/api/v2', devToken: 'dev-secret' }).get('/__transport__/probe');
    expect(sink[0]?.authorization).toBeNull();
    expect(sink[0]?.cacheControl).toBeNull();
  });

  it('attaches no bearer in dev without a token', async () => {
    const sink: CapturedRequest[] = [];
    server.use(probeOk(sink));
    await createHttpClient({ baseUrl: '/api/v2' }).get('/__transport__/probe');
    expect(sink[0]?.authorization).toBeNull();
    expect(sink[0]?.cacheControl).toBe('no-cache');
  });
});

describe('request assembly', () => {
  const client = createHttpClient({ baseUrl: '/api/v2' });
  const path = '/__transport__/probe';

  it('serializes query params and drops undefined values', async () => {
    const sink: CapturedRequest[] = [];
    server.use(probeOk(sink));
    await client.get(path, { query: { a: 1, b: 'x', c: undefined, d: true } });
    expect(sink[0]?.url).toContain('/__transport__/probe?a=1&b=x&d=true');
  });

  it('JSON-encodes object bodies with an application/json content type', async () => {
    const sink: CapturedRequest[] = [];
    server.use(probeAny(sink));
    await client.post(path, { body: { q: 'x' } });
    expect(sink[0]?.contentType).toBe('application/json');
    expect(sink[0]?.bodyText).toBe('{"q":"x"}');
  });

  it('sends string bodies verbatim with the caller’s content type', async () => {
    const sink: CapturedRequest[] = [];
    server.use(probeAny(sink));
    await client.post(path, { body: 'raw-payload', headers: { 'Content-Type': 'text/plain' } });
    expect(sink[0]?.contentType).toBe('text/plain');
    expect(sink[0]?.bodyText).toBe('raw-payload');
  });

  /**
   * Regression test: `serializeBody` used to `JSON.stringify` every
   * non-string body unconditionally, including `FormData`, and
   * `JSON.stringify(new FormData())` is `"{}"` (FormData has no enumerable
   * own properties) — no throw, so a multipart upload's real payload was
   * silently replaced with an empty JSON object. Found while porting Wave-2
   * unit C1 (chat model/store); confirmed the identical bug live in two
   * already-shipped generated call sites (artifacts.ts's createArtifact,
   * applications.ts's uploadApplicationIcon), both of which build a
   * FormData and pass it straight into `eliteaFetch({ body: formData })`.
   */
  it('sends FormData bodies verbatim, not JSON-stringified, with a multipart content type fetch sets itself', async () => {
    const sink: CapturedRequest[] = [];
    server.use(probeAny(sink));
    const formData = new FormData();
    // A plain string field, not a File/Blob: jsdom's Blob-in-FormData
    // doesn't preserve byte content through MSW's interception layer in
    // this test environment (confirmed separately, unrelated to the fix
    // under test here) — a string field survives faithfully and is
    // enough to prove the real bug (JSON.stringify(FormData) === "{}",
    // silently discarding the whole body) is fixed.
    formData.append('field', 'distinctive-field-value');
    await client.post(path, { body: formData });
    expect(sink[0]?.contentType).toMatch(/^multipart\/form-data; boundary=/);
    expect(sink[0]?.bodyText).not.toBe('{}');
    expect(sink[0]?.bodyText).toContain('distinctive-field-value');
  });

  it('throws (programmer error) for a GET with a body', async () => {
    await expect(client.get(path, { body: { nope: true } })).rejects.toThrow(/cannot carry a request body/);
  });

  it('throws with context for a non-serializable body', async () => {
    const cyclic: Record<string, unknown> = {};
    cyclic['self'] = cyclic;
    await expect(client.post(path, { body: cyclic })).rejects.toThrow(/not JSON-serializable/);
  });

  it('throws (programmer error) for a missing baseUrl', () => {
    expect(() => createHttpClient({ baseUrl: '' })).toThrow(/baseUrl is required/);
  });
});
