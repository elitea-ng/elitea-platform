import { act, renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { createStorage } from '@/shared/lib/storage';

import { useToolkitTestToolRun } from './useToolkitTestToolRun';

const RUN_PATH = '/api/v2/elitea_core/test_tool/prompt_lib/:projectId/:toolkitId';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
  createStorage('session').remove('toolkits.pendingTest');
});

function renderRun() {
  return renderHook(() => useToolkitTestToolRun({ projectId: '7', toolkitId: 'tk-1' }));
}

describe('useToolkitTestToolRun', () => {
  it('posts the tool name and arguments to the toolkit run route and keeps the result', async () => {
    let seen: unknown;
    server.use(
      http.post(RUN_PATH, async ({ request }) => {
        seen = await request.json();
        return HttpResponse.json({ ok: true, result: { branches: ['main'] }, truncated: false });
      }),
    );

    const { result } = renderRun();
    await act(async () => {
      await result.current.run('list_branches_in_repo', { repository: 'a/b' });
    });

    expect(seen).toEqual({ request_id: expect.any(String) as unknown, tool_name: 'list_branches_in_repo', tool_params: { repository: 'a/b' } });
    expect(result.current.outcome).toEqual({ kind: 'ok', result: { branches: ['main'] }, truncated: false });
    expect(result.current.isRunning).toBe(false);
  });

  it('reports a tool error as a settled outcome rather than a rejection', async () => {
    server.use(http.post(RUN_PATH, () => HttpResponse.json({ ok: false, error: 'the repository does not exist' })));

    const { result } = renderRun();
    await act(async () => {
      await result.current.run('list_branches_in_repo', {});
    });

    expect(result.current.outcome).toEqual({ kind: 'toolError', message: 'the repository does not exist' });
  });

  it('keeps the job id of a run that outlived the bounded wait', async () => {
    server.use(
      http.post(RUN_PATH, () =>
        HttpResponse.json({ ok: false, reason: 'timeout', task_id: 'job-42', error: 'still running' }, { status: 504 }),
      ),
    );

    const { result } = renderRun();
    await act(async () => {
      await result.current.run('slow_tool', {});
    });

    expect(result.current.outcome).toEqual({ kind: 'timeout', taskId: 'job-42', message: 'still running' });
  });

  it('drops the outcome when reset is called, so a previous tool’s result never sits under a new one', async () => {
    server.use(http.post(RUN_PATH, () => HttpResponse.json({ ok: true, result: 'done', truncated: false })));

    const { result } = renderRun();
    await act(async () => {
      await result.current.run('a_tool', {});
    });
    expect(result.current.outcome).toBeDefined();

    act(() => {
      result.current.reset();
    });
    expect(result.current.outcome).toBeUndefined();
  });

  it('renders the LAST press, not whichever answer arrives last', async () => {
    // The first press is held until the second has been made and answered, so
    // the two settle out of order. Without the press token the slow first
    // answer would overwrite the result the user actually asked for.
    let releaseFirst: (() => void) | undefined;
    const firstHeld = new Promise<void>((resolve) => {
      releaseFirst = resolve;
    });
    let call = 0;
    server.use(
      http.post(RUN_PATH, async () => {
        call += 1;
        if (call === 1) {
          await firstHeld;
          return HttpResponse.json({ ok: true, result: 'FIRST', truncated: false });
        }
        return HttpResponse.json({ ok: true, result: 'SECOND', truncated: false });
      }),
    );

    const { result } = renderRun();
    let firstRun: Promise<void> | undefined;
    act(() => {
      firstRun = result.current.run('a_tool', {});
    });
    await act(async () => {
      await result.current.run('a_tool', { second: true });
    });
    expect(result.current.outcome).toEqual({ kind: 'ok', result: 'SECOND', truncated: false });

    await act(async () => {
      releaseFirst?.();
      await firstRun;
    });

    await waitFor(() => {
      expect(result.current.outcome).toEqual({ kind: 'ok', result: 'SECOND', truncated: false });
    });
  });
});


it('retrieves the recovered result without a second submission', async () => {
  let submissions = 0;
  let reads = 0;
  server.use(
    http.post(RUN_PATH, () => { submissions += 1; return HttpResponse.json({ reason: 'timeout', task_id: 'job-recovery' }, { status: 504 }); }),
    http.get(`${RUN_PATH}/job-recovery`, () => {
      reads += 1;
      return reads === 1 ? HttpResponse.json({ pending: true }, { status: 202 }) : HttpResponse.json({ ok: true, result: 'recovered' });
    }),
  );
  const { result } = renderRun();
  await act(async () => { await result.current.run('slow_tool', {}); });
  await waitFor(() => { expect(result.current.outcome).toMatchObject({ kind: 'ok', result: 'recovered' }); }, { timeout: 7000 });
  expect(submissions).toBe(1);
  expect(reads).toBe(2);
}, 10000);


it('recovers the latest pending execution after remount without resubmitting arguments', async () => {
  let submissions = 0;
  server.use(
    http.post(RUN_PATH, () => { submissions += 1; return HttpResponse.json({ reason: 'timeout', task_id: 'job-reload' }, { status: 504 }); }),
    http.get(`${RUN_PATH}/job-reload`, () => HttpResponse.json({ ok: true, result: 'after-reload' })),
  );
  const first = renderRun();
  await act(async () => { await first.result.current.run('slow_tool', { secret: 'never-persist-this' }); });
  expect(createStorage('session').get('toolkits.pendingTest')).not.toContain('never-persist-this');
  first.unmount();
  const second = renderRun();
  await waitFor(() => { expect(second.result.current.outcome).toMatchObject({ kind: 'ok', result: 'after-reload' }); }, { timeout: 4000 });
  expect(submissions).toBe(1);
  expect(createStorage('session').get('toolkits.pendingTest')).toBeNull();
});

it('restores authorization arguments in memory after reload and waits for user authorization', async () => {
  createStorage('session').setJSON('toolkits.pendingTest', { projectId: '7', toolkitId: '19', taskId: 'job-auth' });
  let submissions = 0;
  let submitted: unknown;
  server.use(
    http.get('/api/v2/elitea_core/test_tool/prompt_lib/7/19/job-auth', () => HttpResponse.json({
      task_id: 'job-auth', reason: 'authorization_required',
      authorization_required: { toolkit_id: '19', toolkit_name: 'Saved', toolkit_type: 'mcp', server_url: 'https://example.test/' },
      authorization_retry: { tool_name: 'echo_marker', tool_params: { marker: 'original-input' } },
    }, { status: 409 })),
    http.post(RUN_PATH, async ({ request }) => { submissions += 1; submitted = await request.json(); return HttpResponse.json({ ok: true, result: 'authorized' }); }),
  );
  const { result } = renderHook(() => useToolkitTestToolRun({ projectId: '7', toolkitId: '19' }));
  await waitFor(() => { expect(result.current.outcome?.kind).toBe('authorizationRequired'); }, { timeout: 4000 });
  expect(submissions).toBe(0);
  expect(createStorage('session').get('toolkits.pendingTest')).not.toContain('original-input');
  await act(async () => { await result.current.authorize('a'.repeat(43)); });
  expect(submissions).toBe(1);
  expect(submitted).toMatchObject({ tool_name: 'echo_marker', tool_params: { marker: 'original-input' }, mcp_authorization_reference: 'a'.repeat(43) });
  expect(result.current.outcome).toMatchObject({ kind: 'ok', result: 'authorized' });
});


it('retains discovery authorization when selecting a tool and clears it when the toolkit changes', async () => {
  const references: unknown[] = [];
  server.use(http.post(RUN_PATH, async ({ request }) => {
    const body = await request.json() as Record<string, unknown>;
    references.push(body['mcp_authorization_reference']);
    return HttpResponse.json({ ok: true, result: 'done', truncated: false });
  }));
  const { result, rerender } = renderHook(({ toolkitId }) => useToolkitTestToolRun({ projectId: '7', toolkitId }), { initialProps: { toolkitId: '19' } });
  act(() => {
    result.current.rememberAuthorization('A'.repeat(43));
    result.current.reset();
  });
  await act(async () => { await result.current.run('echo_marker', { marker: 'first' }); });
  rerender({ toolkitId: '20' });
  await act(async () => { await result.current.run('echo_marker', { marker: 'second' }); });
  expect(references).toEqual(['A'.repeat(43), undefined]);
});

it('saves the reference before POST and recovers after unmount without a received execution id', async () => {
  let release!: () => void;
  let reached!: () => void;
  const arrived = new Promise<void>((resolve) => { reached = resolve; });
  const held = new Promise<void>((resolve) => { release = resolve; });
  let submissions = 0;
  let requestKey = '';
  server.use(
    http.post(RUN_PATH, async ({ request }) => {
      submissions += 1;
      requestKey = request.headers.get('Idempotency-Key') ?? '';
      expect(requestKey).not.toBe('');
      const saved = createStorage('session').get('toolkits.pendingTest') ?? '';
      expect(JSON.parse(saved)).toEqual({ projectId: '7', toolkitId: 'tk-1', taskId: requestKey, lookup: 'request' });
      expect(saved).not.toContain('private-argument');
      reached();
      await held;
      return HttpResponse.error();
    }),
    http.get(`${RUN_PATH}/:receipt`, ({ request, params }) => {
      expect(params['receipt']).toBe(requestKey);
      expect(new URL(request.url).searchParams.get('lookup')).toBe('request');
      return HttpResponse.json({ ok: true, result: 'original-execution-result' });
    }),
  );
  const first = renderRun();
  let running!: Promise<void>;
  act(() => { running = first.result.current.run('echo_marker', { marker: 'private-argument' }); });
  await arrived;
  first.unmount();
  release();
  await running;
  const second = renderRun();
  await waitFor(() => { expect(second.result.current.outcome).toMatchObject({ kind: 'ok', result: 'original-execution-result' }); }, { timeout: 4000 });
  expect(submissions).toBe(1);
  expect(createStorage('session').get('toolkits.pendingTest')).toBeNull();
});

it('retains an unconfirmed request for reload and never resubmits it', async () => {
  let submissions = 0;
  let reads = 0;
  server.use(
    http.post(RUN_PATH, () => { submissions += 1; return HttpResponse.error(); }),
    http.get(`${RUN_PATH}/:receipt`, () => { reads += 1; return HttpResponse.json({}, { status: 404 }); }),
  );
  const first = renderRun();
  await act(async () => { await first.result.current.run('echo_marker', {}); });
  await waitFor(() => { expect(first.result.current.outcome?.kind).toBe('unconfirmed'); }, { timeout: 4000 });
  const receipt = createStorage('session').get('toolkits.pendingTest');
  expect(receipt).toContain('request');
  first.unmount();
  const second = renderRun();
  await waitFor(() => { expect(second.result.current.outcome?.kind).toBe('unconfirmed'); }, { timeout: 4000 });
  expect(createStorage('session').get('toolkits.pendingTest')).toBe(receipt);
  expect(submissions).toBe(1);
  expect(reads).toBe(2);
}, 10000);
