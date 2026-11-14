import { screen, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { EvalRun, EvalScorecard } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { RunInsights } from './RunInsights';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/eval_run_analytics/prompt_lib/:projectId/:runId`, () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })),
    http.get(`${BASE}/elitea_core/eval_runs/prompt_lib/:projectId`, () => HttpResponse.json({ rows: [], total: 0 })),
  );
});
afterEach(() => resetGeneratedClient());

function run(mode: 'predict_blocking' | 'runtime_plane'): EvalRun {
  return {
    id: '2', dataset_id: '5', application_id: 42, status: 'finished', execution_mode: mode,
    progress: { done: 3, total: 3 }, headline_score: 70, created_at: '2026-11-02T00:00:00Z',
  };
}

function trajectory(steps: number, toolCalls: number, errors: number, retries: number, items: unknown[] = []) {
  return { steps, model_calls: steps - toolCalls, tool_calls: toolCalls, tool_errors: errors, retries, tools: [], items, truncated: false };
}

function row(caseId: string, evidence: Record<string, unknown>) {
  return {
    id: `2-${caseId}`, run_id: '2', dataset_case_id: caseId, dimension_id: '3', status: 'ok',
    native_score: 4, normalized_score: 70, evidence: { input: `question ${caseId}`, agent_latency_ms: 1000, ...evidence },
  };
}

function scorecard(mode: 'predict_blocking' | 'runtime_plane', results: unknown[]): EvalScorecard {
  return { run: run(mode), results, headline_score: 70, total: results.length, offset: 0, unavailable: [] } as unknown as EvalScorecard;
}

const names = new Map([['3', 'Helpfulness']]);

describe('the Trajectory card (issue 2011, step B)', () => {
  it('shows steps per case p50 and p95, the tool counts and each case from the step evidence', async () => {
    const results = [
      row('a', { trajectory: trajectory(2, 1, 0, 0, [
        { seq: 1, kind: 'model_call', name: '', duration_ms: 40 },
        { seq: 2, kind: 'tool_call', name: 'search', duration_ms: 90, output: 'three hits' },
      ]) }),
      row('b', { trajectory: trajectory(6, 4, 1, 1) }),
      row('c', { trajectory: trajectory(10, 7, 2, 1), agent_step_limit: true }),
    ];
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard('runtime_plane', results)} />);

    expect(await screen.findByTestId('run-trajectory')).toBeInTheDocument();
    // Nearest rank over [2, 6, 10].
    expect(screen.getByTestId('run-trajectory-p50')).toHaveTextContent('6');
    expect(screen.getByTestId('run-trajectory-p95')).toHaveTextContent('10');
    expect(screen.getByTestId('run-trajectory-tool-calls')).toHaveTextContent('12');
    expect(screen.getByTestId('run-trajectory-tool-errors')).toHaveTextContent('3');
    expect(screen.getByTestId('run-trajectory-retries')).toHaveTextContent('2');
    expect(screen.queryByTestId('run-trajectory-partial')).not.toBeInTheDocument();

    expect(screen.getByTestId('trajectory-counts-b')).toHaveTextContent('6 step(s), 4 tool call(s), 1 error(s), 1 retry(ies)');
    expect(screen.getByTestId('trajectory-case-c')).toHaveTextContent('ended at the step limit');
    // The steps of a case and the tool output kept in the evidence.
    const steps = screen.getByTestId('trajectory-steps-a');
    expect(within(steps).getByText(/tool_call search/)).toBeInTheDocument();
    expect(within(steps).getByText('three hits')).toBeInTheDocument();
  });

  it('adds the tool metrics to the Efficiency card', async () => {
    const results = [
      row('a', { trajectory: trajectory(3, 2, 1, 0) }),
      row('b', { trajectory: trajectory(5, 3, 0, 0) }),
    ];
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard('runtime_plane', results)} />);

    expect(await screen.findByTestId('run-efficiency-tool-calls')).toHaveTextContent('2.5');
    expect(screen.getByTestId('run-efficiency-tool-errors')).toHaveTextContent('1');
    expect(screen.getByTestId('run-efficiency-retries')).toHaveTextContent('0');
  });

  // A case that never started its agent run has no steps: it is not "0 steps".
  it('counts only the cases that recorded steps and says how many it measured', async () => {
    const results = [row('a', { trajectory: trajectory(4, 2, 0, 0) }), row('b', { agent_timed_out: true })];
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard('runtime_plane', results)} />);

    expect(await screen.findByTestId('run-trajectory-partial')).toHaveTextContent('1 of 2');
    expect(screen.getByTestId('run-trajectory-p50')).toHaveTextContent('4');
  });

  it('is absent for a run of one model call per case', async () => {
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard('predict_blocking', [row('a', {})])} />);

    expect(await screen.findByTestId('run-efficiency')).toBeInTheDocument();
    expect(screen.queryByTestId('run-trajectory')).not.toBeInTheDocument();
    expect(screen.queryByTestId('run-efficiency-tool-calls')).not.toBeInTheDocument();
  });

  it('says so when an agent-run run recorded no steps at all', async () => {
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard('runtime_plane', [row('a', {})])} />);

    expect(await screen.findByTestId('run-trajectory-none')).toBeInTheDocument();
    expect(screen.queryByTestId('run-efficiency-tool-calls')).not.toBeInTheDocument();
  });

  it('shows the note of a run that did not get the mode its limits asked for', async () => {
    const card = scorecard('predict_blocking', [row('a', {})]);
    card.run = { ...card.run, snapshot: { execution_note: 'a release gate run ran as one model call' } };
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={card} />);

    expect(await screen.findByTestId('run-execution-note')).toHaveTextContent('release gate');
  });
});
