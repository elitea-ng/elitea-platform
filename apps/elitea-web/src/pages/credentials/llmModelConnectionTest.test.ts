import { describe, expect, it } from 'vitest';

import { clearsTestResult, modelTestBody } from './llmModelConnectionTest';

describe('modelTestBody', () => {
  const form = { name: 'gpt-5', ai_credentials: { elitea_title: 'mine', private: true } };

  it('sends the form alone on the create screen', () => {
    expect(modelTestBody(form, undefined)).toEqual(form);
    expect(modelTestBody(form, '')).toEqual(form);
  });

  it('names the saved row on the edit screen and leaves the form object unchanged', () => {
    expect(modelTestBody(form, '11')).toEqual({ ...form, configuration_id: '11' });
    expect(form).not.toHaveProperty('configuration_id');
  });
});

describe('clearsTestResult', () => {
  it('clears on the model name, the credentials and the DIAL protocol only', () => {
    expect(clearsTestResult('llm_model', 'name')).toBe(true);
    expect(clearsTestResult('llm_model', 'ai_credentials')).toBe(true);
    // The protocol selects the route the gateway tests the model on.
    expect(clearsTestResult('llm_model', 'dial_protocol')).toBe(true);
    expect(clearsTestResult('llm_model', 'description')).toBe(false);
    // No such field exists on the llm_model form yet.
    expect(clearsTestResult('llm_model', 'api_protocol')).toBe(false);
    expect(clearsTestResult('open_ai', 'name')).toBe(false);
  });
});
