import { describe, expect, it } from 'vitest';

import { EMPTY_SUITE_FORM, suiteFormError, suiteFormToInput, suiteToForm } from './suiteForm';

describe('suiteForm', () => {
  it('sends an empty limit as null, which is no limit', () => {
    const input = suiteFormToInput({ ...EMPTY_SUITE_FORM, name: ' Nightly ', tokenLimitPerRun: '50000' });
    expect(input).toEqual({
      name: 'Nightly',
      execution_mode: 'predict_blocking',
      step_limit_per_case: null,
      time_limit_per_case_seconds: null,
      store_tool_outputs: true,
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
      executionMode: 'predict_blocking',
      stepLimitPerCase: '',
      timeLimitPerCaseSeconds: '',
      storeToolOutputs: true,
      tokenLimitPerCase: '2000',
      costLimitPerCase: '',
      tokenLimitPerRun: '',
      costLimitPerRun: '1.5',
      onRunLimit: 'report',
    });
  });

  // The controls mean something only for agent runs (issue 2013, step 2): a
  // predict_blocking suite never sends them, whatever the form still holds.
  it('sends the runtime controls only for the runtime_plane mode', () => {
    const named = { ...EMPTY_SUITE_FORM, name: 'a', stepLimitPerCase: '20', timeLimitPerCaseSeconds: '90', storeToolOutputs: false };
    expect(suiteFormToInput(named)).toMatchObject({
      execution_mode: 'predict_blocking',
      step_limit_per_case: null,
      time_limit_per_case_seconds: null,
      store_tool_outputs: true,
    });
    expect(suiteFormToInput({ ...named, executionMode: 'runtime_plane' })).toMatchObject({
      execution_mode: 'runtime_plane',
      step_limit_per_case: 20,
      time_limit_per_case_seconds: 90,
      store_tool_outputs: false,
    });
  });

  it('refuses a bad steps or time limit only when the suite runs agent runs', () => {
    const named = { ...EMPTY_SUITE_FORM, name: 'a', stepLimitPerCase: '0', timeLimitPerCaseSeconds: '5000' };
    expect(suiteFormError(named)).toBeUndefined();
    const runtime = { ...named, executionMode: 'runtime_plane' as const };
    expect(suiteFormError(runtime)).toMatch(/steps limit/);
    expect(suiteFormError({ ...runtime, stepLimitPerCase: '5' })).toMatch(/time limit/);
    expect(suiteFormError({ ...runtime, stepLimitPerCase: '5', timeLimitPerCaseSeconds: '90' })).toBeUndefined();
    expect(suiteFormError({ ...runtime, stepLimitPerCase: '', timeLimitPerCaseSeconds: '' })).toBeUndefined();
  });

  it('reads a stored runtime suite, and a suite stored before the fields as the default', () => {
    const stored = suiteToForm({
      id: '5',
      name: 'Agent',
      on_run_limit: 'stop',
      execution_mode: 'runtime_plane',
      step_limit_per_case: 12,
      time_limit_per_case_seconds: 60,
      store_tool_outputs: false,
    });
    expect(stored).toMatchObject({
      executionMode: 'runtime_plane',
      stepLimitPerCase: '12',
      timeLimitPerCaseSeconds: '60',
      storeToolOutputs: false,
    });
    expect(suiteToForm({ id: '6', name: 'Old', on_run_limit: 'stop' })).toMatchObject({
      executionMode: 'predict_blocking',
      storeToolOutputs: true,
    });
  });
});
