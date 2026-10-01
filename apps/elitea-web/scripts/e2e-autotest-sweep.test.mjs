import { expect, it } from 'vitest';

import { sweepAutotestEntities } from '../e2e/fixtures/api';

it('counts each application class and preserves rows without the autotest prefix', async () => {
  const conversations = new Map([
    ['conversation', { id: 'conversation', name: 'autotest_conversation' }],
    ['conversation-bystander', { id: 'conversation-bystander', name: 'seeded_conversation' }],
  ]);
  const applications = new Map([
    ['agent', { id: 'agent', name: 'autotest_agent', kind: 'classic' }],
    ['pipeline', { id: 'pipeline', name: 'autotest_pipeline', kind: 'pipeline' }],
    ['agent-bystander', { id: 'agent-bystander', name: 'seeded_agent', kind: 'classic' }],
    ['pipeline-bystander', { id: 'pipeline-bystander', name: 'seeded_pipeline', kind: 'pipeline' }],
  ]);
  const filters = [];
  const response = (status, body) => ({
    ok: () => status === 200,
    status: () => status,
    json: () => Promise.resolve(body),
  });
  const request = {
    get(address) {
      const url = new URL(address);
      const isConversation = url.pathname.includes('/conversations/');
      const kind = url.searchParams.get('agents_type');
      if (!isConversation) filters.push(kind);
      const source = isConversation ? conversations : applications;
      const rows = [...source.values()].filter((row) => !kind || row.kind === kind);
      const offset = Number(url.searchParams.get('offset'));
      const limit = Number(url.searchParams.get('limit'));
      return Promise.resolve(response(200, { rows: rows.slice(offset, offset + limit) }));
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
  expect([...conversations.keys()]).toEqual(['conversation-bystander']);
  expect([...applications.keys()]).toEqual(['agent-bystander', 'pipeline-bystander']);
});
