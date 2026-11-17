import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { EvalRun } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { fetchPreviousScoredRun } from './evaluationInsightsApi';

const BASE = '/api/v2';
const URL_RUNS = `${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`;

function run(id: string, mode: EvalRun['execution_mode'], overrides: Partial<EvalRun> = {}): EvalRun {
  return {
    id,
    dataset_id: '5',
    status: 'finished',
    execution_mode: mode,
    progress: { done: 1, total: 1 },
    headline_score: 50,
    ...overrides,
  };
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('fetchPreviousScoredRun', () => {
  it('skips an earlier run of another mode and returns the nearest of the same mode', async () => {
    const seen: string[] = [];
    server.use(
      http.get(URL_RUNS, ({ request }) => {
        const params = new URL(request.url).searchParams;
        seen.push(`${params.get('dataset_id')}|${params.get('before_run_id')}`);
        return HttpResponse.json({
          rows: [run('8', 'runtime_plane'), run('7', 'predict_blocking'), run('6', 'predict_blocking')],
          total: 3,
        });
      }),
    );
    const previous = await fetchPreviousScoredRun('1', run('9', 'predict_blocking'));
    expect(previous?.id).toBe('7');
    expect(seen).toEqual(['5|9']);
  });

  it('walks back through full pages until a run of the same mode turns up', async () => {
    const page = (start: number) =>
      Array.from({ length: 50 }, (_, index) => run(String(start - index), 'runtime_plane'));
    const seen: string[] = [];
    server.use(
      http.get(URL_RUNS, ({ request }) => {
        const before = new URL(request.url).searchParams.get('before_run_id') ?? '';
        seen.push(before);
        if (before === '200') return HttpResponse.json({ rows: page(199), total: 50 });
        return HttpResponse.json({ rows: [run('100', 'predict_blocking')], total: 1 });
      }),
    );
    const previous = await fetchPreviousScoredRun('1', run('200', 'predict_blocking'));
    expect(previous?.id).toBe('100');
    expect(seen).toEqual(['200', '150']);
  });

  it('answers undefined when no earlier run shares the mode', async () => {
    server.use(http.get(URL_RUNS, () => HttpResponse.json({ rows: [run('8', 'runtime_plane')], total: 1 })));
    expect(await fetchPreviousScoredRun('1', run('9', 'predict_blocking'))).toBeUndefined();
  });

  it('keeps the agent filter when the run is pinned to an agent', async () => {
    let agent: string | null = null;
    server.use(
      http.get(URL_RUNS, ({ request }) => {
        agent = new URL(request.url).searchParams.get('application_id');
        return HttpResponse.json({ rows: [], total: 0 });
      }),
    );
    await fetchPreviousScoredRun('1', run('9', 'predict_blocking', { application_id: 42 }));
    expect(agent).toBe('42');
  });
});
