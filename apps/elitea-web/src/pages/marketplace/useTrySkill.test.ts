import { describe, expect, it } from 'vitest';

import { modelParticipant, resolveDefaultModel } from './useTrySkill';

describe('the model a Try starts on (ADR-0051 amendment 1, A1.3)', () => {
  it('is the one marked default, as the new-chat screen picks it', () => {
    expect(resolveDefaultModel([{ name: 'a', project_id: '1' }, { name: 'b', project_id: '1', default: true }])?.name).toBe('b');
  });

  it('is the first model when none is marked default, and nothing for a project with no model', () => {
    expect(resolveDefaultModel([{ name: 'a', project_id: '1' }, { name: 'b', project_id: '1' }])?.name).toBe('a');
    expect(resolveDefaultModel([])).toBeUndefined();
  });

  it('is a model participant alone: no toolkit, no tool, the model\'s own project', () => {
    expect(modelParticipant({ name: 'shared-model', project_id: '1' })).toEqual({
      entity_name: 'llm',
      entity_meta: { model_name: 'shared-model' },
      entity_settings: { model_project_id: 1 },
    });
  });
});
