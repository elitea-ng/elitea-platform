import { describe, expect, it } from 'vitest';

import { EliteaApiError } from '@/shared/api/generated/mutator';

import { agentSaveErrorMessage } from './agentSaveErrorMessage';

describe('agentSaveErrorMessage', () => {
  function httpError(status: number, body: unknown): EliteaApiError {
    return new EliteaApiError({ kind: 'http', status, url: '/x', body });
  }

  it('shows the error text of a 422 refusal', () => {
    const refusal = { error: 'output_contract: the pattern is not valid RE2', code: 'OUTPUT_CONTRACT_INVALID', reason: 'pattern_invalid' };
    expect(agentSaveErrorMessage(httpError(422, refusal), 'Failed.')).toBe('output_contract: the pattern is not valid RE2');
  });

  it('keeps the fallback for any other failure', () => {
    expect(agentSaveErrorMessage(httpError(500, { error: 'boom' }), 'Failed.')).toBe('Failed.');
    expect(agentSaveErrorMessage(new Error('x'), 'Failed.')).toBe('Failed.');
  });
});
