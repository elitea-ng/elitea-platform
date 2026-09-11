import { afterEach, beforeEach, expect, it } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '@/test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { attachMessageTraces } from './messageTraces';

const path = '/api/v2/elitea_core/message_traces/prompt_lib/2/543';
beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

it('preserves the message envelope and scopes paginated light traces to loaded messages', async () => {
  const offsets: string[] = [];
  server.use(http.get(path, ({ request }) => {
    const query = new URL(request.url).searchParams;
    expect(query.get('message_group_ids')).toBe('5820');
    offsets.push(query.get('offset') ?? '');
    const rows = query.get('offset') === '0'
      ? Array.from({ length: 500 }, (_, id) => ({ id, message_group_id: 5820, kind: 'tool_call', tool_name: 'get_issues', is_error: false }))
      : [{ id: 501, message_group_id: 5820, kind: 'tool_call', is_error: false }, { id: 502, message_group_id: 999, kind: 'tool_call', is_error: false }];
    return HttpResponse.json({ rows });
  }));
  const response = await attachMessageTraces({ items: [{ id: '5820', content: 'answer' }], total: 1 }, 2, 543) as { items: { content: string; persisted_trace: { steps: unknown[] } }[]; total: number };
  expect(offsets).toEqual(['0', '500']);
  expect(response.total).toBe(1);
  expect(response.items[0]?.content).toBe('answer');
  expect(response.items[0]?.persisted_trace.steps).toHaveLength(501);
});

it('keeps messages readable and exposes a retryable trace read failure', async () => {
  server.use(http.get(path, () => HttpResponse.json({ error: 'unavailable' }, { status: 500 })));
  const response = await attachMessageTraces([{ id: 5820, content: 'retained' }], 2, 543) as { content: string; persisted_trace: { failed: boolean } }[];
  expect(response[0]?.content).toBe('retained');
  expect(response[0]?.persisted_trace.failed).toBe(true);
});
