import { describe, expect, it } from 'vitest';

import { EliteaApiError } from '@/shared/api/generated/mutator';

import {
  applicationErrorMessage,
  applicationErrorMessageOrFallback,
  applicationServerErrorMessage,
} from './errorMessage';

describe('applicationErrorMessage', () => {
  it('returns the message of an Error instance', () => {
    expect(applicationErrorMessage(new Error('boom'))).toBe('boom');
  });

  it('returns a subclassed Error message (e.g. EliteaApiError) unchanged', () => {
    class FakeEliteaApiError extends Error {}
    expect(applicationErrorMessage(new FakeEliteaApiError('eliteaFetch: 400 from /x'))).toBe(
      'eliteaFetch: 400 from /x',
    );
  });

  it('stringifies a non-Error value', () => {
    expect(applicationErrorMessage('plain string')).toBe('plain string');
    expect(applicationErrorMessage(404)).toBe('404');
    expect(applicationErrorMessage(undefined)).toBe('undefined');
    expect(applicationErrorMessage(null)).toBe('null');
  });

  // A1-application-chat cluster, finding 3: this is exactly the shape that degrades to the
  // literal "[object Object]" — documented here (not "fixed" here) because `applicationErrorMessage`
  // itself is deliberately a plain stringify-everything helper; `applicationErrorMessageOrFallback`
  // below is the one that actually avoids leaking this literal to the user.
  it('degrades a plain object to the literal "[object Object]"', () => {
    expect(applicationErrorMessage({ some: 'shape' })).toBe('[object Object]');
  });
});

describe('applicationErrorMessageOrFallback', () => {
  it('returns the Error message when the error is a real Error instance', () => {
    expect(applicationErrorMessageOrFallback(new Error('boom'), 'fallback')).toBe('boom');
  });

  it('returns the string as-is when the error is a plain string', () => {
    expect(applicationErrorMessageOrFallback('raw string error', 'fallback')).toBe('raw string error');
  });

  it('falls back to the friendly message for a plain-object rejection instead of "[object Object]"', () => {
    expect(applicationErrorMessageOrFallback({ some: 'shape' }, 'Failed to delete the message, please try again.')).toBe(
      'Failed to delete the message, please try again.',
    );
  });

  it('falls back for undefined/null/number rejections too', () => {
    expect(applicationErrorMessageOrFallback(undefined, 'fallback')).toBe('fallback');
    expect(applicationErrorMessageOrFallback(null, 'fallback')).toBe('fallback');
    expect(applicationErrorMessageOrFallback(404, 'fallback')).toBe('fallback');
  });

  it('falls back when the Error message itself is empty', () => {
    expect(applicationErrorMessageOrFallback(new Error(''), 'fallback')).toBe('fallback');
  });

  it('falls back when the string error itself is empty', () => {
    expect(applicationErrorMessageOrFallback('', 'fallback')).toBe('fallback');
  });
});

/**
 * #147. The server explains a refusal in the response BODY. `EliteaApiError`'s
 * own `message` is the `eliteaFetch: <status> from <url>` diagnostic that
 * `mutator.ts`'s `describeFailure` builds, and showing that to a user says
 * nothing. These cases pin which of the two a caller gets.
 */
describe('applicationServerErrorMessage', () => {
  function httpError(status: number, body: unknown): EliteaApiError {
    return new EliteaApiError({ kind: 'http', status, url: '/api/v2/elitea_core/version/x', body });
  }

  it('returns the server’s own `error` field, not the eliteaFetch diagnostic', () => {
    const error = httpError(400, { error: 'Unpublish first. Cannot delete a published version.' });

    expect(error.message).toContain('eliteaFetch: 400');
    expect(applicationServerErrorMessage(error, 'fallback')).toBe(
      'Unpublish first. Cannot delete a published version.',
    );
  });

  it('accepts `message` as the body field too', () => {
    expect(applicationServerErrorMessage(httpError(409, { message: 'in use' }), 'fallback')).toBe('in use');
  });

  it('accepts a plain-text body', () => {
    expect(applicationServerErrorMessage(httpError(500, 'gateway said no'), 'fallback')).toBe('gateway said no');
  });

  it('falls back when the body explains nothing, rather than leaking the URL', () => {
    for (const body of [undefined, null, {}, { error: '' }, 42, '']) {
      expect(applicationServerErrorMessage(httpError(400, body), 'fallback')).toBe('fallback');
    }
  });

  it('falls back for a non-http failure, which carries no body at all', () => {
    const network = new EliteaApiError({ kind: 'network', url: '/x', message: 'offline', cause: undefined });
    expect(applicationServerErrorMessage(network, 'fallback')).toBe('fallback');
  });

  it('still reports an ordinary Error’s own message', () => {
    expect(applicationServerErrorMessage(new Error('boom'), 'fallback')).toBe('boom');
    expect(applicationServerErrorMessage({ some: 'shape' }, 'fallback')).toBe('fallback');
  });
});
