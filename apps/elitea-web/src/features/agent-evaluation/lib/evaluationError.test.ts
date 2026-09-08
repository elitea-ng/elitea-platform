import { describe, expect, it } from 'vitest';

import { EliteaApiError } from '@/shared/api/generated/mutator';

import { datasetErrorMessage, runErrorMessage } from './evaluationError';

/**
 * THE DEFECT THIS FILE PINS. `EliteaApiError.message` is a TRANSPORT
 * description — "eliteaFetch: 409 from …" — and the server's own text is one
 * level down on `failure.body`. A handler that showed `error.message` compiles,
 * looks correct, and throws away the only part of a refusal a person can act
 * on. It was written that way first here, and the dataset UI test caught it.
 */
describe('evaluationErrorMessage', () => {
  function httpError(status: number, body: unknown): EliteaApiError {
    return new EliteaApiError({ kind: 'http', status, url: '/x', body });
  }

  it('recovers the server’s message from the response body, not from the transport', () => {
    const error = httpError(409, {
      error: 'a dataset holds at most 10 cases: a run spends two model calls per case',
    });
    // The transport message is what a naive handler would have shown.
    expect(error.message).toContain('eliteaFetch: 409');
    expect(datasetErrorMessage(error)).toContain('at most 10 cases');
  });

  it('recovers the 501 reason a run start answers', () => {
    const error = httpError(501, {
      error: 'dimension JSON shape cannot be scored by this release: the code engine needs a sandbox',
    });
    expect(runErrorMessage(error)).toContain('needs a sandbox');
  });

  it('falls back when the body carries no message', () => {
    expect(datasetErrorMessage(httpError(500, null))).toBe('Failed to save the dataset.');
    expect(datasetErrorMessage(httpError(500, {}))).toBe('Failed to save the dataset.');
    expect(datasetErrorMessage(httpError(500, { error: '' }))).toBe('Failed to save the dataset.');
    expect(runErrorMessage(httpError(500, 'not an object'))).toBe('Failed to start the run.');
  });

  /*
   * A NETWORK failure has no server body to recover, and its own message is the
   * most specific thing there is. Replacing it with "Failed to save" would tell
   * a person their input was wrong when the request never arrived.
   */
  it('keeps a network failure’s own description', () => {
    const error = new EliteaApiError({
      kind: 'network',
      url: '/x',
      message: 'connection refused',
      cause: undefined,
    });
    expect(datasetErrorMessage(error)).toContain('connection refused');
  });

  it('answers nothing when there is no error', () => {
    expect(datasetErrorMessage(null)).toBeUndefined();
    expect(datasetErrorMessage(undefined)).toBeUndefined();
    expect(runErrorMessage(undefined)).toBeUndefined();
  });

  it('falls back for a thrown value that is not an Error at all', () => {
    expect(datasetErrorMessage('a bare string')).toBe('Failed to save the dataset.');
  });
});
