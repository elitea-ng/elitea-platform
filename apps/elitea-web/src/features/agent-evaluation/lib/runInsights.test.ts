import { describe, expect, it } from 'vitest';

import type { EvalRun, EvalRunResult } from '@/shared/api/generated/model';

import {
  caseTurns,
  compareRuns,
  dimensionAverages,
  percentile,
  summarizeEfficiency,
} from './runInsights';

function result(caseId: string, dimensionId: string, normalized: number | null, evidence: Record<string, unknown> = {}): EvalRunResult {
  return {
    id: `${caseId}-${dimensionId}`,
    run_id: '1',
    dataset_case_id: caseId,
    dimension_id: dimensionId,
    status: normalized === null ? 'error' : 'ok',
    native_score: normalized,
    normalized_score: normalized,
    evidence,
  };
}

function run(id: string, overrides: Partial<EvalRun> = {}): EvalRun {
  return {
    id,
    dataset_id: '5',
    status: 'finished',
    execution_mode: 'predict_blocking',
    progress: { done: 1, total: 1 },
    headline_score: 50,
    created_at: `2026-11-0${id}T00:00:00Z`,
    ...overrides,
  };
}

describe('caseTurns', () => {
  it('collapses the rows of a case into one turn and keeps the recorded latency', () => {
    const turns = caseTurns([
      result('a', '1', 80, { agent_latency_ms: 1200 }),
      result('a', '2', 70, { agent_latency_ms: 1200 }),
      result('b', '1', 60, { agent_latency_ms: 300, agent_timed_out: true }),
    ]);
    expect(turns).toEqual([
      { caseId: 'a', latencyMs: 1200, timedOut: false },
      { caseId: 'b', latencyMs: 300, timedOut: true },
    ]);
  });

  // A run scored before latency was recorded must not read as "answered in 0 ms".
  it('leaves latency undefined when the evidence has none', () => {
    expect(caseTurns([result('a', '1', 80)])).toEqual([{ caseId: 'a', latencyMs: undefined, timedOut: false }]);
    const efficiency = summarizeEfficiency(caseTurns([result('a', '1', 80)]), undefined);
    expect(efficiency.p50Ms).toBeUndefined();
    expect(efficiency.measuredCases).toBe(0);
  });
});

describe('percentile', () => {
  it('uses the nearest rank', () => {
    const values = [10, 20, 30, 40, 50, 60, 70, 80, 90, 100];
    expect(percentile(values, 0.5)).toBe(50);
    expect(percentile(values, 0.95)).toBe(100);
    expect(percentile([], 0.5)).toBeUndefined();
  });
});

describe('summarizeEfficiency', () => {
  it('derives per-case tokens and cost from the analytics totals', () => {
    const turns = caseTurns([result('a', '1', 80, { agent_latency_ms: 100 }), result('b', '1', 80, { agent_latency_ms: 300, agent_timed_out: true })]);
    const efficiency = summarizeEfficiency(turns, {
      run_id: '1',
      application_id: 1,
      application_version_id: 1,
      status: 'finished',
      created_by: 1,
      created_at: '2026-11-01T00:00:00Z',
      available: true,
      totals: { llm_calls: 4, prompt_tokens: 600, completion_tokens: 400, total_tokens: 1000, errors: 0, avg_duration_ms: 1, unpriced_calls: 0, total_cost: 0.5 },
    });
    expect(efficiency).toMatchObject({ p50Ms: 100, p95Ms: 300, timeouts: 1, tokensPerCase: 500, costPerCase: 0.25 });
  });

  it('reads no cost from an unavailable analytics answer', () => {
    const efficiency = summarizeEfficiency(caseTurns([result('a', '1', 80)]), {
      run_id: '1', application_id: 1, application_version_id: 1, status: 'finished', created_by: 1,
      created_at: '2026-11-01T00:00:00Z', available: false, unavailable_reason: 'log_pruned',
    });
    expect(efficiency.tokensPerCase).toBeUndefined();
    expect(efficiency.costPerCase).toBeUndefined();
  });
});

describe('comparison with the previous run', () => {
  it('compares only the dimensions both runs scored', () => {
    const now = [result('a', '1', 80), result('b', '1', 60), result('a', '2', 90)];
    const before = [result('a', '1', 50), result('a', '3', 10)];
    expect(dimensionAverages(now).get('1')).toBe(70);
    const comparison = compareRuns(
      { run: run('2', { headline_score: 75 }), results: now, efficiency: summarizeEfficiency([], undefined) },
      { run: run('1', { headline_score: 50 }), results: before, efficiency: summarizeEfficiency([], undefined) },
    );
    expect(comparison.headline).toBe(25);
    expect(comparison.dimensions).toEqual([{ dimensionId: '1', current: 70, previous: 50, delta: 20 }]);
    expect(comparison.p50Ms).toBeUndefined();
  });
});
