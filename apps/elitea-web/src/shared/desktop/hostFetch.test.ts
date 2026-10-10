import { describe, expect, it, vi } from 'vitest';

import { createHostFetch, encodeFetchFrame, type RawInvoke } from './hostFetch';

/** The host rejects with `{code, message}` (net.rs `FetchError`); an Error carrying both is that shape. */
function refusal(code: string, message: string): Error {
  return Object.assign(new Error(message), { code });
}

interface Sent {
  meta: { id: number; method: string; url: string; headers: [string, string][] };
  body: string;
}

function decode(frame: Uint8Array): Sent {
  const length = new DataView(frame.buffer, frame.byteOffset).getUint32(0);
  const meta = JSON.parse(new TextDecoder().decode(frame.subarray(4, 4 + length))) as Sent['meta'];
  return { meta, body: new TextDecoder().decode(frame.subarray(4 + length)) };
}

const head = (over: Partial<{ status: number; hasBody: boolean; headers: [string, string][] }> = {}) => ({
  status: 200,
  statusText: 'OK',
  headers: [['content-type', 'application/json']] as [string, string][],
  url: 'https://h.example/api/v2/x',
  hasBody: true,
  ...over,
});

/** A host that answers `http_fetch` with `answer` and serves `chunks` to `http_read_body`. */
function host(answer: unknown, chunks: string[] = []) {
  const sent: Sent[] = [];
  const reads: number[] = [];
  const cancels: number[] = [];
  const queue = chunks.map((c) => new TextEncoder().encode(c).buffer);
  const invoke = vi.fn<RawInvoke>((command, args) => {
    if (command === 'http_fetch') {
      sent.push(decode(args as Uint8Array));
      return answer instanceof Error ? Promise.reject(answer) : Promise.resolve(answer);
    }
    if (command === 'http_read_body') {
      reads.push((args as { id: number }).id);
      return Promise.resolve(queue.shift() ?? new ArrayBuffer(0));
    }
    if (command === 'http_cancel') cancels.push((args as { id: number }).id);
    return Promise.resolve(null);
  });
  return { invoke, sent, reads, cancels };
}

describe('encodeFetchFrame', () => {
  it('is a big-endian length, the JSON metadata, then the body', () => {
    const frame = encodeFetchFrame({ id: 1 }, new Uint8Array([7, 8]));
    expect([...frame.subarray(0, 4)]).toEqual([0, 0, 0, 8]);
    expect(new TextDecoder().decode(frame.subarray(4, 12))).toBe('{"id":1}');
    expect([...frame.subarray(12)]).toEqual([7, 8]);
  });
});

describe('createHostFetch', () => {
  it('sends method, url, headers and body in one frame and streams the answer', async () => {
    const h = host(head(), ['{"a":', '1}']);
    const fetch = createHostFetch(h.invoke, { firstId: 41 });
    const response = await fetch('https://h.example/api/v2/x', {
      method: 'POST',
      headers: { Authorization: 'Bearer t', 'Content-Type': 'application/json' },
      body: '{"q":1}',
    });
    expect(h.sent[0]?.meta).toMatchObject({ id: 42, method: 'POST', url: 'https://h.example/api/v2/x' });
    expect(h.sent[0]?.meta.headers).toEqual(expect.arrayContaining([['authorization', 'Bearer t'], ['content-type', 'application/json']]));
    expect(h.sent[0]?.body).toBe('{"q":1}');
    expect(response.status).toBe(200);
    expect(response.url).toBe('https://h.example/api/v2/x');
    expect(response.headers.get('content-type')).toBe('application/json');
    expect(await response.json()).toEqual({ a: 1 });
    expect(h.reads).toEqual([42, 42, 42]);
  });

  it('gives every request its own id', async () => {
    const h = host(head({ hasBody: false, status: 204 }));
    const fetch = createHostFetch(h.invoke, { firstId: 0 });
    await Promise.all([fetch('https://h.example/a'), fetch('https://h.example/b')]);
    expect(h.sent.map((s) => s.meta.id)).toEqual([1, 2]);
  });

  it('has no body to read for a null-body status', async () => {
    const h = host(head({ hasBody: false, status: 204 }));
    const response = await createHostFetch(h.invoke)('https://h.example/a');
    expect(response.status).toBe(204);
    expect(response.body).toBeNull();
    expect(h.reads).toEqual([]);
  });

  it('frames a FormData body with its multipart boundary', async () => {
    const h = host(head({ hasBody: false, status: 204 }));
    const form = new FormData();
    form.append('note', 'hello');
    await createHostFetch(h.invoke)('https://h.example/upload', { method: 'POST', body: form });
    const contentType = new Map(h.sent[0]?.meta.headers).get('content-type') ?? '';
    expect(contentType).toMatch(/^multipart\/form-data; boundary=/);
    expect(h.sent[0]?.body).toContain('hello');
  });

  it('rejects with an AbortError, and never asks the host, when already aborted', async () => {
    const h = host(head());
    const controller = new AbortController();
    controller.abort();
    await expect(createHostFetch(h.invoke)('https://h.example/a', { signal: controller.signal })).rejects.toMatchObject({ name: 'AbortError' });
    expect(h.invoke).not.toHaveBeenCalled();
  });

  it('cancels the host request on abort and rejects with an AbortError', async () => {
    let release: ((value: unknown) => void) | undefined;
    const cancels: unknown[] = [];
    const invoke = vi.fn<RawInvoke>((command, args) => {
      if (command === 'http_fetch') return new Promise((resolve) => (release = resolve));
      if (command === 'http_cancel') {
        cancels.push(args);
        release?.(Promise.reject(refusal('aborted', 'the request was aborted')));
      }
      return Promise.resolve(null);
    });
    const controller = new AbortController();
    const pending = createHostFetch(invoke, { firstId: 9 })('https://h.example/slow', { signal: controller.signal });
    await Promise.resolve();
    await Promise.resolve();
    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
    expect(cancels).toEqual([{ id: 10 }]);
  });

  it('turns a host refusal into the TypeError fetch would throw', async () => {
    const h = host(refusal('url_not_allowed', 'the app may only reach the connected deployment'));
    const failure = createHostFetch(h.invoke)('https://elsewhere.example/');
    await expect(failure).rejects.toBeInstanceOf(TypeError);
    await expect(failure).rejects.toThrow('connected deployment');
  });

  it('ends the host stream when the reader cancels it (an SSE close)', async () => {
    const h = host(head({ headers: [['content-type', 'text/event-stream']] }), ['data: 1\n\n']);
    const response = await createHostFetch(h.invoke, { firstId: 99 })('https://h.example/events');
    const reader = response.body!.getReader();
    expect(new TextDecoder().decode((await reader.read()).value)).toBe('data: 1\n\n');
    await reader.cancel();
    expect(h.cancels).toEqual([100]);
  });
});
