import { describe, expect, it } from 'vitest';

import type { EvalRunResult } from '@/shared/api/generated/model';

import { summarizeTrajectory } from './trajectory';

function result(caseId: string, evidence: Record<string, unknown>, dimension = '1'): EvalRunResult {
  return {
    id: `${caseId}-${dimension}`, run_id: '1', dataset_case_id: caseId, dimension_id: dimension,
    status: 'ok', native_score: 1, normalized_score: 50, evidence,
  };
}

// Issue 2192: distinct tools, redundant calls, guardrail events, step limit hit.
describe('summarizeTrajectory, the issue 2192 counters', () => {
  const base = { steps: 4, model_calls: 1, tool_calls: 3, tool_errors: 0, retries: 0, items: [], truncated: false };

  it('averages distinct tools per case, unions them over the run and shares redundant calls', () => {
    const a = { ...base, distinct_tools: 2, redundant_calls: 1, tools: [{ name: 'fetch', toolkit_id: 'web', calls: 2, errors: 0 }, { name: 'list', toolkit_id: 'web', calls: 1, errors: 0 }] };
    const b = { ...base, distinct_tools: 1, redundant_calls: 0, tools: [{ name: 'fetch', toolkit_id: 'web', calls: 3, errors: 0 }] };
    const summary = summarizeTrajectory([result('a', { trajectory: a }), result('b', { trajectory: b })]);
    expect(summary).toMatchObject({ distinctToolsPerCase: 1.5, distinctToolsInRun: 2, redundantCalls: 1 });
    expect(summary?.redundantShare).toBeCloseTo(1 / 6);
  });

  it('reads a case stored before the counters existed as not recorded, never zero', () => {
    const summary = summarizeTrajectory([result('a', { trajectory: { ...base, tools: [] } })]);
    expect(summary?.distinctToolsPerCase).toBeUndefined();
    expect(summary?.redundantCalls).toBeUndefined();
    expect(summary?.guardrailEvents).toBeUndefined();
    expect(summary?.cases[0]?.guardrail).toBeUndefined();
  });

  it('sums guardrail events over the cases whose verdicts were read, even a case with no steps', () => {
    const guardrail = { events: 3, blocked: 1, masked: 1, warned: 0, gated: 1 };
    const summary = summarizeTrajectory([
      result('a', { trajectory: { ...base, tools: [] }, guardrail }),
      result('a', { trajectory_ref: '1', guardrail }, '2'),
      result('b', { guardrail: { events: 2, blocked: 2, masked: 0, warned: 0, gated: 0 } }),
      result('c', {}),
    ]);
    expect(summary).toMatchObject({ guardrailEvents: 5, guardrailCases: 2, totalCases: 3 });
    expect(summary?.cases[0]?.guardrail).toEqual(guardrail);
  });

  it('counts the cases that ended at the step limit, with their ids', () => {
    const summary = summarizeTrajectory([
      result('a', { trajectory: { ...base, tools: [] }, agent_step_limit: true }),
      result('a', { agent_step_limit: true }, '2'),
      result('b', { trajectory: { ...base, tools: [] } }),
      result('c', { agent_step_limit: true }),
    ]);
    expect(summary).toMatchObject({ stepLimitCases: 2, stepLimitCaseIds: ['a', 'c'], totalCases: 3 });
  });
});
