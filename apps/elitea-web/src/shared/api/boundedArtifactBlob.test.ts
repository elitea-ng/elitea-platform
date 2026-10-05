import { Blob as NodeBlob } from 'node:buffer';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';

import { boundedArtifactBlob } from './boundedArtifactBlob';

beforeEach(() => { vi.stubGlobal('Blob', NodeBlob); });
afterEach(() => { vi.unstubAllGlobals(); });

it('keeps exact raw bytes from a response within the limit', async () => {
  const response = new Response(new Uint8Array([0, 128, 255]), { headers: { 'Content-Type': 'application/json' } });
  const result = await boundedArtifactBlob(response, 3);
  expect(result.ok).toBe(true);
  if (!result.ok) throw new Error('Expected bounded bytes');
  expect(Array.from(new Uint8Array(await result.data.arrayBuffer()))).toEqual([0, 128, 255]);
});

it.each([undefined, '1'])('cancels an oversized stream even with Content-Length %s', async declared => {
  const cancel = vi.fn();
  const response = new Response(new ReadableStream<Uint8Array>({
    start(controller) { controller.enqueue(new Uint8Array(4)); },
    cancel,
  }), { headers: declared ? { 'Content-Length': declared } : {} });
  expect((await boundedArtifactBlob(response, 3)).ok).toBe(false);
  expect(cancel).toHaveBeenCalledTimes(1);
  expect(response.body?.locked).toBe(false);
});

it('joins small chunks without changing or retaining unused bytes', async () => {
  const response = new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(new Uint8Array([0]));
      controller.enqueue(new Uint8Array([128, 255]));
      controller.close();
    },
  }));
  const result = await boundedArtifactBlob(response, 16);
  expect(result.ok).toBe(true);
  if (!result.ok) throw new Error('Expected bounded bytes');
  expect(Array.from(new Uint8Array(await result.data.arrayBuffer()))).toEqual([0, 128, 255]);
});

it('cancels a denied response without reading private error bytes', async () => {
  const cancel = vi.fn();
  const response = new Response(new ReadableStream<Uint8Array>({ cancel }), { status: 403 });
  const read = vi.spyOn(response, 'text');
  expect(await boundedArtifactBlob(response, 3)).toMatchObject({ ok: false, error: { kind: 'http', status: 403, body: undefined } });
  expect(cancel).toHaveBeenCalledTimes(1);
  expect(read).not.toHaveBeenCalled();
});
