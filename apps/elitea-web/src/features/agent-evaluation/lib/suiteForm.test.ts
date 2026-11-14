import { describe, expect, it } from 'vitest';

import { EMPTY_SUITE_FORM, suiteFormError, suiteFormToInput, suiteToForm } from './suiteForm';

describe('suiteForm', () => {
  it('sends an empty limit as null, which is no limit', () => {
    const input = suiteFormToInput({ ...EMPTY_SUITE_FORM, name: ' Nightly ', tokenLimitPerRun: '50000' });
    expect(input).toEqual({
      name: 'Nightly',
      token_limit_per_case: null,
      cost_limit_per_case: null,
      token_limit_per_run: 50000,
      cost_limit_per_run: null,
      on_run_limit: 'stop',
    });
  });

  it('refuses what the server refuses, before the request', () => {
    const named = { ...EMPTY_SUITE_FORM, name: 'a' };
    expect(suiteFormError(EMPTY_SUITE_FORM)).toMatch(/name/);
    expect(suiteFormError({ ...named, tokenLimitPerRun: '0' })).toMatch(/token limit/);
    expect(suiteFormError({ ...named, tokenLimitPerCase: '1.5' })).toMatch(/token limit/);
    expect(suiteFormError({ ...named, tokenLimitPerRun: '3000000000' })).toMatch(/token limit/);
    expect(suiteFormError({ ...named, costLimitPerCase: '-1' })).toMatch(/cost limit/);
    expect(suiteFormError({ ...named, costLimitPerRun: 'abc' })).toMatch(/cost limit/);
    expect(suiteFormError({ ...named, costLimitPerRun: '0.00001' })).toMatch(/cost limit/);
    expect(suiteFormError({ ...named, costLimitPerRun: '0.0001' })).toBeUndefined();
    expect(suiteFormError({ ...named, tokenLimitPerRun: '1', costLimitPerRun: '0.5' })).toBeUndefined();
  });

  it('round-trips a stored suite into the form', () => {
    const form = suiteToForm({
      id: '4',
      name: 'Cheap',
      token_limit_per_case: 2000,
      cost_limit_per_case: null,
      token_limit_per_run: null,
      cost_limit_per_run: 1.5,
      on_run_limit: 'report',
    });
    expect(form).toEqual({
      name: 'Cheap',
      tokenLimitPerCase: '2000',
      costLimitPerCase: '',
      tokenLimitPerRun: '',
      costLimitPerRun: '1.5',
      onRunLimit: 'report',
    });
  });
});
