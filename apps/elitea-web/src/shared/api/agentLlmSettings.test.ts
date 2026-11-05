import { describe, expect, it } from 'vitest';

import {
  areAgentLlmSettingsEqual,
  toAgentLlmSettings,
  toLlmSettingsBody,
  type AgentLlmSettings,
} from './agentLlmSettings';

const PICKED: AgentLlmSettings = {
  model_name: 'qwen3.5',
  model_project_id: 17,
  max_tokens: -1,
  temperature: 0.6,
};

describe('toAgentLlmSettings', () => {
  it('reads a complete stored blob back', () => {
    expect(toAgentLlmSettings({ model_name: 'qwen3.5', model_project_id: 17, max_tokens: 4096, temperature: 0.2 })).toEqual(
      { model_name: 'qwen3.5', model_project_id: 17, max_tokens: 4096, temperature: 0.2 },
    );
  });

  // The export/import path normalises the id to a numeric string, so a stored
  // version can legitimately hold one.
  it('coerces a stringified model_project_id to a number', () => {
    const settings = toAgentLlmSettings({ model_name: 'qwen3.5', model_project_id: '17' });
    expect(settings?.model_project_id).toBe(17);
    expect(typeof settings?.model_project_id).toBe('number');
  });

  // Every version written before the model picker existed stores `{}`, and it
  // is the platform's catalogue-default fallback that makes those agents
  // answer turns. Returning a fabricated object here would replace a working
  // fallback with a guess.
  it('returns undefined for the empty object every pre-feature version stores', () => {
    expect(toAgentLlmSettings({})).toBeUndefined();
  });

  it('returns undefined for a non-object, null, or undefined', () => {
    expect(toAgentLlmSettings(undefined)).toBeUndefined();
    expect(toAgentLlmSettings(null)).toBeUndefined();
    expect(toAgentLlmSettings('qwen3.5')).toBeUndefined();
  });

  // The worker's `positive_u32` hard-fails without a project id, so a blob
  // that names only a model is not a profile this app may author.
  it('returns undefined when the model name or the project id is missing', () => {
    expect(toAgentLlmSettings({ model_name: 'qwen3.5' })).toBeUndefined();
    expect(toAgentLlmSettings({ model_project_id: 17 })).toBeUndefined();
    expect(toAgentLlmSettings({ model_name: '', model_project_id: 17 })).toBeUndefined();
  });

  it('defaults max_tokens to -1 (Auto) when the blob carries none', () => {
    expect(toAgentLlmSettings({ model_name: 'qwen3.5', model_project_id: 17 })?.max_tokens).toBe(-1);
  });

  // `top_p` is in the OpenAPI schema but not in the worker's allow-list, and
  // `openai_compatible` is re-derived server-side; carrying either forward
  // turns a green save into a refused turn.
  it('drops keys outside the worker allow-list rather than carrying them forward', () => {
    const settings = toAgentLlmSettings({
      model_name: 'qwen3.5',
      model_project_id: 17,
      top_p: 0.9,
      openai_compatible: true,
    });
    expect(settings).toEqual({ model_name: 'qwen3.5', model_project_id: 17, max_tokens: -1 });
    expect(settings).not.toHaveProperty('top_p');
    expect(settings).not.toHaveProperty('openai_compatible');
  });

  it('omits temperature entirely rather than setting it undefined', () => {
    const settings = toAgentLlmSettings({ model_name: 'qwen3.5', model_project_id: 17 });
    expect(Object.hasOwn(settings ?? {}, 'temperature')).toBe(false);
  });

  /*
   * #611-review-3. This read is what seeds every edit
   * (`pages/agents/lib/editApplicationMappers.ts`), and what it drops is
   * written back dropped on the NEXT save of any field at all. The worker
   * honours `reasoning_effort` (`ModelFieldNames::APPLICATION`), so losing it
   * here silently re-authored the version.
   */
  it('reads reasoning_effort back rather than stripping it', () => {
    const settings = toAgentLlmSettings({
      model_name: 'o3-mini',
      model_project_id: 17,
      max_tokens: -1,
      reasoning_effort: 'high',
    });
    expect(settings?.reasoning_effort).toBe('high');
  });

  it('drops a reasoning_effort outside the four the worker parses', () => {
    const settings = toAgentLlmSettings({ model_name: 'o3-mini', model_project_id: 17, reasoning_effort: 'extreme' });
    expect(Object.hasOwn(settings ?? {}, 'reasoning_effort')).toBe(false);
  });

  it('drops a null reasoning_effort rather than carrying it', () => {
    const settings = toAgentLlmSettings({ model_name: 'o3-mini', model_project_id: 17, reasoning_effort: null });
    expect(Object.hasOwn(settings ?? {}, 'reasoning_effort')).toBe(false);
  });

  /*
   * `validate_model` returns `invalid_profile` for `temperature` alongside a
   * reasoning_effort that is not `"none"`. A stored blob carrying both is
   * already refused, and re-sending it keeps it refused.
   */
  it('never returns both temperature and a real reasoning_effort', () => {
    const settings = toAgentLlmSettings({
      model_name: 'o3-mini',
      model_project_id: 17,
      temperature: 0.6,
      reasoning_effort: 'medium',
    });
    expect(settings?.reasoning_effort).toBe('medium');
    expect(Object.hasOwn(settings ?? {}, 'temperature')).toBe(false);
  });

  it('keeps temperature alongside reasoning_effort "none", which the worker allows', () => {
    const settings = toAgentLlmSettings({
      model_name: 'gpt-4o',
      model_project_id: 17,
      temperature: 0.6,
      reasoning_effort: 'none',
    });
    expect(settings?.temperature).toBe(0.6);
    expect(settings?.reasoning_effort).toBe('none');
  });

  it('keeps auto, which the server gives a new agent, through a read and a save (ADR-0038)', () => {
    const settings = toAgentLlmSettings({
      model_name: 'qwen3.5',
      model_project_id: 17,
      temperature: 0.6,
      reasoning_effort: 'auto',
    });
    expect(settings?.reasoning_effort).toBe('auto');
    expect(Object.hasOwn(settings ?? {}, 'temperature')).toBe(false);
    expect(settings ? toLlmSettingsBody(settings).reasoning_effort : undefined).toBe('auto');
  });

  it('keeps temperature alongside reasoning_effort "off", like "none"', () => {
    const settings = toAgentLlmSettings({
      model_name: 'qwen3.5',
      model_project_id: 17,
      temperature: 0.6,
      reasoning_effort: 'off',
    });
    expect(settings?.temperature).toBe(0.6);
    expect(settings?.reasoning_effort).toBe('off');
  });
});

describe('toLlmSettingsBody', () => {
  it('sends model_project_id as a JSON number', () => {
    expect(toLlmSettingsBody(PICKED).model_project_id).toBe(17);
    expect(JSON.parse(JSON.stringify(toLlmSettingsBody(PICKED)))).toEqual({
      model_name: 'qwen3.5',
      model_project_id: 17,
      max_tokens: -1,
      temperature: 0.6,
    });
  });

  it('omits temperature when the settings carry none', () => {
    const body = toLlmSettingsBody({ model_name: 'qwen3.5', model_project_id: 17, max_tokens: -1 });
    expect(Object.hasOwn(body, 'temperature')).toBe(false);
  });

  it('sends reasoning_effort to the wire', () => {
    const body = toLlmSettingsBody({
      model_name: 'o3-mini',
      model_project_id: 17,
      max_tokens: -1,
      reasoning_effort: 'high',
    });
    expect(JSON.parse(JSON.stringify(body))).toEqual({
      model_name: 'o3-mini',
      model_project_id: 17,
      max_tokens: -1,
      reasoning_effort: 'high',
    });
  });

  /* The pair the worker refuses can never leave this function, however it was assembled. */
  it('never sends both temperature and a real reasoning_effort', () => {
    const body = toLlmSettingsBody({
      model_name: 'o3-mini',
      model_project_id: 17,
      max_tokens: -1,
      temperature: 0.6,
      reasoning_effort: 'low',
    });
    expect(Object.hasOwn(body, 'temperature')).toBe(false);
    expect(body.reasoning_effort).toBe('low');
  });
});

describe('areAgentLlmSettingsEqual', () => {
  it('compares key by key, not by identity', () => {
    expect(areAgentLlmSettingsEqual(PICKED, { ...PICKED })).toBe(true);
  });

  it('sees a changed model, temperature, or max_tokens', () => {
    expect(areAgentLlmSettingsEqual(PICKED, { ...PICKED, model_name: 'gpt-4o' })).toBe(false);
    expect(areAgentLlmSettingsEqual(PICKED, { ...PICKED, model_project_id: 18 })).toBe(false);
    expect(areAgentLlmSettingsEqual(PICKED, { ...PICKED, temperature: 0.9 })).toBe(false);
    expect(areAgentLlmSettingsEqual(PICKED, { ...PICKED, max_tokens: 4096 })).toBe(false);
  });

  /* Or the nav blocker cannot see a reasoning-effort change and lets it be discarded (#133's shape). */
  it('sees a changed reasoning_effort', () => {
    const reasoning: AgentLlmSettings = { model_name: 'o3-mini', model_project_id: 17, max_tokens: -1, reasoning_effort: 'low' };
    expect(areAgentLlmSettingsEqual(reasoning, { ...reasoning, reasoning_effort: 'high' })).toBe(false);
    expect(areAgentLlmSettingsEqual(reasoning, { ...reasoning })).toBe(true);
  });

  it('treats picking a first model as a change', () => {
    expect(areAgentLlmSettingsEqual(undefined, PICKED)).toBe(false);
    expect(areAgentLlmSettingsEqual(PICKED, undefined)).toBe(false);
    expect(areAgentLlmSettingsEqual(undefined, undefined)).toBe(true);
  });
});
