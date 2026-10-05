import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { StartConversationError, fetchAgents, startConversationWithAgent } from '@/api/queries/agents';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';

function routedFetch(routes: Record<string, (body: string) => unknown>) {
  const calls: { method: string; url: string; body: string }[] = [];
  const fetch: BaseFetch = async (input) => {
    const body = input.method === 'GET' ? '' : await input.clone().text();
    calls.push({ method: input.method, url: input.url, body });
    const path = input.url.slice(ORIGIN.length).split('?')[0];
    const handler = routes[`${input.method} ${path}`];
    if (!handler) return new Response('{"error":"not found"}', { status: 404 });
    const out = handler(body);
    return new Response(JSON.stringify(out), {
      status: input.method === 'POST' ? 201 : 200,
      headers: { 'Content-Type': 'application/json' },
    });
  };
  return { fetch, calls };
}

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 't' });
});

describe('agents', () => {
  it('lists agents, normalising string ids and dropping malformed rows', async () => {
    const { fetch, calls } = routedFetch({
      'GET /api/v2/elitea_core/applications/prompt_lib/7': () => ({
        total: 3,
        rows: [
          { id: '5', name: ' Helper ', description: 'Helps' },
          { id: 9, name: '' },
          { id: 'abc', name: 'bad' },
        ],
      }),
    });
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(fetchAgents(7, ' sea ', client)).resolves.toEqual([
      { id: 5, name: 'Helper', description: 'Helps' },
      { id: 9, name: 'Agent 9', description: '' },
    ]);
    expect(calls[0].url).toContain('query=sea');
    expect(calls[0].url).toContain('limit=50');
  });

  it('creates a private conversation pinned to the agent current version', async () => {
    const { fetch, calls } = routedFetch({
      'GET /api/v2/elitea_core/application/prompt_lib/7/5': () => ({ id: '5', version_details: { id: '31' } }),
      'POST /api/v2/elitea_core/conversations/prompt_lib/7': () => ({ id: '88', uuid: 'u-88', name: 'Helper' }),
    });
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(startConversationWithAgent(7, { id: 5, name: 'Helper', description: '' }, client)).resolves.toEqual({
      id: 88,
      uuid: 'u-88',
    });
    expect(JSON.parse(calls[1].body)).toEqual({
      name: 'Helper',
      is_private: true,
      participants: [
        { entity_name: 'application', entity_meta: { id: 5, project_id: 7 }, entity_settings: { version_id: 31 } },
      ],
    });
  });

  it('refuses an agent without a version and creates nothing', async () => {
    const { fetch, calls } = routedFetch({
      'GET /api/v2/elitea_core/application/prompt_lib/7/5': () => ({ id: '5' }),
    });
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    await expect(startConversationWithAgent(7, { id: 5, name: 'H', description: '' }, client)).rejects.toBeInstanceOf(
      StartConversationError,
    );
    expect(calls.filter((c) => c.method === 'POST')).toHaveLength(0);
  });
});
