import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { forkEntityWithNested } from '../api/lifecycleApi';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

const parentDocument = {
  ok: true,
  applications: [{ id: '10', name: 'Parent', owner_id: '2', versions: [] }],
  toolkits: [
    { name: 'Repo', type: 'github', import_uuid: 'tool-1', settings: { repo: 'a/b' } },
    { name: 'Helper', type: 'application', import_uuid: 'tool-2', settings: { application_id: 20, application_version_id: 21 } },
  ],
};

describe('forkEntityWithNested (#2109)', () => {
  it('sends the toolkits, forks the nested agent from the source project and links it to the new parent version', async () => {
    const forkBodies: { project: string; body: Record<string, unknown> }[] = [];
    let readUrl = '';
    let relation: { path: string; body: unknown } | undefined;
    server.use(
      http.get(`${BASE}/elitea_core/export_import/prompt_lib/:projectId/:entityId`, ({ request, params }) => {
        readUrl = `${String(params['projectId'])}/${String(params['entityId'])}?${new URL(request.url).search}`;
        return HttpResponse.json({ ok: true, applications: [{ id: '20', name: 'Helper', versions: [] }], toolkits: [] });
      }),
      http.post(`${BASE}/elitea_core/fork/prompt_lib/:projectId`, async ({ request, params }) => {
        const body = (await request.json()) as Record<string, unknown>;
        forkBodies.push({ project: String(params['projectId']), body });
        const nested = forkBodies.length > 1;
        return HttpResponse.json(
          { result: { agents: [{ id: nested ? '201' : '101', version_id: nested ? '202' : '102' }] }, errors: {} },
          { status: 201 },
        );
      }),
      http.patch(`${BASE}/elitea_core/application_relation/prompt_lib/:projectId/:appId/:versionId`, async ({ request, params }) => {
        relation = { path: `${String(params['projectId'])}/${String(params['appId'])}/${String(params['versionId'])}`, body: await request.json() };
        return HttpResponse.json({ application_id: 101, version_id: 102, has_relation: true });
      }),
    );

    const outcome = await forkEntityWithNested('2', '9', parentDocument);

    // The parent goes first and carries its toolkits; the nested agent follows into the SAME target project.
    expect(forkBodies.map((call) => call.project)).toEqual(['9', '9']);
    expect(forkBodies[0]?.body['toolkits']).toEqual(parentDocument.toolkits);
    // The nested agent is read from the source project, at the version the parent used.
    expect(readUrl).toContain('2/20');
    expect(readUrl).toContain('follow_version_ids');
    expect(relation).toEqual({ path: '9/201/202', body: { application_id: 101, version_id: 102, has_relation: true } });
    expect(outcome.status).toBe(201);
    expect((outcome.result as { agents: { id: string }[] }).agents.map((agent) => agent.id)).toEqual(['101', '201']);
  });

  it('reports a nested agent that cannot be forked and keeps the parent', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/export_import/prompt_lib/:projectId/:entityId`, () => HttpResponse.json({ error: 'gone' }, { status: 404 })),
      http.post(`${BASE}/elitea_core/fork/prompt_lib/:projectId`, () =>
        HttpResponse.json({ result: { agents: [{ id: '101', version_id: '102' }] }, errors: {} }, { status: 201 }),
      ),
    );

    const outcome = await forkEntityWithNested('2', '9', parentDocument);

    expect(outcome.status).toBe(207);
    expect((outcome.result as { agents: unknown[] }).agents).toHaveLength(1);
    expect(JSON.stringify(outcome.errors)).toContain('Nested agent 20 was not forked');
  });

  it('creates a shared sub-agent once and links it under every agent that nests it', async () => {
    const links: string[] = [];
    let forks = 0;
    const standIn = (id: number) => ({ name: `N${String(id)}`, type: 'application', settings: { application_id: id } });
    server.use(
      http.get(`${BASE}/elitea_core/export_import/prompt_lib/:projectId/:entityId`, ({ params }) => {
        const id = Number(params['entityId']);
        const nests = id === 20 || id === 30 ? [standIn(40)] : [];
        return HttpResponse.json({ ok: true, applications: [{ id: String(id), name: `A${String(id)}`, versions: [] }], toolkits: nests });
      }),
      http.post(`${BASE}/elitea_core/fork/prompt_lib/:projectId`, () => {
        forks += 1;
        return HttpResponse.json({ result: { agents: [{ id: String(100 + forks), version_id: String(200 + forks) }] }, errors: {} }, { status: 201 });
      }),
      http.patch(`${BASE}/elitea_core/application_relation/prompt_lib/:projectId/:appId/:versionId`, async ({ request, params }) => {
        const body = (await request.json()) as { application_id: number };
        links.push(`${String(params['appId'])}->${String(body.application_id)}`);
        return HttpResponse.json({ application_id: 1, version_id: 1, has_relation: true });
      }),
    );
    const diamond = { ...parentDocument, toolkits: [standIn(20), standIn(30)] };

    await forkEntityWithNested('2', '9', diamond);

    // 10 (parent), 20, 30 and 40 once, not twice; 40 (the third fork, id 103) is linked under both.
    expect(forks).toBe(4);
    expect(links.filter((link) => link.startsWith('103->'))).toHaveLength(2);
  });

  it('does not loop when a nested agent names an agent already being forked', async () => {
    let forks = 0;
    server.use(
      http.get(`${BASE}/elitea_core/export_import/prompt_lib/:projectId/:entityId`, () =>
        HttpResponse.json({
          ok: true,
          applications: [{ id: '20', name: 'Helper', versions: [] }],
          toolkits: [{ name: 'Back', type: 'application', settings: { application_id: 20 } }],
        }),
      ),
      http.post(`${BASE}/elitea_core/fork/prompt_lib/:projectId`, () => {
        forks += 1;
        return HttpResponse.json({ result: { agents: [{ id: String(100 + forks), version_id: String(200 + forks) }] }, errors: {} }, { status: 201 });
      }),
      http.patch(`${BASE}/elitea_core/application_relation/prompt_lib/:projectId/:appId/:versionId`, () =>
        HttpResponse.json({ application_id: 101, version_id: 201, has_relation: true }),
      ),
    );

    await forkEntityWithNested('2', '9', parentDocument);

    expect(forks).toBe(2);
  });
});
