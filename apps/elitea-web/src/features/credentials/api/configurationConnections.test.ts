/**
 * configurationConnections.test.ts — contract coverage for API-154..160
 * (unit A7), split out of `configurations.test.ts` alongside
 * `configurationConnections.ts` itself (see that file's doc comment for
 * why). Same MSW-per-test pattern as the sibling test file.
 */
import { afterEach, describe, expect, it } from 'vitest';
import { http, HttpResponse } from 'msw';

import { server } from '../../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import {
  batchTestConfigurationConnection,
  getTtsVoices,
  listCredentialTypes,
  listModels,
  setProjectDefaultModel,
  testConfigurationConnection,
  toggleConfigurationSharing,
} from './configurationConnections';

const BASE = '/api/v2';

afterEach(() => {
  resetGeneratedClient();
});

function setup(): void {
  configureGeneratedClient({ baseUrl: BASE });
}

describe('API-154 testConfigurationConnection', () => {
  it('POSTs to /check_connection/{projectId}/{configType}', async () => {
    setup();
    let url = '';
    server.use(
      http.post(`${BASE}/configurations/check_connection/7/openai`, ({ request }) => {
        url = request.url;
        return HttpResponse.json({});
      }),
    );
    await testConfigurationConnection(7, 'openai', { api_key: 'x' });
    expect(url).toContain('/configurations/check_connection/7/openai');
  });

  it('surfaces an error body without throwing when the endpoint reports one', async () => {
    setup();
    server.use(http.post(`${BASE}/configurations/check_connection/7/openai`, () => HttpResponse.json({ error: 'bad key' })));
    await expect(testConfigurationConnection(7, 'openai', {})).resolves.toEqual({ error: 'bad key' });
  });
});

describe('API-155 batchTestConfigurationConnection', () => {
  it('POSTs the items array directly as the body', async () => {
    setup();
    let body: unknown;
    server.use(
      http.post(`${BASE}/configurations/check_connections/7`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json([{ id: '1', success: true }]);
      }),
    );
    const items = [{ id: '1', type: 'openai', data: {} }];
    const result = await batchTestConfigurationConnection(7, items);
    expect(body).toEqual(items);
    expect(result).toEqual([{ id: '1', success: true }]);
  });
});

describe('API-156 toggleConfigurationSharing', () => {
  it('PUTs { shared } only', async () => {
    setup();
    let body: unknown;
    server.use(
      http.put(`${BASE}/configurations/configuration/7/abc`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ uid: 'abc', shared: true });
      }),
    );
    await toggleConfigurationSharing(7, 'abc', true);
    expect(body).toEqual({ shared: true });
  });
});

describe('API-157 listModels', () => {
  it('synthesizes id = project_id_name for every row', async () => {
    setup();
    server.use(
      http.get(`${BASE}/configurations/models/7`, () =>
        HttpResponse.json({ items: [{ name: 'gpt-4', project_id: 7 }], total: 1 }),
      ),
    );
    const result = await listModels(7);
    expect(result.items).toEqual([{ name: 'gpt-4', project_id: 7, id: '7_gpt-4' }]);
  });

  it('passes include_shared and section through as query params', async () => {
    setup();
    let url = '';
    server.use(
      http.get(`${BASE}/configurations/models/7`, ({ request }) => {
        url = request.url;
        return HttpResponse.json({ items: [], total: 0 });
      }),
    );
    await listModels(7, { include_shared: true, section: 'embedding' });
    expect(url).toContain('include_shared=true');
    expect(url).toContain('section=embedding');
  });
});

describe('API-158 listCredentialTypes', () => {
  it('GETs /types/{projectId}', async () => {
    setup();
    server.use(http.get(`${BASE}/configurations/types/7`, () => HttpResponse.json({ rows: ['openai', 'azure'] })));
    await expect(listCredentialTypes(7)).resolves.toEqual({ rows: ['openai', 'azure'] });
  });
});

describe('API-159 setProjectDefaultModel', () => {
  it('POSTs name/target_project_id/section, defaulting section to llm', async () => {
    setup();
    let body: unknown;
    server.use(
      http.post(`${BASE}/configurations/models/7`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ items: [{ name: 'gpt-4', project_id: 7 }], total: 1 });
      }),
    );
    const result = await setProjectDefaultModel({ projectId: 7, name: 'gpt-4', target_project_id: 9 });
    expect(body).toEqual({ name: 'gpt-4', target_project_id: 9, section: 'llm' });
    expect(result.items[0]?.id).toBe('7_gpt-4');
  });
});

describe('API-160 getTtsVoices', () => {
  it('GETs /tts_voices/{projectId} with model_name as a query param', async () => {
    setup();
    let url = '';
    server.use(
      http.get(`${BASE}/configurations/tts_voices/7`, ({ request }) => {
        url = request.url;
        return HttpResponse.json([{ voice: 'alloy' }]);
      }),
    );
    await getTtsVoices(7, 'gpt-4o-mini');
    expect(url).toContain('model_name=gpt-4o-mini');
  });

  it('omits model_name when undefined', async () => {
    setup();
    let url = '';
    server.use(
      http.get(`${BASE}/configurations/tts_voices/7`, ({ request }) => {
        url = request.url;
        return HttpResponse.json([]);
      }),
    );
    await getTtsVoices(7, undefined);
    expect(url).not.toContain('model_name');
  });
});
