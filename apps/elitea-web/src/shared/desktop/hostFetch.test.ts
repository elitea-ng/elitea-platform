import { describe, expect, it, vi } from 'vitest';

import { createHostFetch, encodeFetchMeta, MAX_REQUEST_BODY_BYTES, META_HEADER, type RawInvoke, type ReleaseRegistry } from './hostFetch';

/** The host rejects with `{code, message}` (net.rs `FetchError`); an Error carrying both is that shape. */
function refusal(code: string, message: string): Error {
  return Object.assign(new Error(message), { code });
}

interface Sent {
  meta: { id: number; method: string; url: string; headers: [string, string][] };
  body: string;
  raw: Uint8Array;
}

/** What the host reads: the raw payload is the body, the header the metadata. */
function decode(payload: unknown, options: Parameters<RawInvoke>[2]): Sent {
  const raw = payload as Uint8Array;
  const header = options?.headers?.[META_HEADER] ?? '';
  const meta = JSON.parse(decodeURIComponent(header)) as Sent['meta'];
  return { meta, body: new TextDecoder().decode(raw), raw };
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
  const invoke = vi.fn<RawInvoke>((command, args, options) => {
    if (command === 'http_fetch') {
      sent.push(decode(args, options));
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

describe('encodeFetchMeta', () => {
  it('is percent-encoded JSON, ASCII whatever the header values hold', () => {
    const value = encodeFetchMeta({ id: 1, headers: [['x-name', 'Zoë']] });
    expect(value).toMatch(/^[\x20-\x7e]+$/);
    expect(JSON.parse(decodeURIComponent(value))).toEqual({ id: 1, headers: [['x-name', 'Zoë']] });
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
    await vi.waitFor(() => expect(invoke).toHaveBeenCalledWith('http_fetch', expect.anything(), expect.anything()));
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

  it('honours the input Request\'s own signal when the init has none', async () => {
    const h = host(head());
    const controller = new AbortController();
    controller.abort();
    const request = new Request('https://h.example/a', { signal: controller.signal });
    await expect(createHostFetch(h.invoke)(request)).rejects.toMatchObject({ name: 'AbortError' });
    expect(h.invoke).not.toHaveBeenCalled();
  });

  it('cancels on an abort of the input Request\'s signal', async () => {
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
    const pending = createHostFetch(invoke, { firstId: 30 })(new Request('https://h.example/slow', { signal: controller.signal }));
    await vi.waitFor(() => expect(invoke).toHaveBeenCalledWith('http_fetch', expect.anything(), expect.anything()));
    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
    expect(cancels).toEqual([{ id: 31 }]);
  });

  it('refuses a body over the limit before buffering it when its size is known', async () => {
    const h = host(head());
    const fetch = createHostFetch(h.invoke, { maxBodyBytes: 4 });
    const arrayBuffer = vi.spyOn(Request.prototype, 'arrayBuffer');
    await expect(fetch('https://h.example/up', { method: 'POST', body: new Blob(['hello']) })).rejects.toBeInstanceOf(TypeError);
    const form = new FormData();
    form.append('f', new Blob(['hello']));
    await expect(fetch('https://h.example/up', { method: 'POST', body: form })).rejects.toBeInstanceOf(TypeError);
    expect(arrayBuffer).not.toHaveBeenCalled();
    arrayBuffer.mockRestore();
    // A body whose size only buffering tells: refused after it, still before the host.
    await expect(fetch(new Request('https://h.example/up', { method: 'POST', body: 'hello' }))).rejects.toThrow('larger than the desktop app can send');
    await expect(fetch('https://h.example/up', { method: 'POST', body: 'ok' })).resolves.toBeInstanceOf(Response);
    expect(h.sent.map((s) => s.body)).toEqual(['ok']);
    expect(MAX_REQUEST_BODY_BYTES).toBe(160 * 1024 * 1024);
  });

  it('releases the host entry at once for a HEAD or null-body response', async () => {
    const h = host(head({ hasBody: true }));
    const fetch = createHostFetch(h.invoke, { firstId: 50 });
    const response = await fetch('https://h.example/a', { method: 'HEAD' });
    expect(response.body).toBeNull();
    expect(h.cancels).toEqual([51]);
    expect(h.reads).toEqual([]);
  });

  it('releases a body whose stream was collected unread, and not one read to its end', async () => {
    // The first stream's eager first pull takes 'x' (a stream fills its queue on creation).
    const h = host(head(), ['x', 'one']);
    let collect: ((id: number) => void) | undefined;
    const registered: { target: object; id: number; token: object }[] = [];
    const unregistered: object[] = [];
    const registry: ReleaseRegistry = {
      register: (target, id, token) => registered.push({ target, id, token }),
      unregister: (token) => unregistered.push(token),
    };
    const fetch = createHostFetch(h.invoke, {
      firstId: 60,
      createRegistry: (release) => {
        collect = release;
        return registry;
      },
    });
    const unread = await fetch('https://h.example/a');
    expect(registered[0]).toMatchObject({ id: 61, target: unread.body });
    // The garbage collector found the stream unreachable:
    collect?.(61);
    expect(h.cancels).toEqual([61]);

    const read = await fetch('https://h.example/b');
    expect(await read.text()).toBe('one');
    expect(unregistered).toContain(registered[1]?.token);
  });

  it('sends the body itself as the payload, with no frame around it', async () => {
    const h = host(head({ hasBody: false, status: 204 }));
    const buffers: ArrayBuffer[] = [];
    const arrayBuffer = vi.spyOn(Request.prototype, 'arrayBuffer');
    arrayBuffer.mockImplementation(async function (this: Request) {
      const buffer = await new Response(this.body).arrayBuffer();
      buffers.push(buffer);
      return buffer;
    });
    await createHostFetch(h.invoke, { firstId: 70 })('https://h.example/up', { method: 'PUT', body: 'payload' });
    arrayBuffer.mockRestore();
    expect(h.sent[0]?.body).toBe('payload');
    expect(h.sent[0]?.raw.byteLength).toBe('payload'.length);
    // The very buffer the request was read into: not copied into a frame.
    expect(h.sent[0]?.raw.buffer).toBe(buffers[0]);
    expect(h.sent[0]?.meta).toMatchObject({ id: 71, method: 'PUT', url: 'https://h.example/up' });
  });

  it('ends at once on an abort while the body is still being read, and never asks the host', async () => {
    const h = host(head());
    const controller = new AbortController();
    // A body that never finishes arriving.
    const endless = new ReadableStream<Uint8Array>({ pull: () => new Promise(() => undefined) });
    const pending = createHostFetch(h.invoke)('https://h.example/up', {
      method: 'POST',
      body: endless,
      signal: controller.signal,
      duplex: 'half',
    } as RequestInit);
    await new Promise((resolve) => setTimeout(resolve, 10));
    controller.abort();
    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
    expect(h.invoke).not.toHaveBeenCalled();
  });
});
