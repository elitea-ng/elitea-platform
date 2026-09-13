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
