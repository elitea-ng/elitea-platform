import { beforeEach, describe, expect, it, vi } from 'vitest';

import * as sharedArtifacts from '@/shared/api/artifacts';
import { getConfig } from '@/shared/config';
import * as runtimeConfig from '@/shared/config';

import {
  artifactObjectExists,
  listArtifactBucketNames,
  openArtifactFileInCanvas,
  saveCanvasToArtifact,
} from './canvasFileTransfer';

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(sharedArtifacts, 'fetchArtifactBlob');
  vi.spyOn(sharedArtifacts, 'uploadArtifactObject');
  vi.spyOn(sharedArtifacts, 'listBuckets');
  vi.spyOn(sharedArtifacts, 'listArtifacts');
  vi.spyOn(runtimeConfig, 'getConfig');
  vi.mocked(getConfig).mockReturnValue({
    status: 'ok',
    config: {
      vite_server_url: '/api/v2',
      vite_base_uri: '/',
      vite_public_project_id: 'public',
      allow_project_own_llms: false,
    },
  });
});

describe('openArtifactFileInCanvas', () => {
  it('fetches the object and returns a canvas-ready document with its source', async () => {
    const textBlob = { text: vi.fn().mockResolvedValue('print(1)'), size: 8 } as unknown as Blob;
    vi.mocked(sharedArtifacts.fetchArtifactBlob).mockResolvedValue({
      ok: true,
      status: 200,
      data: textBlob,
      headers: new Headers({ etag: 'W/"abc"' }),
    });

    const result = await openArtifactFileInCanvas({ projectId: 'p1', bucket: 'docs', name: 'script.py' });
    expect(result).toEqual({
      ok: true,
      file: {
        codeBlock: 'print(1)',
        language: 'python',
        type: 'code',
        source: { bucket: 'docs', name: 'script.py', etag: 'W/"abc"' },
      },
    });
    expect(sharedArtifacts.fetchArtifactBlob).toHaveBeenCalledWith(
      expect.objectContaining({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'docs', filePath: 'script.py' }),
    );
  });

  it('refuses an unsupported kind WITHOUT fetching anything', async () => {
    const result = await openArtifactFileInCanvas({ projectId: 'p1', bucket: 'docs', name: 'photo.png' });
    expect(result).toEqual({ ok: false, reason: 'unsupported-kind' });
    expect(sharedArtifacts.fetchArtifactBlob).not.toHaveBeenCalled();
  });

  it('names a .docx refusal distinctly (issue #879) — a recognised office format, not an unheard-of extension', async () => {
    const result = await openArtifactFileInCanvas({ projectId: 'p1', bucket: 'docs', name: 'report.docx' });
    expect(result).toEqual({ ok: false, reason: 'unsupported-format' });
    expect(sharedArtifacts.fetchArtifactBlob).not.toHaveBeenCalled();
  });

  it('refuses an oversized file by its KNOWN size, before fetching', async () => {
    const result = await openArtifactFileInCanvas({ projectId: 'p1', bucket: 'docs', name: 'big.py', size: 3 * 1024 * 1024 });
    expect(result).toEqual({ ok: false, reason: 'too-large' });
    expect(sharedArtifacts.fetchArtifactBlob).not.toHaveBeenCalled();
  });

  it('also catches an oversized file whose bytes only turn out large once fetched', async () => {
    const bigBlob = { text: vi.fn().mockResolvedValue('x'), size: 3 * 1024 * 1024 } as unknown as Blob;
    vi.mocked(sharedArtifacts.fetchArtifactBlob).mockResolvedValue({ ok: true, status: 200, data: bigBlob, headers: new Headers() });
    const result = await openArtifactFileInCanvas({ projectId: 'p1', bucket: 'docs', name: 'notes.txt' });
    expect(result).toEqual({ ok: false, reason: 'too-large' });
  });

  it('reports a failed fetch', async () => {
    vi.mocked(sharedArtifacts.fetchArtifactBlob).mockResolvedValue({
      ok: false,
      error: { kind: 'http', status: 404, url: '/x', body: undefined },
    });
    const result = await openArtifactFileInCanvas({ projectId: 'p1', bucket: 'docs', name: 'notes.txt' });
    expect(result).toEqual({ ok: false, reason: 'fetch-failed' });
  });
});

describe('saveCanvasToArtifact', () => {
  it('uploads the document as the object key', async () => {
    vi.mocked(sharedArtifacts.uploadArtifactObject).mockResolvedValue({ ok: true, status: 200, data: undefined, headers: new Headers() });
    const result = await saveCanvasToArtifact({ projectId: 'p1', bucket: 'docs', name: 'notes.txt', content: 'hello' });
    expect(result).toEqual({ ok: true });
    expect(sharedArtifacts.uploadArtifactObject).toHaveBeenCalledWith(
      expect.objectContaining({ baseUrl: '/api/v2', projectId: 'p1', bucket: 'docs', fileKey: 'notes.txt' }),
    );
  });

  it('reports a failed upload', async () => {
    vi.mocked(sharedArtifacts.uploadArtifactObject).mockResolvedValue({
      ok: false,
      error: { kind: 'http', status: 500, url: '/x', body: undefined },
    });
    const result = await saveCanvasToArtifact({ projectId: 'p1', bucket: 'docs', name: 'notes.txt', content: 'hello' });
    expect(result).toEqual({ ok: false, reason: 'upload-failed' });
  });
});

describe('listArtifactBucketNames', () => {
  it('normalises the bucket list and drops system buckets', async () => {
    vi.mocked(sharedArtifacts.listBuckets).mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers(),
      data: {
        buckets: [
          { name: 'zeta', is_pinned: false, created_at: '2026-01-01T00:00:00Z' },
          { name: 'alpha', is_pinned: true, created_at: '2026-02-01T00:00:00Z' },
        ],
      },
    });
    await expect(listArtifactBucketNames('p1')).resolves.toEqual(['alpha', 'zeta']);
  });

  it('returns an empty list rather than throwing on a failed read', async () => {
    vi.mocked(sharedArtifacts.listBuckets).mockResolvedValue({
      ok: false,
      error: { kind: 'http', status: 500, url: '/x', body: undefined },
    });
    await expect(listArtifactBucketNames('p1')).resolves.toEqual([]);
  });
});

describe('artifactObjectExists', () => {
  it('finds an exact key match', async () => {
    vi.mocked(sharedArtifacts.listArtifacts).mockResolvedValue({
      ok: true,
      status: 200,
      headers: new Headers(),
      data: { objects: [{ key: 'notes.txt' }, { key: 'other.txt' }], common_prefixes: [] },
    });
    await expect(artifactObjectExists('p1', 'docs', 'notes.txt')).resolves.toBe(true);
    await expect(artifactObjectExists('p1', 'docs', 'missing.txt')).resolves.toBe(false);
  });
});
