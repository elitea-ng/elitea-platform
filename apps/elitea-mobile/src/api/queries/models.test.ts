import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { fetchModels, startConversationWithModel } from '@/api/queries/models';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';

function routedFetch(routes: Record<string, (body: string) => { status?: number; body: unknown }>) {
  const calls: { method: string; url: string; body: string }[] = [];
  const fetch: BaseFetch = async (input) => {
    const body = input.method === 'GET' ? '' : await input.clone().text();
    calls.push({ method: input.method, url: input.url, body });
    const path = input.url.slice(ORIGIN.length).split('?')[0];
    const handler = routes[`${input.method} ${path}`];
    if (!handler) return new Response('{"error":"not found"}', { status: 404 });
    const out = handler(body);
    return new Response(JSON.stringify(out.body), {
      status: out.status ?? 200,
      headers: { 'Content-Type': 'application/json' },
    });
  };
  return { fetch, calls };
}

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 90107 });
  setCredential({ kind: 'pat', token: 't' });
});

// DEF-3 (Agent Zefir E2E): mobile could start conversations with agents only.
describe('models', () => {
  it("lists the project's chat models with the default first, as the live server answers", async () => {
    const { fetch, calls } = routedFetch({
      'GET /api/v2/configurations/models/90107': () => ({
        body: {
          total: 4,
          items: [
            { name: 'shared/big', display_name: 'Big', project_id: 1, shared: true, default: false },
            { name: 'vllm/E2E-MOCK-MODEL', display_name: 'vllm/E2E-MOCK-MODEL', project_id: 90107, default: true },
            { name: 'embed', project_id: 90107, section: 'embedding_model' },
            { name: '', project_id: 90107 },
          ],
          default_model_name: 'vllm/E2E-MOCK-MODEL',
          default_model_project_id: 90107,
          // The live server sends "" for unset tier ids (schema says integer).
          low_tier_default_model_project_id: '',
        },
      }),
    });
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    const catalogue = await fetchModels(90107, client);
    expect(catalogue.models).toEqual([
      { name: 'vllm/E2E-MOCK-MODEL', displayName: 'vllm/E2E-MOCK-MODEL', projectId: 90107, isDefault: true },
      { name: 'shared/big', displayName: 'Big', projectId: 1, isDefault: false },
    ]);
    expect(catalogue.defaultModel).toEqual(catalogue.models[0]);
    expect(calls[0].url).toContain('section=llm');
    expect(calls[0].url).toContain('include_shared=true');
  });

  it('falls back to the first model when the server names no default', async () => {
    const { fetch } = routedFetch({
      'GET /api/v2/configurations/models/90107': () => ({ body: { total: 1, items: [{ name: 'only', project_id: 90107 }] } }),
    });
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    expect((await fetchModels(90107, client)).defaultModel).toMatchObject({ name: 'only' });
  });

  it('creates a private conversation whose model participant carries the model', async () => {
    const { fetch, calls } = routedFetch({
      'POST /api/v2/elitea_core/conversations/prompt_lib/90107': () => ({ status: 201, body: { id: '5', uuid: 'u-5' } }),
    });
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(
      startConversationWithModel(90107, { name: 'shared/big', displayName: 'Big', projectId: 1, isDefault: false }, client),
    ).resolves.toEqual({ id: 5, uuid: 'u-5' });
    expect(JSON.parse(calls[0].body)).toEqual({
      name: 'Big',
      is_private: true,
      participants: [
        {
          entity_name: 'dummy',
          entity_meta: {},
          entity_settings: { llm_settings: { model_name: 'shared/big', model_project_id: 1, stream: true } },
        },
      ],
    });
  });
});
