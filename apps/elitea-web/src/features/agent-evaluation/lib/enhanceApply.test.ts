import { describe, expect, it } from 'vitest';

import type { ApplicationVersionDetail, EvalDatasetCase } from '@/shared/api/generated/model';

import type { EvalDimension } from '../model/types';
import { caseWriteInput, dimensionWriteInput, draftVersionBody, draftVersionName } from './enhanceApply';

const row: EvalDimension = {
  id: '3', name: 'Helpfulness', description: 'old rubric', tier: 'project', application_id: null,
  allowed_engines: ['ai'], scale_type: 'ordinal', scale_min: 1, scale_max: 5, polarity: 'higher_better',
  default_weight: 2, default_target: 4, default_target_operator: '>=', code: '', return_contract: '',
};

describe('dimensionWriteInput', () => {
  it('replaces only the proposed rubric and keeps everything else from the stored row', () => {
    const input = dimensionWriteInput(row, { dimension_id: '3', dimension_name: 'Helpfulness', description: 'new rubric', rationale: 'r' });
    expect(input).toMatchObject({ description: 'new rubric', default_target: 4, default_target_operator: '>=', default_weight: 2, polarity: 'higher_better' });
  });

  it('replaces the target and its operator together', () => {
    const input = dimensionWriteInput(row, { dimension_id: '3', dimension_name: 'H', default_target: 3, default_target_operator: '>', rationale: 'r' });
    expect(input).toMatchObject({ description: 'old rubric', default_target: 3, default_target_operator: '>' });
  });
});

describe('caseWriteInput', () => {
  it('keeps the stored input and variables and changes only the expected output', () => {
    const stored = { id: '11', dataset_id: '5', input: 'q {{x}}', variables: { x: 1 }, expected_output: 'old', source_type: 'manual', order_index: 0, excluded: false } satisfies EvalDatasetCase;
    expect(caseWriteInput(stored, { case_id: '11', dataset_id: '5', input: 'q', current_expected_output: 'old', expected_output: 'new', rationale: 'r' })).toEqual({
      input: 'q {{x}}', variables: { x: 1 }, expected_output: 'new',
    });
  });
});

describe('draft version', () => {
  // Everything the pinned version holds, including keys this file has never heard of.
  const base = {
    id: '9', application_id: '1', name: 'base', status: 'draft', created_at: '2026-11-01T00:00:00Z', author_id: '3',
    author: { id: '3' }, is_forked: false, tools: [{ id: 1 }],
    instructions: 'old', agent_type: 'openai', welcome_message: 'hi', llm_settings: { model_name: 'qwen', temperature: 0.2 },
    conversation_starters: ['a', 'b'], variables: [{ name: 'x', value: '1' }], tags: [{ name: 't' }],
    meta: { step_limit: 4, notes: 'n' }, pipeline_settings: { nodes: [] }, soul: { voice: 'dry' }, a_future_field: { keep: true },
  } as unknown as ApplicationVersionDetail;

  it('differs from the base only in the instructions', () => {
    const body = draftVersionBody(base, 'new') as Record<string, unknown>;
    const serverOwned = ['id', 'application_id', 'name', 'status', 'created_at', 'author_id', 'author', 'is_forked', 'tools'];
    const expected: Record<string, unknown> = { ...(base as unknown as Record<string, unknown>), instructions: 'new' };
    for (const key of serverOwned) delete expected[key];
    expect(body).toEqual(expected);
  });

  it('leaves out null and absent fields so the new version stores its defaults', () => {
    const body = draftVersionBody({ ...base, soul: null, meta: null } as unknown as ApplicationVersionDetail, 'new');
    expect(body).not.toHaveProperty('soul');
    expect(body).not.toHaveProperty('meta');
  });

  it('names every accept uniquely, so a second accept cannot collide with the first draft', () => {
    expect(draftVersionName('42', 1_000_000)).toBe('eval-fix-42-lfls');
    expect(draftVersionName('42', 1_000_000)).not.toBe(draftVersionName('42', 2_000_000));
  });
});
