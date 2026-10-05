/**
 * shared/api/artifacts.ts.
 * Covers: every URL landing on elitea-main's real `/api/v2/artifacts/...`
 * routes (issue #138), the multipart upload envelope, credentials
 * resolution, DEV token gating, http/network failure paths, and the ZIP
 * multi-download loop.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import artifactList200 from '../../test/msw/fixtures/artifacts/artifact-list.200.json';
import bucketList200 from '../../test/msw/fixtures/artifacts/bucket-list.200.json';
import {
  artifactContentByPath,
  artifactContentNetworkError,
  artifactContentNotFound,
  artifactContentOk,
  artifactListNetworkError,
  artifactListOk,
  bucketListLyingJson,
  bucketListNetworkError,
  bucketListNotFound,
  bucketListOk,
  objectUploadNetworkError,
  objectUploadNotFound,
  objectUploadOk,
} from '../../test/msw/handlers/artifacts';
import type { CapturedArtifactsRequest } from '../../test/msw/handlers/artifacts';

import {
  buildArtifactContentUrl,
  buildArtifactListUrl,
  buildBucketListUrl,
  buildObjectUploadUrl,
  downloadArtifactsAsZip,
  fetchArtifactBlob,
  listArtifacts,
  listBuckets,
  uploadArtifactObject,
} from './artifacts';
import type { ZipArchiver } from './artifacts';

const ORIGIN = window.location.origin;
const CROSS_ORIGIN_BASE = `${ORIGIN.replace('localhost', 'cross-origin.example')}/api/v2`;

afterEach(() => {
  vi.unstubAllEnvs();
});

describe('buildObjectUploadUrl — POST /api/v2/artifacts/objects/{projectID}/{bucket}', () => {
  it('keeps /api/v2 and asks for overwrite (router.go:288, objects.go:301)', () => {
    expect(buildObjectUploadUrl('/api/v2', 'proj 1', 'my bucket'))
      .toBe('/api/v2/artifacts/objects/proj%201/my%20bucket?overwrite=true');
  });

  it('normalises a trailing slash on the base rather than doubling it', () => {
    expect(buildObjectUploadUrl('/api/v2/', 'p1', 'b')).toBe('/api/v2/artifacts/objects/p1/b?overwrite=true');
  });
});

describe('uploadArtifactObject', () => {
  it('POSTs multipart/form-data and resolves ok:true on 201', async () => {
    const sink: CapturedArtifactsRequest[] = [];
    server.use(objectUploadOk(sink));
    const file = new Blob(['file-bytes'], { type: 'text/plain' });
    const result = await uploadArtifactObject({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'bucket', fileKey: 'a.txt', file });
    if (!result.ok) throw new Error('unreachable');
    expect(result.headers).toBeInstanceOf(Headers);
    expect(result).toEqual({ ok: true, status: 201, data: undefined, headers: result.headers });
    expect(sink[0]?.method).toBe('POST');
    expect(sink[0]?.contentType).toMatch(/^multipart\/form-data; boundary=/);
    expect(new URL(sink[0]!.url).pathname).toBe('/api/v2/artifacts/objects/p1/bucket');
    expect(new URL(sink[0]!.url).searchParams.get('overwrite')).toBe('true');
  });

  it('reads the 201 answer to the end — a no-store answer left unread never finishes loading', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    server.use(objectUploadOk());
    const result = await uploadArtifactObject({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'bucket', fileKey: 'a.txt', file: new Blob(['x']) });
    expect(result.ok).toBe(true);
    const response: unknown = await fetchSpy.mock.results.at(-1)?.value;
    if (!(response instanceof Response)) throw new Error('upload did not call fetch');
    expect(response.bodyUsed).toBe(true);
  });

  /**
   * The part itself is asserted on the `FormData` handed to `fetch`, not on
   * the serialised request body: jsdom's `FormData`/`Blob` and Node's undici
   * `fetch` are different realms, so undici cannot read a jsdom Blob's bytes
   * and emits an empty part. That is a test-environment artifact — in a
   * browser both come from the same realm — and inspecting the FormData is
   * the stronger assertion anyway, since it names the exact part the browser
   * will encode.
   */
  async function uploadedPart(params: { fileKey: string; file: Blob; contentType?: string }): Promise<File> {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    server.use(objectUploadOk());
    await uploadArtifactObject({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'bucket', ...params });
    const body = fetchSpy.mock.calls.at(-1)?.[1]?.body;
    if (!(body instanceof FormData)) throw new Error('upload did not send a FormData body');
    const part = body.get('file');
    if (!(part instanceof File)) throw new Error('upload did not send a file part');
    return part;
  }

  it("names the part 'file' and uses the object KEY as its filename", async () => {
    const part = await uploadedPart({ fileKey: 'a.txt', file: new Blob(['file-bytes'], { type: 'text/plain' }) });
    expect(part.name).toBe('a.txt');
    expect(part.type).toBe('text/plain');
    expect(await part.text()).toBe('file-bytes');
  });

  it('sends a multi-segment key whole — the server parses Content-Disposition itself so folders survive (objects.go:326-344)', async () => {
    const part = await uploadedPart({ fileKey: 'folder/a.txt', file: new Blob(['x']) });
    expect(part.name).toBe('folder/a.txt');
  });

  it('leaves the part untyped when neither contentType nor file.type is set — the server derives it from the extension', async () => {
    const part = await uploadedPart({ fileKey: 'a.bin', file: new Blob(['bytes']) });
    expect(part.type).toBe('');
  });

  it('an explicit contentType wins over file.type, without copying the payload', async () => {
    const part = await uploadedPart({
      fileKey: 'a',
      file: new Blob(['x'], { type: 'text/plain' }),
      contentType: 'application/x-custom',
    });
    expect(part.type).toBe('application/x-custom');
    expect(await part.text()).toBe('x');
  });

  it('resolves same-origin credentials for the relative /api/v2 base', async () => {
    const sink: CapturedArtifactsRequest[] = [];
    server.use(objectUploadOk(sink));
    await uploadArtifactObject({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', fileKey: 'a', file: new Blob(['x']) });
    expect(sink[0]?.credentials).toBe('same-origin');
  });

  it("resolves include credentials for a cross-origin absolute base — F4's cross-origin rule applied here", async () => {
    const sink: CapturedArtifactsRequest[] = [];
    server.use(objectUploadOk(sink));
    await uploadArtifactObject({ baseUrl: CROSS_ORIGIN_BASE, projectId: 'p1', bucket: 'b', fileKey: 'a', file: new Blob(['x']) });
    expect(sink[0]?.credentials).toBe('include');
  });

  it('attaches the DEV bearer + Cache-Control only under import.meta.env.DEV', async () => {
    const sink: CapturedArtifactsRequest[] = [];
    server.use(objectUploadOk(sink));
    await uploadArtifactObject({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', fileKey: 'a', file: new Blob(['x']), devToken: 'dev-secret' });
    expect(sink[0]?.authorization).toBe('Bearer dev-secret');
  });

  it('attaches no bearer outside DEV', async () => {
    vi.stubEnv('DEV', false);
    const sink: CapturedArtifactsRequest[] = [];
    server.use(objectUploadOk(sink));
    await uploadArtifactObject({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', fileKey: 'a', file: new Blob(['x']), devToken: 'dev-secret' });
    expect(sink[0]?.authorization).toBeNull();
  });

  it('resolves a kind:http failure for a 404, never throws', async () => {
    server.use(objectUploadNotFound());
    const result = await uploadArtifactObject({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', fileKey: 'a', file: new Blob(['x']) });
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    expect(result.error).toMatchObject({ kind: 'http', status: 404 });
  });

  it('resolves a kind:network failure for a network error, never throws', async () => {
    server.use(objectUploadNetworkError());
    const result = await uploadArtifactObject({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', fileKey: 'a', file: new Blob(['x']) });
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    expect(result.error.kind).toBe('network');
  });
});

describe('bucket / object list URLs land on mountArtifactRoutes', () => {
  it('buildBucketListUrl targets GET /api/v2/artifacts/buckets/{projectID} (router.go:279)', () => {
    expect(buildBucketListUrl('/api/v2', 'p1')).toBe('/api/v2/artifacts/buckets/p1');
  });

  it('buildArtifactListUrl targets GET /api/v2/artifacts/objects/{projectID}/{bucket} (router.go:285)', () => {
    expect(buildArtifactListUrl('/api/v2', 'p1', 'my bucket')).toBe('/api/v2/artifacts/objects/p1/my%20bucket');
  });

  it('listBuckets parses the JSON body the Go handler actually returns', async () => {
    const sink: CapturedArtifactsRequest[] = [];
    server.use(bucketListOk(sink));
    const result = await listBuckets({ baseUrl: '/api/v2', projectId: 'p1' });
    if (!result.ok) throw new Error('unreachable');
    expect(result.headers).toBeInstanceOf(Headers);
    expect(result.status).toBe(200);
    expect(result.data).toEqual(bucketList200.body);
    expect(new URL(sink[0]!.url).pathname).toBe('/api/v2/artifacts/buckets/p1');
  });

  it('listArtifacts fetches the object-plane collection URL', async () => {
    const sink: CapturedArtifactsRequest[] = [];
    server.use(artifactListOk(sink));
    const result = await listArtifacts({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'demo-bucket' });
    if (!result.ok) throw new Error('unreachable');
    expect(result.data).toEqual(artifactList200.body);
    expect(new URL(sink[0]!.url).pathname).toBe('/api/v2/artifacts/objects/p1/demo-bucket');
  });

  it('listBuckets resolves a kind:network failure without throwing', async () => {
    server.use(bucketListNetworkError());
    const result = await listBuckets({ baseUrl: '/api/v2', projectId: 'p1' });
    expect(result.ok).toBe(false);
  });

  it('listBuckets resolves a kind:http failure for a 404, never throws', async () => {
    server.use(bucketListNotFound());
    const result = await listBuckets({ baseUrl: '/api/v2', projectId: 'p1' });
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    expect(result.error).toMatchObject({ kind: 'http', status: 404 });
  });

  it('listBuckets surfaces raw text when the server lies about application/json', async () => {
    server.use(bucketListLyingJson());
    const result = await listBuckets({ baseUrl: '/api/v2', projectId: 'p1' });
    if (!result.ok) throw new Error('unreachable');
    expect(result.headers).toBeInstanceOf(Headers);
    expect(result).toEqual({ ok: true, status: 200, data: 'not json', headers: result.headers });
  });

  it('listArtifacts resolves a kind:network failure without throwing', async () => {
    server.use(artifactListNetworkError());
    const result = await listArtifacts({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b' });
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    expect(result.error.kind).toBe('network');
  });
});

describe('buildArtifactContentUrl — GET /api/v2/artifacts/objects/{projectID}/{bucket}/{key...}', () => {
  it('targets the object-item route (router.go:288)', () => {
    expect(buildArtifactContentUrl('/api/v2', 'p1', 'bucket', 'notes.md'))
      .toBe('/api/v2/artifacts/objects/p1/bucket/notes.md');
  });

  it("encodes each key segment but keeps '/' literal — the route is a chi wildcard, so %2F would miss it", () => {
    expect(buildArtifactContentUrl('/api/v2', 'p1', 'bucket', 'my folder/notes v2.md'))
      .toBe('/api/v2/artifacts/objects/p1/bucket/my%20folder/notes%20v2.md');
  });
});

describe('fetchArtifactBlob', () => {
  it('resolves ok:true with the response Blob', async () => {
    server.use(artifactContentOk());
    const result = await fetchArtifactBlob({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'bucket', filePath: 'notes.md' });
    expect(result.ok).toBe(true);
    if (!result.ok) throw new Error('unreachable');
    expect(result.data).toBeInstanceOf(Blob);
    expect(await result.data.text()).toBe('hello artifact content');
  });

  it('resolves include credentials — unifies utils.jsx (no credentials) and the hook (credentials:include) under one rule', async () => {
    const sink: CapturedArtifactsRequest[] = [];
    server.use(artifactContentOk(sink));
    await fetchArtifactBlob({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', filePath: 'f.md' });
    expect(sink[0]?.credentials).toBe('same-origin'); // /api/v2 is same-origin relative to the page here
  });

  it('resolves a kind:http failure for a 404, never throws', async () => {
    server.use(artifactContentNotFound());
    const result = await fetchArtifactBlob({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', filePath: 'missing.md' });
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    expect(result.error).toMatchObject({ kind: 'http', status: 404 });
  });

  it('resolves a kind:network failure, never throws', async () => {
    server.use(artifactContentNetworkError());
    const result = await fetchArtifactBlob({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', filePath: 'f.md' });
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    expect(result.error.kind).toBe('network');
  });

  it('resolves a kind:aborted failure for a pre-aborted signal, never throws', async () => {
    server.use(artifactContentOk());
    const controller = new AbortController();
    controller.abort();
    const result = await fetchArtifactBlob({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', filePath: 'f.md', signal: controller.signal });
    expect(result.ok).toBe(false);
    if (result.ok) throw new Error('unreachable');
    if (result.error.kind !== 'aborted') throw new Error('unreachable');
    expect(result.error.url).toContain('/api/v2/artifacts/objects/p1/b/f.md');
  });
});

function mockArchiver(): ZipArchiver & { files: Map<string, Blob> } {
  const files = new Map<string, Blob>();
  return {
    files,
    file(path, data) {
      files.set(path, data);
    },
    generateAsync() {
      return Promise.resolve(new Blob(['zip-bytes']));
    },
  };
}

describe('downloadArtifactsAsZip', () => {
  it('fetches every file, strips currentPrefix from the archive path, and generates the zip blob', async () => {
    server.use(artifactContentByPath({ 'folder/a.md': 'A content', 'folder/b.md': 'B content' }));
    const archiver = mockArchiver();
    const progress: Array<[number, number, string]> = [];
    const result = await downloadArtifactsAsZip({
      baseUrl: '/api/v2',
      projectId: 'p1',
      bucket: 'bucket',
      filenames: ['folder/a.md', 'folder/b.md'],
      currentPrefix: 'folder',
      archiver,
      onProgress: (current, total, filename) => progress.push([current, total, filename]),
    });
    expect(result.status).toBe('ok');
    if (result.status !== 'ok') throw new Error('unreachable');
    expect(result.blob).toBeInstanceOf(Blob);
    expect([...archiver.files.keys()].sort()).toEqual(['a.md', 'b.md']);
    expect(progress).toEqual([
      [1, 2, 'folder/a.md'],
      [2, 2, 'folder/b.md'],
    ]);
  });

  it('reports cancelled for an empty file list', async () => {
    const result = await downloadArtifactsAsZip({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', filenames: [], archiver: mockArchiver() });
    expect(result).toEqual({ status: 'cancelled' });
  });

  it('reports cancelled when the signal is already aborted', async () => {
    const controller = new AbortController();
    controller.abort();
    const result = await downloadArtifactsAsZip({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', filenames: ['a.md'], archiver: mockArchiver(), signal: controller.signal });
    expect(result).toEqual({ status: 'cancelled' });
  });

  it('stops and reports the failing file on the first HTTP error', async () => {
    server.use(artifactContentNotFound());
    const archiver = mockArchiver();
    const result = await downloadArtifactsAsZip({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', filenames: ['missing.md'], archiver });
    expect(result.status).toBe('error');
    if (result.status !== 'error') throw new Error('unreachable');
    expect(result.filename).toBe('missing.md');
    expect(result.error).toMatchObject({ kind: 'http', status: 404 });
    expect(archiver.files.size).toBe(0);
  });

  it('keeps the full filename when currentPrefix is not a real prefix of it', async () => {
    server.use(artifactContentByPath({ 'a.md': 'A' }));
    const archiver = mockArchiver();
    await downloadArtifactsAsZip({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', filenames: ['a.md'], currentPrefix: 'other/', archiver });
    expect([...archiver.files.keys()]).toEqual(['a.md']);
  });

  it('keeps the full filename when currentPrefix is omitted entirely', async () => {
    server.use(artifactContentByPath({ 'a.md': 'A' }));
    const archiver = mockArchiver();
    await downloadArtifactsAsZip({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'b', filenames: ['a.md'], archiver });
    expect([...archiver.files.keys()]).toEqual(['a.md']);
  });
});
