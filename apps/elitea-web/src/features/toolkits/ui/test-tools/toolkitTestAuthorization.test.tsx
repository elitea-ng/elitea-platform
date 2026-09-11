import { act, renderHook } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { beforeEach, afterEach, expect, it } from 'vitest';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { useToolkitTestToolRun } from './useToolkitTestToolRun';
import { readToolkitTestAuthorization } from '../../api/toolkitTestAuthorization';

const challenge = { toolkit_id: 9, toolkit_name: 'Docs', toolkit_type: 'mcp', server_url: 'https://mcp.example/tools', resource_metadata: {} };
const path = '*/api/v2/elitea_core/test_tool/prompt_lib/:project/:toolkit';
beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

it('rejects missing, malformed and foreign toolkit challenges', () => {
  for (const raw of [undefined, {}, { ...challenge, toolkit_id: 8 }, { ...challenge, server_url: 'javascript:bad' }, { ...challenge, resource_metadata: null }]) {
    expect(readToolkitTestAuthorization(raw, '9')).toBeUndefined();
  }
  expect(readToolkitTestAuthorization(challenge, '9')).toEqual(challenge);
  expect(readToolkitTestAuthorization({ ...challenge, toolkit_id: '9' }, '9')).toEqual(challenge);
});

it('retries the frozen operation and arguments with only the server reference', async () => {
  const bodies: Record<string, unknown>[] = [];
  server.use(http.post(path, async ({ request }) => {
    bodies.push(await request.json() as Record<string, unknown>);
    return bodies.length === 1 ? HttpResponse.json({ reason: 'authorization_required', authorization_required: challenge, task_id: 'task-1' }, { status: 409 }) : HttpResponse.json({ ok: true, result: 'done' });
  }));
  const { result } = renderHook(() => useToolkitTestToolRun({ projectId: '7', toolkitId: '9' }));
  const args = { query: 'original' };
  await act(async () => result.current.run('search', args));
  expect(result.current.outcome?.kind).toBe('authorizationRequired');
  args.query = 'edited';
  await act(async () => result.current.authorize('A'.repeat(43)));
  expect(bodies).toEqual([{ request_id: expect.any(String) as unknown, tool_name: 'search', tool_params: { query: 'original' } }, { request_id: expect.any(String) as unknown, tool_name: 'search', tool_params: { query: 'original' }, mcp_authorization_reference: 'A'.repeat(43) }]);
  expect(bodies[1]?.['request_id']).not.toBe(bodies[0]?.['request_id']);
  await act(async () => result.current.authorize('A'.repeat(43)));
  expect(bodies).toHaveLength(2);
});

it('skip is local and invalidates a late authorization completion', async () => {
  let calls = 0;
  server.use(http.post(path, () => { calls++; return HttpResponse.json({ reason: 'authorization_required', authorization_required: challenge, task_id: 'task-1' }, { status: 409 }); }));
  const { result } = renderHook(() => useToolkitTestToolRun({ projectId: '7', toolkitId: '9' }));
  await act(async () => result.current.run('search', {}));
  act(() => result.current.skip());
  await act(async () => result.current.authorize('A'.repeat(43)));
  expect(calls).toBe(1);
  expect(result.current.outcome).toEqual({ kind: 'skipped' });
});

it('project changes and missing references cannot retry an old challenge', async () => {
  let calls = 0;
  server.use(http.post(path, () => { calls++; return HttpResponse.json({ reason: 'authorization_required', authorization_required: challenge, task_id: 'task-1' }, { status: 409 }); }));
  const { result, rerender } = renderHook(({ projectId }) => useToolkitTestToolRun({ projectId, toolkitId: '9' }), { initialProps: { projectId: '7' } });
  await act(async () => result.current.run('search', {}));
  await act(async () => result.current.authorize(''));
  expect(calls).toBe(1);
  rerender({ projectId: '8' });
  await act(async () => result.current.authorize('A'.repeat(43)));
  expect(calls).toBe(1);
});
