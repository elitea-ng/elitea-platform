import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { readToolkitToolResult, testToolkitTool } from './toolkitTestRun';

const PATH = '*/api/v2/elitea_core/test_tool/prompt_lib/proj-1/tk-1';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('testToolkitTool', () => {
  it('POSTs tool_name/tool_params to /test_tool/prompt_lib/{projectId}/{toolId}', async () => {
    let body: unknown;
    server.use(
      http.post(PATH, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ ok: true, result: {}, tool_name: 'search_index' });
      }),
    );

    await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search_index', toolParams: { query: 'x' } });

    expect(body).toEqual({ request_id: expect.any(String) as unknown, tool_name: 'search_index', tool_params: { query: 'x' } });
  });

  it('OK (200 ok:true): resolves to the result and truncated:false', async () => {
    server.use(http.post(PATH, () => HttpResponse.json({ ok: true, result: { hits: 3 }, tool_name: 'search_index' })));

    const outcome = await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search_index', toolParams: {} });

    expect(outcome).toEqual({ kind: 'ok', result: { hits: 3 }, truncated: false });
  });

  it('OK, truncated (200 ok:true, truncated:true): resolves with no result payload', async () => {
    server.use(http.post(PATH, () => HttpResponse.json({ ok: true, truncated: true, tool_name: 'search_index' })));

    const outcome = await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search_index', toolParams: {} });

    expect(outcome).toEqual({ kind: 'ok', result: undefined, truncated: true });
  });

  it('TOOL_ERROR (200 ok:false): resolves to the tool\'s own sentence', async () => {
    server.use(http.post(PATH, () => HttpResponse.json({ ok: false, error: 'the API key was rejected', tool_name: 'search_index' })));

    const outcome = await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search_index', toolParams: {} });

    expect(outcome).toEqual({ kind: 'toolError', message: 'the API key was rejected' });
  });

  it('UNSUPPORTED_TOOLKIT (422, reason:unsupported_toolkit): resolves to a named refusal', async () => {
    server.use(
      http.post(PATH, () => HttpResponse.json({ ok: false, reason: 'unsupported_toolkit', error: 'this image cannot build "custom"' }, { status: 422 })),
    );

    const outcome = await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'custom', toolParams: {} });

    expect(outcome).toEqual({ kind: 'unsupportedToolkit', message: 'this image cannot build "custom"' });
  });

  it('UNKNOWN_TOOL (422, reason:unknown_tool): resolves to a named refusal', async () => {
    server.use(http.post(PATH, () => HttpResponse.json({ ok: false, reason: 'unknown_tool', error: 'no tool named "bogus"' }, { status: 422 })));

    const outcome = await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'bogus', toolParams: {} });

    expect(outcome).toEqual({ kind: 'unknownTool', message: 'no tool named "bogus"' });
  });

  it('bounded wait passed (504): resolves with the task id so the caller can poll', async () => {
    server.use(
      http.post(PATH, () =>
        HttpResponse.json({ ok: false, task_id: 'job-42', reason: 'timeout', error: 'the tool did not finish within the bounded wait' }, { status: 504 }),
      ),
    );

    const outcome = await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search_index', toolParams: {} });

    expect(outcome).toEqual({ kind: 'timeout', taskId: 'job-42', message: 'the tool did not finish within the bounded wait' });
  });

  it('an unrecognised server failure (500) resolves to the generic failure kind, never rejects', async () => {
    server.use(http.post(PATH, () => HttpResponse.json({ ok: false, error: 'boom' }, { status: 500 })));

    const outcome = await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search_index', toolParams: {} });

    expect(outcome).toEqual({ kind: 'failure', message: 'boom' });
  });

  it('a lost response retains the pre-submission identity for a read', async () => {
    // No `server.use(...)` override — the request itself never reaches a
    // handler in this test's own describe scope, so the shared `server`'s
    // `onUnhandledRequest: 'error'` (see `src/test/setup.ts`) throws inside
    // the fetch. `testToolkitTool` must still resolve, never reject.
    server.use(http.post(PATH, () => HttpResponse.error()));

    const outcome = await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search_index', toolParams: {} });

    expect(outcome).toMatchObject({ kind: 'timeout', lookup: 'request', taskId: expect.any(String) });
  });
});

it('preserves selected model controls and excludes unrelated configuration fields', async () => {
  let body: unknown;
  server.use(http.post(PATH, async ({ request }) => { body = await request.json(); return HttpResponse.json({ ok: true, result: {} }); }));
  const llmSettings = { temperature: 0.4, max_tokens: -1, reasoning_effort: 'medium', api_key: 'must-not-cross' };
  await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search', toolParams: {}, llmModel: 'chosen-model', llmSettings });
  expect(body).toEqual({ request_id: expect.any(String) as unknown, tool_name: 'search', tool_params: {}, llm_model: 'chosen-model', llm_settings: { temperature: 0.4, max_tokens: -1, reasoning_effort: 'medium' } });
});


it('gives separate actions new identities and preserves an explicit retry identity', async () => {
  const identities: string[] = [];
  server.use(http.post(PATH, async ({ request }) => {
    const body = await request.json() as { request_id: string };
    identities.push(body.request_id);
    return HttpResponse.json({ ok: true, result: {} });
  }));
  const params = { projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search', toolParams: {} };
  await testToolkitTool(params);
  await testToolkitTool(params);
  await testToolkitTool({ ...params, requestId: 'retry-request' });
  await testToolkitTool({ ...params, requestId: 'retry-request' });
  expect(identities[0]).toEqual(expect.any(String));
  expect(identities[0]).not.toBe(identities[1]);
  expect(identities.slice(2)).toEqual(['retry-request', 'retry-request']);
});


describe('readToolkitToolResult', () => {
  it('observes pending and then the original result with GET requests only', async () => {
    let reads = 0;
    server.use(http.get(`${PATH}/execution-1`, () => {
      reads += 1;
      return reads === 1 ? HttpResponse.json({ pending: true, task_id: 'execution-1' }, { status: 202 }) : HttpResponse.json({ ok: true, result: { answer: 42 } });
    }));
    const request = { projectId: 'proj-1', toolkitId: 'tk-1', taskId: 'execution-1' };
    expect(await readToolkitToolResult(request)).toMatchObject({ kind: 'timeout', taskId: 'execution-1' });
    expect(await readToolkitToolResult(request)).toEqual({ kind: 'ok', result: { answer: 42 }, truncated: false });
    expect(reads).toBe(2);
  });
  it('keeps observing through Main unavailability but exposes terminal runtime failure', async () => {
    const request = { projectId: 'proj-1', toolkitId: 'tk-1', taskId: 'execution-1' };
    server.use(http.get(`${PATH}/execution-1`, () => HttpResponse.json({}, { status: 503 })));
    expect(await readToolkitToolResult(request)).toMatchObject({ kind: 'timeout' });
    server.use(http.get(`${PATH}/execution-1`, () => HttpResponse.json({ reason: 'runtime_failure', error: 'The execution failed.' }, { status: 500 })));
    expect(await readToolkitToolResult(request)).toEqual({ kind: 'failure', message: 'The execution failed.' });
  });
});

it('looks up request receipts and switches pending observation to the accepted execution', async () => {
  server.use(http.get(`${PATH}/request-key`, ({ request }) => {
    expect(new URL(request.url).searchParams.get('lookup')).toBe('request');
    return HttpResponse.json({ pending: true, task_id: 'accepted-execution' }, { status: 202 });
  }));
  const params = { projectId: 'proj-1', toolkitId: 'tk-1', taskId: 'request-key', lookup: 'request' as const };
  const outcome = await readToolkitToolResult(params);
  expect(outcome).toMatchObject({ kind: 'timeout', taskId: 'accepted-execution' });
  expect(outcome).not.toHaveProperty('lookup');
  server.use(http.get(`${PATH}/request-key`, () => HttpResponse.json({}, { status: 404 })));
  expect(await readToolkitToolResult(params)).toMatchObject({ kind: 'unconfirmed' });
});
