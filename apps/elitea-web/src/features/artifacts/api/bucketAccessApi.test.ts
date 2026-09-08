/**
 * The transport rules that are easiest to get wrong here: the envelope peel,
 * the absent-map normalisation, and refusing a non-numeric project id rather
 * than building a URL with `NaN` in it.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import {
  fetchBucketPermissions,
  removeBucketPermission,
  saveBucketPermissions,
} from './bucketAccessApi';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('fetchBucketPermissions', () => {
  it('reads the rows out of the transport envelope', async () => {
    server.use(http.get(`${BASE}/artifacts/bucket_permissions/7`, () =>
      HttpResponse.json({
        total: 1,
        rows: [{ user_id: 11, name: 'A', email: 'a@b.c', bucket_permissions: { reports: [] } }],
      })));
    const rows = await fetchBucketPermissions('7');
    expect(rows).toHaveLength(1);
    // `[]` survives as `[]`. Collapsing it to "no entry" would turn a block
    // into the default.
    expect(rows[0]?.bucket_permissions).toEqual({ reports: [] });
  });

  it('normalises a row with no map at all to an empty map', async () => {
    server.use(http.get(`${BASE}/artifacts/bucket_permissions/7`, () =>
      HttpResponse.json({ total: 1, rows: [{ user_id: 11 }] })));
    const rows = await fetchBucketPermissions('7');
    expect(rows[0]?.bucket_permissions).toEqual({});
  });

  it('refuses a project id that is not a number', async () => {
    await expect(fetchBucketPermissions('not-a-project')).rejects.toThrow(/not numeric/);
  });
});

describe('saveBucketPermissions', () => {
  it('throws when the server refuses the write', async () => {
    server.use(http.put(`${BASE}/artifacts/bucket_permissions/7`, () =>
      HttpResponse.json({ error: { code: 'Forbidden', message: 'no' } }, { status: 403 })));
    // `eliteaFetch` rejects on a non-2xx answer; the message names the status,
    // which is what the caller logs.
    await expect(saveBucketPermissions('7', 11, {})).rejects.toThrow(/403/);
  });

  it('resolves on a stored write', async () => {
    server.use(http.put(`${BASE}/artifacts/bucket_permissions/7`, () =>
      HttpResponse.json({ user_id: 11, bucket_permissions: {} })));
    await expect(saveBucketPermissions('7', 11, {})).resolves.toBeUndefined();
  });
});

describe('removeBucketPermission', () => {
  it('throws when the exception was not there', async () => {
    server.use(http.delete(`${BASE}/artifacts/bucket_permissions/7`, () =>
      HttpResponse.json({ error: { code: 'NotFound', message: 'no' } }, { status: 404 })));
    await expect(removeBucketPermission('7', 11, 'reports')).rejects.toThrow(/404/);
  });

  it('resolves on a 204', async () => {
    server.use(http.delete(`${BASE}/artifacts/bucket_permissions/7`, () =>
      new HttpResponse(null, { status: 204 })));
    await expect(removeBucketPermission('7', 11, 'reports')).resolves.toBeUndefined();
  });
});
