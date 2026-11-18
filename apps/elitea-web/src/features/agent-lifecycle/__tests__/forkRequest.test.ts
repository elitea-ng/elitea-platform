/** The fork read names the copied version as a number, or not at all (#2032). */
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { fetchForkDocument } from '../api/lifecycleApi';

const BASE = '/api/v2';
let urls: string[] = [];

beforeEach(() => {
  urls = [];
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/export_import/prompt_lib/:projectId/:entityId`, ({ request }) => {
      urls.push(request.url);
      return HttpResponse.json({ ok: true, applications: [] });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('fetchForkDocument', () => {
  it('sends a numeric version id even when handed a numeric string', async () => {
    await fetchForkDocument('2', 7, '11');
    expect(new URL(urls[0] ?? '').searchParams.getAll('follow_version_ids')).toEqual(['11']);
  });

  it('omits follow_version_ids for NaN instead of sending the word NaN', async () => {
    await fetchForkDocument('2', 7, Number.NaN);
    expect(urls[0]).not.toContain('follow_version_ids');
    expect(urls[0]).toContain('fork=true');
  });
});
