import { TRACE_PAGE, fetchMessageTraceDetail, fetchMessageTraces } from '@/api/queries/messageTraces';
import { MAX_TOOL_TEXT } from '@/features/chat/toolActivity';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

// listMessageTraces rows in the MessageTraceStep shape (client contract 1.1).
const toolRow = (id: number, group: number, extra: Record<string, unknown> = {}) => ({
  id,
  message_group_id: group,
  kind: 'tool_call',
  step_type: 'tool',
  tool_name: 'list_issues',
  is_error: false,
  started_at: '2026-10-05T10:00:00Z',
  finished_at: '2026-10-05T10:00:01.5Z',
  parent_agent_name: null,
  parent_agent_call_id: null,
  model_name: null,
  finish_reason: 'stop',
  attrs: {
    metadata: { display_name: 'List issues', toolkit_name: 'github', toolkit_type: 'github' },
    tool_meta: { name: 'list_issues' },
  },
  ...extra,
});

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 't' });
});

describe('fetchMessageTraces', () => {
  it('asks for the loaded groups only and groups tool calls by message group', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/message_traces/prompt_lib/7/42', {
      status: 200,
      body: {
        total: null,
        rows: [
          toolRow(3, 11, { started_at: '2026-10-05T10:00:05Z', finished_at: null }),
          toolRow(1, 11),
          { ...toolRow(2, 11), kind: 'thinking_step', tool_name: null },
          toolRow(4, 12, { is_error: true, parent_agent_name: 'researcher', attrs: { tool_output_chunks: { complete: false, received: 2, total: 4 } } }),
        ],
      },
    });
    const traces = await fetchMessageTraces(7, 42, [11, 12, 11], server.client);
    const query = server.requests[0].url.searchParams;
    expect(query.get('kind')).toBe('tool_call');
    expect(query.get('message_group_ids')).toBe('11,12');
    expect(traces?.get(11)?.map((s) => [s.traceId, s.status, s.displayName, s.toolkit])).toEqual([
      [1, 'done', 'List issues', 'github'],
      [3, 'running', 'List issues', 'github'],
    ]);
    expect(traces?.get(12)?.[0]).toMatchObject({ status: 'error', parentAgent: 'researcher', partial: true, inputs: null });
  });

  it('reads every page: the newest answers (last in start order) keep their tool calls', async () => {
    const server = fakeServer();
    const full = Array.from({ length: TRACE_PAGE }, (_, i) => toolRow(i + 1, 11));
    server.on(
      'GET',
      '/elitea_core/message_traces/prompt_lib/7/42',
      { status: 200, body: { total: null, rows: full } },
      { status: 200, body: { total: null, rows: [toolRow(TRACE_PAGE + 1, 12), toolRow(TRACE_PAGE + 2, 12)] } },
    );
    const traces = await fetchMessageTraces(7, 42, [11, 12], server.client);
    expect(server.requests.map((r) => r.url.searchParams.get('offset'))).toEqual(['0', String(TRACE_PAGE)]);
    expect(traces?.get(11)).toHaveLength(TRACE_PAGE);
    expect(traces?.get(12)?.map((s) => s.traceId)).toEqual([TRACE_PAGE + 1, TRACE_PAGE + 2]);
  });

  it('reads "not available" from an older server or a refusal, and asks nothing for no groups', async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/message_traces/prompt_lib/7/42', { status: 404, body: { error: 'not found' } });
    await expect(fetchMessageTraces(7, 42, [11], server.client)).resolves.toBeNull();
    server.on('GET', '/elitea_core/message_traces/prompt_lib/7/42', { status: 403, body: { error: 'forbidden' } });
    await expect(fetchMessageTraces(7, 42, [11], server.client)).resolves.toBeNull();
    await expect(fetchMessageTraces(7, 42, [], server.client)).resolves.toEqual(new Map());
    expect(server.requests).toHaveLength(2);
  });
});

describe('fetchMessageTraceDetail', () => {
  it("fetches one step's I/O with its group id, capped like a live step", async () => {
    const server = fakeServer();
    server.on('GET', '/elitea_core/message_trace/prompt_lib/7/1', {
      status: 200,
      body: { ...toolRow(1, 11), tool_inputs: { state: 'open' }, tool_output: 'y'.repeat(MAX_TOOL_TEXT + 10), text: null, thinking: null },
    });
    const detail = await fetchMessageTraceDetail(7, 1, 11, server.client);
    expect(server.requests[0].url.searchParams.get('message_group_id')).toBe('11');
    expect(detail).toMatchObject({ inputs: '{\n  "state": "open"\n}', inputsTruncated: false, outputTruncated: true });
    expect(detail?.output).toHaveLength(MAX_TOOL_TEXT);
  });
});
