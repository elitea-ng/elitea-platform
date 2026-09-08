import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { testToolkitTool } from './toolkitTestRun';

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

    expect(body).toEqual({ tool_name: 'search_index', tool_params: { query: 'x' } });
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

  it('a network failure (no handler, MSW onUnhandledRequest) still resolves to the generic failure kind', async () => {
    // No `server.use(...)` override — the request itself never reaches a
    // handler in this test's own describe scope, so the shared `server`'s
    // `onUnhandledRequest: 'error'` (see `src/test/setup.ts`) throws inside
    // the fetch. `testToolkitTool` must still resolve, never reject.
    server.use(http.post(PATH, () => HttpResponse.error()));

    const outcome = await testToolkitTool({ projectId: 'proj-1', toolkitId: 'tk-1', toolName: 'search_index', toolParams: {} });

    expect(outcome.kind).toBe('failure');
  });
});
