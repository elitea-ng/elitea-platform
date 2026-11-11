import { describe, expect, it } from 'vitest';

import { EliteaApiError } from './generated/mutator';
import { serverErrorMessage, serverErrorOf, serverErrorSentence } from './serverErrorMessage';

const url = 'http://localhost/api/v2/elitea_core/agent_teams/prompt_lib/1';
const refusal = (body: unknown, status = 400): EliteaApiError => new EliteaApiError({ kind: 'http', status, url, body });

describe('serverErrorMessage (issue 1848: one reader for every feature)', () => {
  it("reads the server's own {\"error\": ...} body", () => {
    expect(serverErrorMessage(refusal({ error: 'content is required', code: 'x' }), 'fallback')).toBe('content is required');
  });

  it('falls back to {"message": ...} when "error" is absent or empty', () => {
    expect(serverErrorMessage(refusal({ message: 'too long' }), 'fallback')).toBe('too long');
    expect(serverErrorMessage(refusal({ error: '', message: 'too long' }), 'fallback')).toBe('too long');
  });

  it('reads a plain-text body, trimmed', () => {
    expect(serverErrorMessage(refusal('memory not found', 404), 'fallback')).toBe('memory not found');
  });

  it("keeps the caller's sentence over a stock status line", () => {
    expect(serverErrorMessage(refusal('404 page not found\n', 404), 'fallback')).toBe('fallback');
    expect(serverErrorMessage(refusal('Forbidden\n', 403), 'fallback')).toBe('fallback');
    expect(serverErrorMessage(refusal('Internal Server Error', 500), 'fallback')).toBe('fallback');
  });

  it('never shows markup (a proxy error page)', () => {
    expect(serverErrorMessage(refusal('<html><body>502 Bad Gateway</body></html>', 502), 'fallback')).toBe('fallback');
  });

  it('uses the fallback for a non-EliteaApiError, never its message', () => {
    expect(serverErrorMessage(new Error('network down'), 'fallback')).toBe('fallback');
    expect(serverErrorMessage(undefined, 'fallback')).toBe('fallback');
  });

  it('uses the fallback when the body carries no usable sentence', () => {
    for (const body of [{}, '', '  ', null, 42, { error: 7 }, { error: '' }]) {
      expect(serverErrorMessage(refusal(body, 500), 'fallback')).toBe('fallback');
    }
  });

  it('uses the fallback for a network failure', () => {
    expect(serverErrorMessage(new EliteaApiError({ kind: 'network', url, message: 'offline', cause: undefined }), 'fallback')).toBe('fallback');
  });
});

describe('serverErrorOf / serverErrorSentence', () => {
  it('is undefined when the server said nothing', () => {
    expect(serverErrorOf(new Error('x'))).toBeUndefined();
    expect(serverErrorOf(refusal({}))).toBeUndefined();
    expect(serverErrorSentence(undefined)).toBeUndefined();
  });

  it('reads the body the same way the message does', () => {
    expect(serverErrorOf(refusal({ error: 'Only a manager may pin.' }, 403))).toBe('Only a manager may pin.');
    expect(serverErrorSentence({ message: 'm' })).toBe('m');
  });
});
