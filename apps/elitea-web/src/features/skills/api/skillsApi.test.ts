import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import {
  createSkill,
  createSkillVersion,
  deleteSkill,
  exportSkill,
  fetchSkill,
  fetchSkills,
  generateSkillDraft,
  importSkill,
  setDefaultSkillVersion,
  updateSkill,
} from './skillsApi';

const BASE = '/api/v2';
const skill = {
  id: 'skill-1',
  project_id: 'project-1',
  name: 'Review',
  description: 'Review code',
  type: 'skill',
  is_default: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
};

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('skills API', () => {
  it('lists current item envelopes and legacy row envelopes', async () => {
    let query = '';
    server.use(
      http.get(`${BASE}/elitea_core/skills/prompt_lib/:projectId`, ({ request }) => {
        query = new URL(request.url).search;
        return HttpResponse.json({ items: [skill], total: 21, page: 2, page_size: 10, total_pages: 3 });
      }),
    );
    const page = await fetchSkills('project-1', {
      page: 2,
      pageSize: 10,
      query: 'review',
      sortBy: 'name',
      sortOrder: 'asc',
    });
    expect(query).toContain('query=review');
    expect(page).toMatchObject({ items: [skill], total: 21, page: 2, pageSize: 10, totalPages: 3 });

    server.use(
      http.get(`${BASE}/elitea_core/skills/prompt_lib/:projectId`, () =>
        HttpResponse.json({ rows: [skill] }),
      ),
    );
    await expect(fetchSkills('project-1')).resolves.toMatchObject({
      items: [skill],
      total: 1,
      page: 1,
      pageSize: 20,
      totalPages: 1,
    });
  });

  it('gets a skill with and without a version suffix', async () => {
    const paths: string[] = [];
    server.use(
      http.get(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, ({ request }) => {
        paths.push(new URL(request.url).pathname);
        return HttpResponse.json(skill);
      }),
      http.get(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId/:versionId`, ({ request }) => {
        paths.push(new URL(request.url).pathname);
        return HttpResponse.json(skill);
      }),
    );
    await fetchSkill('project-1', 'skill-1');
    await fetchSkill('project-1', 'skill-1', 'v2');
    expect(paths).toEqual([
      `${BASE}/elitea_core/skill/prompt_lib/project-1/skill-1`,
      `${BASE}/elitea_core/skill/prompt_lib/project-1/skill-1/v2`,
    ]);
  });

  it('creates a base version with trimmed metadata', async () => {
    let body: unknown;
    server.use(
      http.post(`${BASE}/elitea_core/skills/prompt_lib/:projectId`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(skill, { status: 201 });
      }),
    );
    await createSkill('project-1', {
      name: ' Review ',
      description: ' Code ',
      instructions: 'Do it',
      tags: ['quality'],
    });
    expect(body).toEqual({
      name: 'Review',
      description: 'Code',
      versions: [{ name: 'base', instructions: 'Do it', tags: ['quality'] }],
    });
  });

  it('updates skill and version paths', async () => {
    const paths: string[] = [];
    server.use(
      http.put(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, ({ request }) => {
        paths.push(new URL(request.url).pathname);
        return HttpResponse.json(skill);
      }),
      http.put(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId/:versionId`, ({ request }) => {
        paths.push(new URL(request.url).pathname);
        return HttpResponse.json(skill);
      }),
    );
    const input = { name: 'Review', description: 'Code', instructions: 'Run', tags: [] };
    await updateSkill('project-1', 'skill-1', input);
    await updateSkill('project-1', 'skill-1', input, 'v2');
    expect(paths[1]).toContain('/skill-1/v2');
  });

  it('creates versions, sets default, and deletes skill or version', async () => {
    const calls: Array<{ readonly method: string; readonly path: string; readonly body?: unknown }> = [];
    server.use(
      http.post(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, async ({ request }) => {
        calls.push({ method: request.method, path: new URL(request.url).pathname, body: await request.json() });
        return HttpResponse.json(skill);
      }),
      http.patch(`${BASE}/elitea_core/skill_default_version/prompt_lib/:projectId/:skillId`, async ({ request }) => {
        calls.push({ method: request.method, path: new URL(request.url).pathname, body: await request.json() });
        return HttpResponse.json(skill);
      }),
      http.delete(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, ({ request }) => {
        calls.push({ method: request.method, path: new URL(request.url).pathname });
        return new HttpResponse(null, { status: 204 });
      }),
      http.delete(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId/:versionId`, ({ request }) => {
        calls.push({ method: request.method, path: new URL(request.url).pathname });
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await createSkillVersion('project-1', 'skill-1', { name: 'v2', instructions: 'Run', tags: [] });
    await setDefaultSkillVersion('project-1', 'skill-1', 'v2');
    await deleteSkill('project-1', 'skill-1');
    await deleteSkill('project-1', 'skill-1', 'v2');
    expect(calls.map(({ method }) => method)).toEqual(['POST', 'PATCH', 'DELETE', 'DELETE']);
    expect(calls[1]?.body).toEqual({ version_id: 'v2' });
  });

  it('generates a draft and imports multipart Markdown', async () => {
    let importContentType = '';
    server.use(
      http.post(`${BASE}/elitea_core/generate_skill_draft/prompt_lib/:projectId`, () =>
        HttpResponse.json({ name: 'Draft', description: 'D', instructions: 'I', tags: [] }),
      ),
      http.post(`${BASE}/elitea_core/skill_import/prompt_lib/:projectId`, ({ request }) => {
        importContentType = request.headers.get('content-type') ?? '';
        return HttpResponse.json(skill);
      }),
    );
    await expect(generateSkillDraft('project-1', 'Make a reviewer')).resolves.toMatchObject({ name: 'Draft' });
    const file = new File(['---\nname: A\n---'], 'skill.md', { type: 'text/markdown' });
    await importSkill('project-1', file);
    expect(importContentType).toContain('multipart/form-data');
  });

  it('exports text and sends/cancels stateless test requests', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/skill_export/prompt_lib/:projectId/:skillId/:versionId`, () =>
        HttpResponse.text('# Skill'),
      ),
    );
    await expect(exportSkill('project-1', 'skill-1', 'v2')).resolves.toBe('# Skill');
  });
});
