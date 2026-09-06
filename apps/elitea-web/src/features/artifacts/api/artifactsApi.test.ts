import { beforeEach, describe, expect, it, vi } from 'vitest';

import {
  batchDeleteObjects,
  createBucket,
  deleteBucket,
  deleteObject,
  updateBucket,
} from '@/shared/api/generated/artifacts/artifacts';
import { eliteaFetch } from '@/shared/api/generated/mutator';
import { listArtifacts, listBuckets } from '@/shared/api/artifacts';
import * as generatedArtifacts from '@/shared/api/generated/artifacts/artifacts';
import * as generatedMutator from '@/shared/api/generated/mutator';
import * as sharedArtifacts from '@/shared/api/artifacts';

import {
  createArtifactBucket,
  fetchArtifacts,
  fetchArtifactBuckets,
  fetchArtifactStorageConfigurations,
  removeArtifact,
  removeArtifacts,
  removeArtifactBucket,
  setArtifactBucketPinned,
  setArtifactBucketRetention,
} from './artifactsApi';

const okResponse = { data: {}, status: 200, headers: new Headers() };

beforeEach(() => {
  vi.restoreAllMocks();
  vi.spyOn(generatedArtifacts, 'createBucket');
  vi.spyOn(generatedArtifacts, 'deleteObject');
  vi.spyOn(generatedArtifacts, 'batchDeleteObjects');
  vi.spyOn(generatedArtifacts, 'deleteBucket');
  vi.spyOn(generatedArtifacts, 'updateBucket');
  vi.spyOn(generatedMutator, 'eliteaFetch');
  vi.spyOn(sharedArtifacts, 'listArtifacts');
  vi.spyOn(sharedArtifacts, 'listBuckets');
  vi.mocked(eliteaFetch).mockResolvedValue({ data: { rows: [] } });
});

describe('artifacts API', () => {
  it('normalises the bucket list and sorts pinned first — one call, no metadata merge', async () => {
    vi.mocked(listBuckets).mockResolvedValue({
      ok: true,
      data: {
        buckets: [
          { name: 'zeta', is_pinned: false, created_at: '2026-01-01T00:00:00Z' },
          { name: 'alpha', is_pinned: true, created_at: '2026-02-01T00:00:00Z' },
        ],
      },
      status: 200,
      headers: new Headers(),
    });
    await expect(fetchArtifactBuckets('/api/v2', 'p1')).resolves.toEqual([
      { id: 'alpha', name: 'alpha', isPinned: true, createdAt: '2026-02-01T00:00:00Z', retentionDays: null, sizeBytes: 0 },
      { id: 'zeta', name: 'zeta', isPinned: false, createdAt: '2026-01-01T00:00:00Z', retentionDays: null, sizeBytes: 0 },
    ]);
    // The legacy `/artifacts/buckets/default/{projectId}` metadata fetch is gone (#138).
    expect(eliteaFetch).not.toHaveBeenCalled();
  });

  it('excludes internal system buckets (tasks, reports) from the listing', async () => {
    vi.mocked(listBuckets).mockResolvedValue({
      ok: true,
      data: {
        buckets: [
          { name: 'docs', is_pinned: false, created_at: '2026-01-01T00:00:00Z' },
          { name: 'tasks', is_pinned: false, created_at: '2026-01-01T00:00:00Z' },
          { name: 'reports', is_pinned: false, created_at: '2026-01-01T00:00:00Z' },
        ],
      },
      status: 200,
      headers: new Headers(),
    });
    await expect(fetchArtifactBuckets('/api/v2', 'p1')).resolves.toEqual([
      { id: 'docs', name: 'docs', isPinned: false, createdAt: '2026-01-01T00:00:00Z', retentionDays: null, sizeBytes: 0 },
    ]);
  });

  it('rejects failed and malformed bucket responses', async () => {
    vi.mocked(listBuckets).mockResolvedValue({
      ok: false,
      error: { kind: 'http', status: 500, url: '/x', body: null },
    });
    await expect(fetchArtifactBuckets('/api/v2', 'p1')).rejects.toThrow('Unable');
    vi.mocked(listBuckets).mockResolvedValue({ ok: true, data: {}, status: 200, headers: new Headers() });
    await expect(fetchArtifactBuckets('/api/v2', 'p1')).rejects.toThrow('unexpected');
  });

  it('normalizes artifact listings and rejects malformed responses', async () => {
    vi.mocked(listArtifacts).mockResolvedValue({
      ok: true,
      data: {
        common_prefixes: [],
        objects: [{ key: 'a.txt', size_bytes: 2, media_type: 'text/plain', etag: 'e', modified_at: '2026-01-01T00:00:00Z' }],
      },
      status: 200,
      headers: new Headers(),
    });
    await expect(fetchArtifacts('/api/v2', 'p1', 'docs')).resolves.toEqual([
      { key: 'a.txt', size: 2, lastModified: '2026-01-01T00:00:00Z', bucket: 'docs' },
    ]);
    vi.mocked(listArtifacts).mockResolvedValue({ ok: true, data: { bad: true }, status: 200, headers: new Headers() });
    await expect(fetchArtifacts('/api/v2', 'p1', 'docs')).rejects.toThrow('unexpected');
  });

  it('delegates bucket and artifact mutations to generated endpoints', async () => {
    vi.mocked(createBucket).mockResolvedValue(okResponse as never);
    vi.mocked(updateBucket).mockResolvedValue(okResponse as never);
    vi.mocked(deleteBucket).mockResolvedValue(okResponse as never);
    vi.mocked(deleteObject).mockResolvedValue(okResponse as never);
    vi.mocked(batchDeleteObjects).mockResolvedValue(okResponse as never);

    await createArtifactBucket('1', 'docs');
    await setArtifactBucketRetention('1', 'docs', 30);
    await setArtifactBucketPinned('1', 'reports', true);
    await removeArtifactBucket('1', 'reports');
    await removeArtifact('1', 'docs', 'a.txt');
    await removeArtifacts('1', 'docs', ['a.txt', 'b.txt']);

    // projectId is a NUMERIC path segment in the S11 artifacts API.
    expect(createBucket).toHaveBeenCalledWith(1, { name: 'docs' });
    expect(updateBucket).toHaveBeenCalledWith(1, 'docs', { retention_days: 30 });
    expect(updateBucket).toHaveBeenCalledWith(1, 'reports', { is_pinned: true });
    expect(deleteBucket).toHaveBeenCalledWith(1, 'reports');
    expect(deleteObject).toHaveBeenCalledWith(1, 'docs', 'a.txt');
    expect(batchDeleteObjects).toHaveBeenCalledWith(1, 'docs', { keys: ['a.txt', 'b.txt'] });
  });

  it('rejects a non-numeric project id rather than building a bad URL', async () => {
    await expect(createArtifactBucket('not-a-number', 'docs')).rejects.toThrow('not numeric');
  });

  it('skips the bulk delete entirely when no keys are given (an empty array is a 400)', async () => {
    vi.mocked(batchDeleteObjects).mockResolvedValue(okResponse as never);
    await removeArtifacts('1', 'docs', []);
    expect(batchDeleteObjects).not.toHaveBeenCalled();
  });

  it('loads and normalizes regular and shared S3 configurations', async () => {
    vi.mocked(eliteaFetch).mockResolvedValue({
      data: {
        items: [{ id: 1, title: 'Primary' }],
        shared: { items: [{ uid: 'shared', elitea_title: 'Shared', shared: true }] },
      },
    });
    await expect(fetchArtifactStorageConfigurations('p1')).resolves.toEqual([
      { id: '1', title: 'Primary', shared: false },
      { id: 'shared', title: 'Shared', shared: true },
    ]);
    expect(vi.mocked(eliteaFetch).mock.calls[0]?.[0]).toContain('type=s3');
  });
});
