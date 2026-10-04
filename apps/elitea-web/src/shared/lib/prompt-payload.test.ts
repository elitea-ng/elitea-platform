import { describe, expect, it } from 'vitest';

import { APPLICATION_PAYLOAD_KEY, PROMPT_PAYLOAD_KEY } from './prompt-payload';

describe('PROMPT_PAYLOAD_KEY', () => {
  it('preserves the exact field-name mapping (constants.js:1030-1056)', () => {
    expect(PROMPT_PAYLOAD_KEY.context).toBe('prompt');
    expect(PROMPT_PAYLOAD_KEY.modelName).toBe('model_name');
    expect(PROMPT_PAYLOAD_KEY.maxTokens).toBe('max_tokens');
    expect(PROMPT_PAYLOAD_KEY.reasoningEffort).toBe('reasoning_effort');
    expect(PROMPT_PAYLOAD_KEY.welcomeMessage).toBe('welcome_message');
    // 24: `webhookSecret` is gone. The agent webhook is the inbound trigger
    // (legacy issue 6656), not a model setting.
    expect(PROMPT_PAYLOAD_KEY).not.toHaveProperty('webhookSecret');
    expect(Object.keys(PROMPT_PAYLOAD_KEY)).toHaveLength(24);
  });
});

describe('APPLICATION_PAYLOAD_KEY', () => {
  it('preserves the exact field-name mapping (constants.js:126-132)', () => {
    expect(APPLICATION_PAYLOAD_KEY).toEqual({
      file: 'file',
      name: 'name',
      description: 'description',
      tags: 'tags',
      type: 'type',
    });
  });
});
