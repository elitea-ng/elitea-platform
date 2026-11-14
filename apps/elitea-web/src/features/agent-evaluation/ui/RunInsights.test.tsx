import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { EvalRun, EvalScorecard } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { RunInsights } from './RunInsights';

const BASE = '/api/v2';

function figures(tokens: number, cost: number) {
  return { llm_calls: 2, prompt_tokens: tokens / 2, completion_tokens: tokens / 2, total_tokens: tokens, errors: 0, avg_duration_ms: 10, unpriced_calls: 0, total_cost: cost };
}

function analytics(overrides: Record<string, unknown> = {}) {
  return {
    run_id: '9', application_id: 42, application_version_id: 9, status: 'finished', created_by: 1,
    created_at: '2026-11-02T00:00:00Z', available: true,
    totals: figures(1000, 0.5), agent: figures(800, 0.4), judge: figures(200, 0.1),
    ...overrides,
  };
}

function run(id: string, overrides: Partial<EvalRun> = {}): EvalRun {
  return {
    id, dataset_id: '5', application_id: 42, status: 'finished', execution_mode: 'predict_blocking',
    progress: { done: 2, total: 2 }, headline_score: 75, created_at: `2026-11-0${id}T00:00:00Z`, ...overrides,
  };
}

function row(runId: string, caseId: string, normalized: number, latency?: number, timedOut = false) {
  return {
    id: `${runId}-${caseId}`, run_id: runId, dataset_case_id: caseId, dimension_id: '3', status: 'ok',
    native_score: 4, normalized_score: normalized,
    evidence: latency === undefined ? {} : { agent_latency_ms: latency, ...(timedOut ? { agent_timed_out: true } : {}) },
  };
}

function scorecard(current: EvalRun, results: unknown[]): EvalScorecard {
  return { run: current, results, headline_score: current.headline_score, total: results.length, offset: 0, unavailable: [] } as unknown as EvalScorecard;
}

const previousQueries: URLSearchParams[] = [];

function mockHistory(options: { readonly previous?: boolean } = {}): void {
  previousQueries.length = 0;
  server.use(
    // The previous run is a server-side query: nearest older scored run, one row.
    http.get(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, ({ request }) => {
      previousQueries.push(new URL(request.url).searchParams);
      const rows = options.previous === false ? [] : [run('1', { headline_score: 50 })];
      return HttpResponse.json({ rows, total: rows.length });
    }),
    http.get(`${BASE}/elitea_core/eval_results/prompt_lib/:projectId/:runId`, ({ params }) =>
      HttpResponse.json(scorecard(run(String(params['runId']), { headline_score: 50 }), [row('1', 'a', 40, 2000), row('1', 'b', 60, 3000)])),
    ),
  );
}

const names = new Map([['3', 'Helpfulness']]);

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('RunInsights', () => {
  it('shows agent and judge consumption apart, and the efficiency strip from recorded latency', async () => {
    server.use(http.get(`${BASE}/elitea_core/eval_run_analytics/prompt_lib/:projectId/:runId`, () => HttpResponse.json(analytics())));
    mockHistory();
    const current = run('2');
    renderWithEvaluationProviders(
      <RunInsights projectId="1" dimensionNames={names} scorecard={scorecard(current, [row('2', 'a', 80, 1000), row('2', 'b', 70, 5000, true)])} />,
    );

    expect(await screen.findByTestId('run-consumption-agent')).toHaveTextContent('800 tokens');
    expect(screen.getByTestId('run-consumption-judge')).toHaveTextContent('200 tokens');
    expect(screen.getByTestId('run-efficiency-p50')).toHaveTextContent('1.0 s');
    expect(screen.getByTestId('run-efficiency-p95')).toHaveTextContent('5.0 s');
    expect(screen.getByTestId('run-efficiency-timeouts')).toHaveTextContent('1');
    expect(screen.getByTestId('run-efficiency-tokens')).toHaveTextContent('500');
  });

  it('compares with the previous run on the same dataset', async () => {
    server.use(http.get(`${BASE}/elitea_core/eval_run_analytics/prompt_lib/:projectId/:runId`, () => HttpResponse.json(analytics())));
    mockHistory();
    renderWithEvaluationProviders(
      <RunInsights projectId="1" dimensionNames={names} scorecard={scorecard(run('2'), [row('2', 'a', 80, 1000), row('2', 'b', 70, 1000)])} />,
    );

    // 75 against 50, and the dimension average moved from 50 to 75.
    await waitFor(() => expect(screen.getByTestId('run-comparison-headline')).toHaveTextContent('+25.0'));
    expect(screen.getByTestId('run-comparison-dimension-3')).toHaveTextContent('Helpfulness: +25.0');
    expect(screen.getByTestId('run-comparison-latency')).toHaveTextContent('-1.0 s');
    expect(previousQueries[0]?.get('before_run_id')).toBe('2');
    expect(previousQueries[0]?.get('scored_only')).toBe('true');
    expect(previousQueries[0]?.get('limit')).toBe('1');
    expect(previousQueries[0]?.get('dataset_id')).toBe('5');
  });

  it('says there is no earlier run instead of comparing with nothing', async () => {
    server.use(http.get(`${BASE}/elitea_core/eval_run_analytics/prompt_lib/:projectId/:runId`, () => HttpResponse.json(analytics())));
    mockHistory({ previous: false });
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard(run('2'), [row('2', 'a', 80, 1000)])} />);
    expect(await screen.findByTestId('run-comparison-none')).toBeInTheDocument();
  });

  // The analytics read has its own permission; a viewer without it still sees efficiency.
  it('names a missing analytics permission and still shows latency', async () => {
    server.use(http.get(`${BASE}/elitea_core/eval_run_analytics/prompt_lib/:projectId/:runId`, () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })));
    mockHistory({ previous: false });
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard(run('2'), [row('2', 'a', 80, 400)])} />);
    expect(await screen.findByTestId('run-consumption-gap')).toHaveTextContent('analytics permission');
    expect(screen.getByTestId('run-efficiency-p50')).toHaveTextContent('400 ms');
    expect(screen.getByTestId('run-efficiency-cost')).toHaveTextContent('not priced');
  });

  it('says a run from before attribution has no figures rather than showing zero', async () => {
    server.use(http.get(`${BASE}/elitea_core/eval_run_analytics/prompt_lib/:projectId/:runId`, () =>
      HttpResponse.json({ run_id: '2', application_id: 42, application_version_id: 9, status: 'finished', created_by: 1, created_at: '2026-11-02T00:00:00Z', available: false, unavailable_reason: 'before_attribution' })));
    mockHistory({ previous: false });
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard(run('2'), [row('2', 'a', 80)])} />);
    expect(await screen.findByTestId('run-consumption-gap')).toHaveTextContent('before consumption was recorded');
    expect(screen.queryByTestId('run-consumption-total')).toBeNull();
    // No latency was recorded for this run either.
    expect(screen.getByTestId('run-efficiency-p50')).toHaveTextContent('not recorded');
  });

  it('shows nothing while the run is still active', () => {
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard(run('2', { status: 'running' }), [])} />);
    expect(screen.queryByTestId('run-insights')).toBeNull();
  });
});
