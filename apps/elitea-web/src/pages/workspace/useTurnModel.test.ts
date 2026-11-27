import { describe, expect, it } from 'vitest';

import { agentModel, chatModels, resolveModelName } from './useTurnModel';

describe('useTurnModel helpers', () => {
  it('keeps the catalogue’s chat models only, the first of each name', () => {
    const body = {
      items: [
        { name: 'gpt-5', project_id: 42, supports_reasoning: true, description: 'Thinks' },
        { name: 'embed', project_id: 42, type: 'embedding_model' },
        { name: 'gpt-5', project_id: 1, supports_reasoning: false },
        { name: 'mini', project_id: 1, type: 'llm_model' },
        { name: '' },
      ],
    };
    expect(chatModels(body)).toEqual([
      { name: 'gpt-5', description: 'Thinks', supportsReasoning: true },
      { name: 'mini', description: undefined, supportsReasoning: false },
    ]);
    expect(chatModels(undefined)).toEqual([]);
    expect(chatModels({ data: { items: [{ name: 'x' }] }, status: 200, headers: new Headers() })).toEqual([{ name: 'x', description: undefined, supportsReasoning: false }]);
  });

  it('reads the agent version’s own model, effort and pin', () => {
    expect(agentModel({ llm_settings: { model_name: 'gpt-4o', reasoning_effort: 'low', pin_model: true } })).toEqual({ name: 'gpt-4o', effort: 'low', pinned: true });
    expect(agentModel({ llm_settings: { model_name: '', reasoning_effort: 'extreme' } })).toEqual({ name: null, effort: undefined, pinned: false });
    expect(agentModel(undefined)).toEqual({ name: null, effort: undefined, pinned: false });
  });

  it('keeps a stored auto and off, and reads none as off (ADR-0038)', () => {
    expect(agentModel({ llm_settings: { model_name: 'qwen3.5', reasoning_effort: 'auto' } }).effort).toBe('auto');
    expect(agentModel({ llm_settings: { model_name: 'qwen3.5', reasoning_effort: 'off' } }).effort).toBe('off');
    expect(agentModel({ llm_settings: { model_name: 'qwen3.5', reasoning_effort: 'none' } }).effort).toBe('off');
  });

  it('matches a /model name to the catalogue’s spelling, any case, and refuses unknown names and an unloaded list', () => {
    const models = [{ name: 'GPT-5', supportsReasoning: true }];
    expect(resolveModelName(models, 'gpt-5')).toEqual({ name: 'GPT-5' });
    expect(resolveModelName(models, 'gpt')).toEqual({ notice: 'This project has no chat model named “gpt”.' });
    expect(resolveModelName(undefined, 'GPT-5')).toEqual({ notice: 'The models are still loading. Try again in a moment.' });
  });
});
