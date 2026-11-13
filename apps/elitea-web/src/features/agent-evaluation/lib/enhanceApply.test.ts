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
  it('carries the pinned version with the new instructions and never a name or id', () => {
    const base = { id: '9', application_id: '1', name: 'base', status: 'draft', instructions: 'old', agent_type: 'openai', llm_settings: { model_name: 'qwen' }, meta: { step_limit: 4 } } as unknown as ApplicationVersionDetail;
    const body = draftVersionBody(base, 'new');
    expect(body).toMatchObject({ instructions: 'new', agent_type: 'openai', llm_settings: { model_name: 'qwen' }, meta: { step_limit: 4 } });
    expect(body).not.toHaveProperty('name');
    expect(body).not.toHaveProperty('id');
  });

  it('names the draft after the run, within the version name limit', () => {
    expect(draftVersionName('42')).toBe('eval-fix-42');
    expect(draftVersionName('9'.repeat(40)).length).toBeLessThanOrEqual(20);
  });
});
