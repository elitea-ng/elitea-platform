import { describe, expect, it } from 'vitest';

import type { EvalRunResult } from '@/shared/api/generated/model';

import { summarizeTrajectory } from './trajectory';

function result(caseId: string, evidence: Record<string, unknown>, dimension = '1'): EvalRunResult {
  return {
    id: `${caseId}-${dimension}`, run_id: '1', dataset_case_id: caseId, dimension_id: dimension,
    status: 'ok', native_score: 1, normalized_score: 50, evidence,
  };
}

describe('summarizeTrajectory', () => {
  it('is undefined when no case has a trajectory, so nothing is invented', () => {
    expect(summarizeTrajectory([result('a', { agent_latency_ms: 10 })])).toBeUndefined();
    expect(summarizeTrajectory([])).toBeUndefined();
  });

  it('collapses the rows of a case and sums the counts of the measured cases', () => {
    const trajectory = { steps: 4, model_calls: 2, tool_calls: 2, tool_errors: 1, retries: 1, tools: [{ name: 'fetch', calls: 2, errors: 1 }], items: [] };
    const summary = summarizeTrajectory([
      result('a', { trajectory }, '1'),
      result('a', { trajectory }, '2'),
      result('b', {}),
    ]);
    expect(summary?.totalCases).toBe(2);
    expect(summary?.cases).toHaveLength(1);
    expect(summary).toMatchObject({ toolCalls: 2, toolErrors: 1, retries: 1, stepsP50: 4, stepsP95: 4 });
    expect(summary?.cases[0]?.tools).toEqual([{ name: 'fetch', toolkitId: undefined, calls: 2, errors: 1 }]);
  });

  it('ignores a malformed trajectory instead of reading it as zero', () => {
    expect(summarizeTrajectory([result('a', { trajectory: { steps: 'many' } }), result('b', { trajectory: 7 })])).toBeUndefined();
  });
});
