import { screen } from '@testing-library/react';
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

const run: EvalRun = {
  id: '2', dataset_id: '5', application_id: 42, status: 'finished', execution_mode: 'runtime_plane',
  progress: { done: 2, total: 2 }, headline_score: 70, created_at: '2026-11-02T00:00:00Z',
};

function row(caseId: string, evidence: Record<string, unknown>) {
  return {
    id: `2-${caseId}`, run_id: '2', dataset_case_id: caseId, dimension_id: '3', status: 'ok',
    native_score: 4, normalized_score: 70, evidence: { input: `question ${caseId}`, agent_latency_ms: 1000, ...evidence },
  };
}

function scorecard(results: unknown[]): EvalScorecard {
  return { run, results, headline_score: 70, total: results.length, offset: 0, unavailable: [] } as unknown as EvalScorecard;
}

const names = new Map([['3', 'Helpfulness']]);

function trajectory(toolCalls: number, extra: Record<string, unknown> = {}) {
  return { steps: toolCalls + 1, model_calls: 1, tool_calls: toolCalls, tool_errors: 0, retries: 0, tools: [], items: [], truncated: false, ...extra };
}

describe('the Trajectory card counters of issue 2192', () => {
  it('shows distinct tools, redundant calls, guardrail events and step limit hit with each case value', async () => {
    const fetchTool = { name: 'fetch', toolkit_id: 'web', calls: 3, errors: 0 };
    const results = [
      row('a', {
        trajectory: trajectory(4, { distinct_tools: 2, redundant_calls: 1, tools: [fetchTool, { name: 'list', toolkit_id: 'web', calls: 1, errors: 0 }] }),
        guardrail: { events: 3, blocked: 1, masked: 1, warned: 0, gated: 1 },
      }),
      row('b', {
        trajectory: trajectory(4, { distinct_tools: 1, redundant_calls: 0, tools: [fetchTool] }),
        guardrail: { events: 0, blocked: 0, masked: 0, warned: 0, gated: 0 },
        agent_step_limit: true,
      }),
    ];
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard(results)} />);

    expect(await screen.findByTestId('run-trajectory-distinct-tools')).toHaveTextContent('1.5 per case, 2 in all');
    expect(screen.getByTestId('run-trajectory-redundant')).toHaveTextContent('1 (13% of tool calls)');
    expect(screen.getByTestId('run-trajectory-guardrail')).toHaveTextContent('3');
    expect(screen.getByTestId('run-trajectory-step-limit')).toHaveTextContent('1 of 2 case(s)');
    expect(screen.getByTestId('trajectory-extra-a')).toHaveTextContent('2 distinct tool(s), 1 redundant, 3 guardrail event(s)');
    expect(screen.getByTestId('trajectory-guardrail-a')).toHaveTextContent('1 blocked, 1 masked, 0 warned, 1 held for approval');
    expect(screen.queryByTestId('trajectory-guardrail-b')).not.toBeInTheDocument();
    expect(screen.getByTestId('trajectory-case-b')).toHaveTextContent('ended at the step limit');
  });

  it('says not recorded for a run stored before the counters, never 0', async () => {
    renderWithEvaluationProviders(<RunInsights projectId="1" dimensionNames={names} scorecard={scorecard([row('a', { trajectory: trajectory(2) })])} />);

    expect(await screen.findByTestId('run-trajectory-distinct-tools')).toHaveTextContent('not recorded');
    expect(screen.getByTestId('run-trajectory-redundant')).toHaveTextContent('not recorded');
    expect(screen.getByTestId('run-trajectory-guardrail')).toHaveTextContent('not recorded');
    expect(screen.getByTestId('run-trajectory-step-limit')).toHaveTextContent('0 of 1 case(s)');
  });
});
