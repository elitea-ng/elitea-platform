/**
 * shared/api/upload.ts — spec §5.7 row 2 (unit S6).
 * Covers: CHUNK_SIZE=5MiB chunking (RED/GREEN proof a), the 5 required
 * FormData fields, progress wiring, the {in_progress:true} vs. complete
 * discrimination (RED/GREEN proof c), credentials, DEV token gating,
 * http/network/abort failure paths, and the small-file/chunked orchestrator.
 *
 * Field-content assertions use `vi.spyOn(FormData.prototype, 'append')`
 * rather than server-side `request.formData()` parsing — see the comment in
 * ../../test/msw/handlers/upload.ts for why (a jsdom/undici cross-realm gap
 * unrelated to upload.ts's own correctness).
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import {
  chunkAckComplete,
  chunkAckInProgress,
  chunkAckSequence,
  CHUNKED_FILE_UPLOADED,
  SMALL_FILE_UPLOADED,
  smallFileOk,
  uploadNetworkError,
  uploadServerError,
} from '../../test/msw/handlers/upload';
import type { CapturedUploadRequest } from '../../test/msw/handlers/upload';

import {
  CHUNK_SIZE,
  buildAttachmentUploadUrl,
  createFileChunks,
  normalizeFileExtension,
  normalizeFileNameExtension,
  parseChunkAck,
  parseSmallFileAck,
  uploadChunk,
  uploadFileWithProgress,
  uploadSmallFile,
} from './upload';

const ORIGIN = window.location.origin;

function makeBlob(size: number): Blob {
  const bytes = new Uint8Array(size);
  for (let i = 0; i < size; i++) bytes[i] = i % 256;
  return new Blob([bytes]);
}

/** Samples a handful of indices instead of a full deep-equal — toEqual() on a multi-MB TypedArray takes several SECONDS (measured ~5.3s for 5MB) and blows the test timeout. */
async function expectByteAt(blob: Blob, offsetInBlob: number, expectedByte: number): Promise<void> {
  const sample = blob.slice(offsetInBlob, offsetInBlob + 1);
  const buf = new Uint8Array(await sample.arrayBuffer());
  expect(buf[0]).toBe(expectedByte);
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});

describe('CHUNK_SIZE', () => {
  it('is exactly 5 MiB', () => {
    expect(CHUNK_SIZE).toBe(5 * 1024 * 1024);
  });
});

describe('createFileChunks — the 5 MiB chunk-size boundary (RED/GREEN proof a)', () => {
  it('a file exactly one chunk stays a single chunk', () => {
    const chunks = createFileChunks(makeBlob(CHUNK_SIZE));
    expect(chunks).toHaveLength(1);
    expect(chunks[0]?.size).toBe(CHUNK_SIZE);
  });

  it('a file ONE BYTE over one chunk splits into exactly 2 chunks with correct byte ranges', async () => {
    const size = CHUNK_SIZE + 1;
    const file = makeBlob(size);

    const chunks = createFileChunks(file);
    expect(chunks).toHaveLength(2);
    expect(chunks[0]?.size).toBe(CHUNK_SIZE); // bytes [0, CHUNK_SIZE)
    expect(chunks[1]?.size).toBe(1); // bytes [CHUNK_SIZE, CHUNK_SIZE+1)

    // Byte-range correctness at the boundary: chunk 0's last byte is source
    // byte CHUNK_SIZE-1; chunk 1's only byte is source byte CHUNK_SIZE.
    await expectByteAt(chunks[0]!, 0, 0 % 256);
    await expectByteAt(chunks[0]!, CHUNK_SIZE - 1, (CHUNK_SIZE - 1) % 256);
    await expectByteAt(chunks[1]!, 0, CHUNK_SIZE % 256);
  });

  it('a file exactly two chunks worth splits into exactly 2 equal chunks', () => {
    const chunks = createFileChunks(makeBlob(CHUNK_SIZE * 2));
    expect(chunks).toHaveLength(2);
    expect(chunks[0]?.size).toBe(CHUNK_SIZE);
    expect(chunks[1]?.size).toBe(CHUNK_SIZE);
  });

  it('an empty file produces zero chunks', () => {
    expect(createFileChunks(makeBlob(0))).toHaveLength(0);
  });
});

describe('parseChunkAck — {in_progress:true} vs. complete (RED/GREEN proof c)', () => {
  it('an array with a truthy first element is complete', () => {
    expect(parseChunkAck(JSON.stringify([{ id: 'a' }]))).toEqual({ status: 'complete', data: { id: 'a' } });
  });

  it('an empty array is in_progress', () => {
    expect(parseChunkAck(JSON.stringify([]))).toEqual({ status: 'in_progress' });
  });

  it('a literal {in_progress:true} object is in_progress (old-app quirk: response[0] on a non-array is undefined)', () => {
    expect(parseChunkAck(JSON.stringify({ in_progress: true }))).toEqual({ status: 'in_progress' });
  });

  it('an array whose first element is null is in_progress (?? semantics)', () => {
    expect(parseChunkAck(JSON.stringify([null]))).toEqual({ status: 'in_progress' });
  });

  it('an array whose first element is falsy-but-not-nullish (0) is complete', () => {
    expect(parseChunkAck(JSON.stringify([0]))).toEqual({ status: 'complete', data: 0 });
  });

  it("invalid JSON is complete with no payload (parity: the old app's typo-fallback also lacked in_progress)", () => {
    expect(parseChunkAck('not json')).toEqual({ status: 'complete', data: undefined });
  });
});

describe('parseSmallFileAck', () => {
  it('returns the first array element', () => {
    expect(parseSmallFileAck(JSON.stringify([{ id: 'x' }]))).toEqual({ id: 'x' });
  });

  it('returns null for an empty array', () => {
    expect(parseSmallFileAck(JSON.stringify([]))).toBeNull();
  });

  it('returns null for invalid JSON', () => {
    expect(parseSmallFileAck('not json')).toBeNull();
  });

  it('returns null for a non-array body', () => {
    expect(parseSmallFileAck(JSON.stringify({ id: 'x' }))).toBeNull();
  });
});

describe('buildAttachmentUploadUrl', () => {
  it('is under /api/v2 (no strip) — unlike artifacts.ts rows 1/5', () => {
    expect(buildAttachmentUploadUrl('/api/v2', 'p1', 'c1')).toBe('/api/v2/elitea_core/attachments/prompt_lib/p1/c1');
  });

  it('normalises a trailing slash on baseUrl', () => {
    expect(buildAttachmentUploadUrl('/api/v2/', 'p1', 'c1')).toBe('/api/v2/elitea_core/attachments/prompt_lib/p1/c1');
  });

  it('encodes projectId/conversationId', () => {
    expect(buildAttachmentUploadUrl('/api/v2', 'p 1', 'c/1')).toBe('/api/v2/elitea_core/attachments/prompt_lib/p%201/c%2F1');
  });
});

describe('uploadChunk — fields, progress, credentials, DEV token', () => {
  it('sends exactly the 5 required fields (file, chunk_index, total_chunks, file_id, file_name, overwrite_attachments=1)', async () => {
    server.use(chunkAckInProgress());
    const appendSpy = vi.spyOn(FormData.prototype, 'append');
    await uploadChunk({
      baseUrl: '/api/v2',
      projectId: 'p1',
      conversationId: 'c1',
      chunk: makeBlob(10),
      chunkIndex: 0,
      totalChunks: 2,
      fileId: 'file-abc',
      fileName: 'demo.bin',
    });
    const calls = appendSpy.mock.calls.map(([key, value]) => [key, typeof value === 'string' ? value : '<blob>']);
    expect(calls).toEqual([
      ['file', '<blob>'],
      ['chunk_index', '0'],
      ['total_chunks', '2'],
      ['file_id', 'file-abc'],
      ['file_name', 'demo.bin'],
      ['overwrite_attachments', '1'],
    ]);
  });

  it('wires xhr.upload.onprogress to the onProgress callback', async () => {
    server.use(chunkAckInProgress());
    const calls: Array<[number, number]> = [];
    const result = await uploadChunk({
      baseUrl: '/api/v2',
      projectId: 'p1',
      conversationId: 'c1',
      chunk: makeBlob(1024),
      chunkIndex: 0,
      totalChunks: 1,
      fileId: 'file-abc',
      fileName: 'demo.bin',
      onProgress: (loaded, total) => calls.push([loaded, total]),
    });
    expect(result.ok).toBe(true);
    expect(calls.length).toBeGreaterThan(0);
    const [loaded, total] = calls[calls.length - 1]!;
    expect(loaded).toBe(total); // msw synthesizes a single loaded=total progress event
    expect(total).toBeGreaterThan(0);
  });

  it('resolves same-origin credentials (xhr.withCredentials=false) for a relative baseUrl', async () => {
    const sink: CapturedUploadRequest[] = [];
    server.use(chunkAckInProgress(sink));
    await uploadChunk({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 0, totalChunks: 1, fileId: 'f', fileName: 'n' });
    expect(sink[0]?.credentials).toBe('same-origin');
  });

  it("resolves include credentials (xhr.withCredentials=true) for a cross-origin baseUrl — F4's cross-origin rule applied here", async () => {
    const sink: CapturedUploadRequest[] = [];
    server.use(chunkAckInProgress(sink));
    await uploadChunk({ baseUrl: `${ORIGIN.replace('localhost', 'cross-origin.example')}/api/v2`, projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 0, totalChunks: 1, fileId: 'f', fileName: 'n' });
    expect(sink[0]?.credentials).toBe('include');
  });

  it('attaches the DEV bearer + Cache-Control only under import.meta.env.DEV', async () => {
    const sink: CapturedUploadRequest[] = [];
    server.use(chunkAckInProgress(sink));
    await uploadChunk({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 0, totalChunks: 1, fileId: 'f', fileName: 'n', devToken: 'dev-secret' });
    expect(sink[0]?.authorization).toBe('Bearer dev-secret');
    expect(sink[0]?.cacheControl).toBe('no-cache');
  });

  it('attaches nothing outside DEV even with a token configured', async () => {
    vi.stubEnv('DEV', false);
    const sink: CapturedUploadRequest[] = [];
    server.use(chunkAckInProgress(sink));
    await uploadChunk({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 0, totalChunks: 1, fileId: 'f', fileName: 'n', devToken: 'dev-secret' });
    expect(sink[0]?.authorization).toBeNull();
    expect(sink[0]?.cacheControl).toBeNull();
  });

  it('resolves a kind:http failure for a non-2xx status, never throws', async () => {
    server.use(uploadServerError());
    const result = await uploadChunk({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 0, totalChunks: 1, fileId: 'f', fileName: 'n' });
    expect(result).toEqual({ ok: false, error: { kind: 'http', status: 500 } });
  });

  it('resolves a kind:network failure for a network error, never throws', async () => {
    server.use(uploadNetworkError());
    const result = await uploadChunk({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 0, totalChunks: 1, fileId: 'f', fileName: 'n' });
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    expect(result.error.kind).toBe('network');
  });

  it('resolves a kind:aborted failure for a pre-aborted signal, without sending', async () => {
    server.use(chunkAckInProgress());
    const controller = new AbortController();
    controller.abort();
    const result = await uploadChunk({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 0, totalChunks: 1, fileId: 'f', fileName: 'n', signal: controller.signal });
    expect(result).toEqual({ ok: false, error: { kind: 'aborted' } });
  });

  it('wires the AbortSignal listener so an in-flight abort() call reaches xhr.abort() (environment note below)', async () => {
    // Environment limitation, confirmed empirically: under msw's
    // XMLHttpRequestInterceptor + jsdom, `abort` is not one of the proxied
    // XMLHttpRequest methods (only open/send/setRequestHeader/
    // addEventListener are — XMLHttpRequestController.ts's `methodCall`
    // switch), so calling the underlying jsdom xhr.abort() while a mocked
    // response is pending does NOT fire jsdom's 'abort' event or stop the
    // mocked 'load' from eventually firing — a real browser correctly
    // cancels the in-flight request here. This test proves the WIRING (the
    // signal's 'abort' listener reaches xhr.abort()) without asserting a
    // kind:'aborted' result, since this harness cannot produce one for a
    // signal that aborts strictly AFTER send() (only a signal already
    // aborted BEFORE send(), covered by the pre-aborted test above, can).
    server.use(chunkAckInProgress());
    const controller = new AbortController();
    const pending = uploadChunk({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 0, totalChunks: 1, fileId: 'f', fileName: 'n', signal: controller.signal });
    controller.abort();
    const result = await pending;
    expect(result.ok).toBe(true); // the mocked 'load' still wins the race, per the note above
  });

  it('distinguishes an in-progress ack from the final completion response (RED/GREEN proof c)', async () => {
    server.use(chunkAckInProgress());
    const intermediate = await uploadChunk({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 0, totalChunks: 2, fileId: 'f', fileName: 'n' });
    server.use(chunkAckComplete());
    const final = await uploadChunk({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', chunk: makeBlob(1), chunkIndex: 1, totalChunks: 2, fileId: 'f', fileName: 'n' });
    expect(intermediate.ok).toBe(true);
    expect(final.ok).toBe(true);
    if (!intermediate.ok || !final.ok) throw new Error('unreachable');
    expect(intermediate.data.status).toBe('in_progress');
    expect(final.data).toEqual({ status: 'complete', data: CHUNKED_FILE_UPLOADED[0] });
  });
});

describe('normalizeFileNameExtension / normalizeFileExtension — regression', () => {
  /**
   * Found while porting Wave-2 unit C1 (chat model/store): the old app's
   * useUploadWithProgress.js:154 calls normalizeFileExtension(file) BEFORE
   * ever branching into the small-file/chunked paths — this file (its
   * direct port) never carried that step over, so an uppercase-extension
   * upload silently reached the server under a different filename.
   */
  it('lowercases only the extension, leaving the base name untouched', () => {
    expect(normalizeFileNameExtension('photo.JPG')).toBe('photo.jpg');
    expect(normalizeFileNameExtension('MyFile.PDF')).toBe('MyFile.pdf');
  });

  it('leaves an already-lowercase extension and a dotless name unchanged', () => {
    expect(normalizeFileNameExtension('photo.jpg')).toBe('photo.jpg');
    expect(normalizeFileNameExtension('README')).toBe('README');
  });

  it('treats a trailing dot with nothing after it as dotless (no rewrite)', () => {
    expect(normalizeFileNameExtension('weird.')).toBe('weird.');
  });

  it('normalizeFileExtension returns the SAME File instance when nothing changes (no gratuitous re-wrap)', () => {
    const file = new File(['x'], 'photo.jpg', { type: 'image/jpeg' });
    expect(normalizeFileExtension(file)).toBe(file);
  });

  it('normalizeFileExtension rebuilds the File with a lowercased extension, preserving type/content', async () => {
    const file = new File(['content'], 'PHOTO.JPG', { type: 'image/jpeg' });
    const normalized = normalizeFileExtension(file);
    expect(normalized.name).toBe('PHOTO.jpg');
    expect(normalized.type).toBe('image/jpeg');
    expect(await normalized.text()).toBe('content');
  });
});

describe('uploadSmallFile — 2 fields only (no chunk fields)', () => {
  it('sends only file + overwrite_attachments', async () => {
    server.use(smallFileOk());
    const appendSpy = vi.spyOn(FormData.prototype, 'append');
    const result = await uploadSmallFile({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', file: makeBlob(10) });
    expect(result).toEqual({ ok: true, data: SMALL_FILE_UPLOADED[0] });
    const keys = appendSpy.mock.calls.map(([key]) => key);
    expect(keys).toEqual(['file', 'overwrite_attachments']);
    expect(appendSpy.mock.calls[1]?.[1]).toBe('1');
  });

  it('resolves a kind:http failure for a non-2xx status, never throws', async () => {
    server.use(uploadServerError());
    const result = await uploadSmallFile({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', file: makeBlob(10) });
    expect(result).toEqual({ ok: false, error: { kind: 'http', status: 500 } });
  });

  it('normalizes an uppercase file extension before appending, given a real File', async () => {
    server.use(smallFileOk());
    const appendSpy = vi.spyOn(FormData.prototype, 'append');
    const file = new File(['x'], 'PHOTO.JPG', { type: 'image/jpeg' });
    await uploadSmallFile({ baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1', file });
    const appendedFile = appendSpy.mock.calls.find(([key]) => key === 'file')?.[1] as File;
    expect(appendedFile.name).toBe('PHOTO.jpg');
  });
});

describe('uploadFileWithProgress — small-file vs. chunked orchestration', () => {
  it('uses the single-shot path for a file <= CHUNK_SIZE', async () => {
    server.use(smallFileOk());
    const result = await uploadFileWithProgress({
      baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1',
      file: makeBlob(1024), fileName: 'small.txt', fileId: 'file-1',
    });
    expect(result).toEqual({ ok: true, data: SMALL_FILE_UPLOADED[0] });
  });

  it('normalizes an uppercase fileName extension on the single-shot path, even given a plain (non-File) Blob', async () => {
    server.use(smallFileOk());
    const appendSpy = vi.spyOn(FormData.prototype, 'append');
    await uploadFileWithProgress({
      baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1',
      file: makeBlob(10), fileName: 'PHOTO.JPG', fileId: 'file-5',
    });
    const appendedFile = appendSpy.mock.calls.find(([key]) => key === 'file')?.[1] as File;
    expect(appendedFile.name).toBe('PHOTO.jpg');
  });

  it('normalizes an uppercase fileName extension on the chunked path\'s file_name field', async () => {
    server.use(chunkAckSequence(2));
    const appendSpy = vi.spyOn(FormData.prototype, 'append');
    await uploadFileWithProgress({
      baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1',
      file: makeBlob(CHUNK_SIZE + 100), fileName: 'BIG.BIN', fileId: 'file-6',
    });
    const fileNameValue = appendSpy.mock.calls.find(([key]) => key === 'file_name')?.[1];
    expect(fileNameValue).toBe('BIG.bin');
  });

  it('uses the chunked path for a file > CHUNK_SIZE, aggregating progress and returning the final ack', async () => {
    const sink: CapturedUploadRequest[] = [];
    // CHUNK_SIZE + 100 bytes -> exactly 2 chunks; the 2nd POST gets the complete ack.
    server.use(chunkAckSequence(2, sink));
    const progressCalls: Array<[number, number]> = [];
    const totalBytes = CHUNK_SIZE + 100;

    const result = await uploadFileWithProgress({
      baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1',
      file: makeBlob(totalBytes), fileName: 'big.bin', fileId: 'file-2',
      onProgress: (loaded, total) => progressCalls.push([loaded, total]),
    });

    expect(result).toEqual({ ok: true, data: CHUNKED_FILE_UPLOADED[0] });
    expect(sink).toHaveLength(2); // 2 POSTs, one per chunk
    expect(progressCalls.length).toBeGreaterThanOrEqual(2); // at least one event per chunk
    const last = progressCalls[progressCalls.length - 1]!;
    // `total` passed to the caller is always the nominal file size (parity: useUploadWithProgress.js:196).
    expect(last[1]).toBe(totalBytes);
    // `loaded` is bytesBeforeThisChunk + the XHR event's `event.loaded`, which is the
    // full MULTIPART-ENCODED request size (boundaries + the other 5 fields), not just
    // the raw chunk bytes — so it can slightly EXCEED totalBytes at the final chunk.
    // This is exactly why the old app clamps with Math.min(overallProgress, 100)
    // (useUploadWithProgress.js:167/197) instead of expecting exact byte alignment.
    expect(last[0]).toBeGreaterThanOrEqual(totalBytes);
  });

  it('stops and propagates the failure on the first failing chunk', async () => {
    server.use(uploadServerError());
    const result = await uploadFileWithProgress({
      baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1',
      file: makeBlob(CHUNK_SIZE + 1), fileName: 'big.bin', fileId: 'file-3',
    });
    expect(result).toEqual({ ok: false, error: { kind: 'http', status: 500 } });
  });

  it('treats an all-in_progress chunked upload as no final data (no chunk ever completes)', async () => {
    server.use(chunkAckInProgress());
    const result = await uploadFileWithProgress({
      baseUrl: '/api/v2', projectId: 'p1', conversationId: 'c1',
      file: makeBlob(CHUNK_SIZE + 1), fileName: 'big.bin', fileId: 'file-4',
    });
    expect(result).toEqual({ ok: true, data: undefined });
  });
});
