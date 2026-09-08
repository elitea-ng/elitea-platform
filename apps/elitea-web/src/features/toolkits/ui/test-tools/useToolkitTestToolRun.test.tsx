import { act, renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useToolkitTestToolRun } from './useToolkitTestToolRun';

const RUN_PATH = '/api/v2/elitea_core/test_tool/prompt_lib/:projectId/:toolkitId';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
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

    expect(seen).toEqual({ tool_name: 'list_branches_in_repo', tool_params: { repository: 'a/b' } });
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
