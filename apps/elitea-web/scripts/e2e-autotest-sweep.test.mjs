import { expect, it } from 'vitest';

import { sweepAutotestEntities } from '../e2e/fixtures/api';

it('counts each application class and preserves rows without the autotest prefix', async () => {
  const conversations = new Map([
    ['conversation', { id: 'conversation', name: 'autotest_conversation' }],
    ['conversation-bystander', { id: 'conversation-bystander', name: 'seeded_conversation' }],
  ]);
  const applications = new Map([
    ['agent', { id: 'agent', name: 'autotest_agent', kind: 'classic', versions: [{ id: 'v-live', status: 'published' }, { id: 'v-draft', status: 'draft' }] }],
    ['pipeline', { id: 'pipeline', name: 'autotest_pipeline', kind: 'pipeline' }],
    ['agent-bystander', { id: 'agent-bystander', name: 'seeded_agent', kind: 'classic' }],
    ['pipeline-bystander', { id: 'pipeline-bystander', name: 'seeded_pipeline', kind: 'pipeline' }],
  ]);
  const filters = [];
  const unpublished = [];
  const response = (status, body) => ({
    ok: () => status === 200,
    status: () => status,
    json: () => Promise.resolve(body),
  });
  const request = {
    get(address) {
      const url = new URL(address);
      const detail = url.pathname.match(/\/application\/prompt_lib\/[^/]+\/([^/]+)$/);
      if (detail) {
        const row = applications.get(detail[1]);
        return Promise.resolve(row ? response(200, { versions: row.versions ?? [] }) : response(404, {}));
      }
      const isConversation = url.pathname.includes('/conversations/');
      const kind = url.searchParams.get('agents_type');
      if (!isConversation) filters.push(kind);
      const source = isConversation ? conversations : applications;
      const rows = [...source.values()].filter((row) => !kind || row.kind === kind);
      const offset = Number(url.searchParams.get('offset'));
      const limit = Number(url.searchParams.get('limit'));
      return Promise.resolve(response(200, { rows: rows.slice(offset, offset + limit) }));
    },
    post(address) {
      const versionId = new URL(address).pathname.split('/').at(-1);
      unpublished.push(versionId);
      for (const row of applications.values()) {
        for (const version of row.versions ?? []) if (version.id === versionId) version.status = 'draft';
      }
      return Promise.resolve(response(200, {}));
    },
    delete(address) {
      const url = new URL(address);
      const source = url.pathname.includes('/conversation/') ? conversations : applications;
      const removed = source.delete(url.pathname.split('/').at(-1));
      return Promise.resolve(response(removed ? 200 : 404, {}));
    },
  };

  await expect(sweepAutotestEntities(request)).resolves.toEqual({
    conversations: 1,
    agents: 1,
    pipelines: 1,
    failures: [],
  });
  expect(filters).toEqual(['classic', 'pipeline']);
  // A published version refuses the delete: the sweep withdraws it first, and only it.
  expect(unpublished).toEqual(['v-live']);
  expect([...conversations.keys()]).toEqual(['conversation-bystander']);
  expect([...applications.keys()]).toEqual(['agent-bystander', 'pipeline-bystander']);
});
