/**
 * The platform model form's description (legacy issue 6766): read from the
 * stored row, written trimmed, and written even when blank so a cleared
 * description does not survive the merge with the stored object.
 */
import { describe, expect, it } from 'vitest';

import type { PlatformModel } from './api/adminLlmPlatformModelsApi';
import { formOf, modelDraftOf } from './platformModelForm';

const STORED = {
  elitea_title: 'gpt5',
  type: 'llm_model',
  model_name: 'gpt-5',
  credential_name: 'openai',
  low_tier: false,
  high_tier: true,
  data: { name: 'gpt-5', description: 'Fast for everyday tasks', ai_credentials: { elitea_title: 'openai' } },
} as unknown as PlatformModel;

describe('platform model description', () => {
  it('opens a new model with no description and an edited one with its stored text', () => {
    expect(formOf(undefined, ['llm_model']).description).toBe('');
    expect(formOf(STORED, ['llm_model']).description).toBe('Fast for everyday tasks');
  });

  it('writes the trimmed description over the stored one', () => {
    const form = { ...formOf(STORED, ['llm_model']), description: '  Best for coding and agents ' };
    expect(modelDraftOf(form, STORED.data).data['description']).toBe('Best for coding and agents');
  });

  it('writes a cleared description as blank, so the stored text does not survive the merge', () => {
    const form = { ...formOf(STORED, ['llm_model']), description: '   ' };
    expect(modelDraftOf(form, STORED.data).data['description']).toBe('');
  });

  it('writes no description for a model kind that has none', () => {
    const form = { ...formOf(undefined, ['embedding_model']), type: 'embedding_model', description: 'ignored' };
    expect('description' in modelDraftOf(form, undefined).data).toBe(false);
  });
});
