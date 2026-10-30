import { describe, expect, it } from 'vitest';

import { EliteaApiError } from '@/shared/api/generated/mutator';

import {
  areExtraFieldsEqual,
  mapCreateErrorToFieldErrors,
  type CreateAgentFormExtraFields,
} from './createApplicationDraftState';

const BASE: CreateAgentFormExtraFields = {
  instructions: 'Be helpful.',
  welcomeMessage: 'Hi',
  variables: [{ name: 'a', value: '1' }],
  stepLimit: 25,
  llmSettings: undefined,
  outputContract: null,
};

function httpError(body: unknown): EliteaApiError {
  return new EliteaApiError({ kind: 'http', status: 400, body } as never);
}

describe('areExtraFieldsEqual', () => {
  it('is true for a distinct object with the same values', () => {
    expect(areExtraFieldsEqual(BASE, { ...BASE, variables: [{ name: 'a', value: '1' }] })).toBe(true);
  });

  it.each<[string, Partial<CreateAgentFormExtraFields>]>([
    ['instructions', { instructions: 'Something else' }],
    ['welcomeMessage', { welcomeMessage: 'Hello' }],
    ['stepLimit', { stepLimit: 7 }],
    ['variables', { variables: [{ name: 'a', value: '2' }] }],
  ])('sees a changed %s', (_field, patch) => {
    expect(areExtraFieldsEqual(BASE, { ...BASE, ...patch })).toBe(false);
  });

  /*
   * The nav-blocker half of the model picker (#133). A comparison that
   * skipped this field would report "unchanged" after the user had pointed
   * the agent at a different model, and navigating away would discard it with
   * no prompt.
   */
  it('sees a model picked where there was none', () => {
    const picked = { ...BASE, llmSettings: { model_name: 'qwen3.5', model_project_id: 9, max_tokens: -1 } };
    expect(areExtraFieldsEqual(BASE, picked)).toBe(false);
  });

  it('does not report a re-emitted identical model as a change', () => {
    // The picker hands back a fresh object every time, so an identity check
    // here would keep the guard armed forever.
    const settings = { model_name: 'qwen3.5', model_project_id: 9, max_tokens: -1 };
    expect(areExtraFieldsEqual({ ...BASE, llmSettings: settings }, { ...BASE, llmSettings: { ...settings } })).toBe(true);
  });

  it('sees a change of temperature alone', () => {
    const a = { ...BASE, llmSettings: { model_name: 'qwen3.5', model_project_id: 9, max_tokens: -1, temperature: 0.6 } };
    const b = { ...BASE, llmSettings: { model_name: 'qwen3.5', model_project_id: 9, max_tokens: -1, temperature: 0.2 } };
    expect(areExtraFieldsEqual(a, b)).toBe(false);
  });
});

describe('mapCreateErrorToFieldErrors', () => {
  it('attributes a FastAPI-shaped entry to its field', () => {
    expect(mapCreateErrorToFieldErrors(httpError([{ loc: ['body', 'name'], msg: 'Already taken' }]))).toEqual({
      name: 'Already taken',
    });
  });

  it('returns nothing for a body that is not the validation-array shape', () => {
    expect(mapCreateErrorToFieldErrors(httpError({ error: 'boom' }))).toEqual({});
  });

  it('returns nothing for something that is not an API error at all', () => {
    expect(mapCreateErrorToFieldErrors(new Error('offline'))).toEqual({});
  });
});
