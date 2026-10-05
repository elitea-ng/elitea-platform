import { Blob as NodeBlob } from 'node:buffer';
import { webcrypto } from 'node:crypto';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';

import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';
import fixture from './fixtures/code-debug-public-trace.json';
import snapshot from './fixtures/code-debug-snapshot.json?raw';
import { parseCodeDebugProof } from './codeDebugArtifact';
import { readCodeDebugArtifact, verifyCodeDebugArtifact } from './readCodeDebugArtifact';

const reference = parseCodeDebugProof(fixture.first_attempt_history.metadata.code_debug_v1)!.artifact!;
const route = `/api/v2/artifacts/objects/7/code-debug/${reference.name}`;

beforeEach(() => {
  vi.stubGlobal('Blob', NodeBlob);
  vi.stubGlobal('crypto', webcrypto);
  vi.stubGlobal('elitea_ui_config', { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public', allow_project_own_llms: false });
  resetConfigForTests();
});
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); resetConfigForTests(); });

it('uses the existing authenticated route and verifies the frozen raw byte fixture', async () => {
  let calls = 0;
  server.use(http.get(route, ({ request }) => {
    calls++;
    expect(request.credentials).toBe('same-origin');
    expect(request.headers.get('authorization')).toBeNull();
    return new HttpResponse(snapshot, { headers: { 'Content-Type': 'application/json' } });
  }));
  const result = await readCodeDebugArtifact(reference, '7', new AbortController().signal);
  expect(calls).toBe(1);
  expect(result.ok).toBe(true);
  if (!result.ok) throw new Error('Expected the verified fixture');
  expect(result.blob.size).toBe(148);
  expect(await result.blob.text()).toBe(snapshot);
});

it.each([401, 403])('returns safe authorization guidance for HTTP %s', async status => {
  server.use(http.get(route, () => HttpResponse.json({ error: 'private source and credential diagnostic' }, { status })));
  expect(await readCodeDebugArtifact(reference, '7', new AbortController().signal)).toEqual({ ok: false, reason: 'authorization' });
});

it.each([snapshot.slice(1), `${snapshot} `, snapshot.replace('hello', 'HELLO')])('refuses truncated, longer or changed bytes', async content => {
  expect(await verifyCodeDebugArtifact(new Blob([content]), reference, new AbortController().signal)).toEqual({ ok: false, reason: 'changed' });
});

it('refuses an oversized body before hashing', async () => {
  const body = new Blob(['x'.repeat(3 * 1024 * 1024 + 1)]);
  expect(await verifyCodeDebugArtifact(body, reference, new AbortController().signal)).toEqual({ ok: false, reason: 'changed' });
});

it('refuses verification when Web Crypto is unavailable', async () => {
  vi.stubGlobal('crypto', {});
  expect(await verifyCodeDebugArtifact(new Blob([snapshot]), reference, new AbortController().signal)).toEqual({ ok: false, reason: 'verification' });
});

it('does not deliver a digest result after cancellation', async () => {
  let release = (): void => {};
  const gate = new Promise<void>(resolve => { release = resolve; });
  const realDigest = webcrypto.subtle.digest.bind(webcrypto.subtle);
  const digestObservation = vi.spyOn(webcrypto.subtle, 'digest').mockImplementation(async (algorithm, data) => {
    await gate;
    return realDigest(algorithm, data);
  });
  const controller = new AbortController();
  const pending = verifyCodeDebugArtifact(new Blob([snapshot]), reference, controller.signal);
  await vi.waitFor(() => expect(digestObservation).toHaveBeenCalledTimes(1));
  controller.abort();
  release();
  expect(await pending).toEqual({ ok: false, reason: 'aborted' });
  digestObservation.mockRestore();
});

it('does not fetch after cancellation', async () => {
  const controller = new AbortController();
  controller.abort();
  expect(await readCodeDebugArtifact(reference, '7', controller.signal)).toEqual({ ok: false, reason: 'aborted' });
});

it('refuses a foreign project before an artifact request', async () => {
  const fetch = vi.spyOn(globalThis, 'fetch');
  expect(await readCodeDebugArtifact(reference, '8', new AbortController().signal)).toEqual({ ok: false, reason: 'authorization' });
  expect(fetch).not.toHaveBeenCalled();
});

it('refuses an invalid reference before an artifact request', async () => {
  const fetch = vi.spyOn(globalThis, 'fetch');
  expect(await readCodeDebugArtifact({ ...reference, byte_length: 3 * 1024 * 1024 + 1 }, '7', new AbortController().signal)).toEqual({ ok: false, reason: 'changed' });
  expect(fetch).not.toHaveBeenCalled();
});

it('cancels a larger response before hashing or download', async () => {
  const cancel = vi.fn();
  vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(new ReadableStream<Uint8Array>({
    start(controller) { controller.enqueue(new Uint8Array(reference.byte_length + 1)); },
    cancel,
  })));
  const digest = vi.spyOn(webcrypto.subtle, 'digest');
  expect(await readCodeDebugArtifact(reference, '7', new AbortController().signal)).toEqual({ ok: false, reason: 'unavailable' });
  expect(cancel).toHaveBeenCalledTimes(1);
  expect(digest).not.toHaveBeenCalled();
});
