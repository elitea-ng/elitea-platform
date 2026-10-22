import { describe, expect, it } from 'vitest';

import { readIndexesListErrorMessage, readIndexesListErrorStatus } from './indexesListError';

/**
 * The rejection `useIndexesListQuery` really produces: an `EliteaApiError`
 * (`shared/api/generated/mutator.ts:110`) carrying `HttpFailure`
 * (`shared/api/http.ts:45-49`). Built by hand rather than imported so this
 * helper's structural matching is tested for what it is — a shape contract,
 * not an `instanceof` check.
 */
function httpFailure(status: number, body: unknown): Error & { readonly failure: unknown } {
  return Object.assign(new Error(`eliteaFetch: ${String(status)} from /x`), {
    failure: { kind: 'http', status, url: '/x', body },
  });
}

/**
 * A 400 `writeError` (`services/elitea-main/internal/api/v2/indexing/
 * index_meta.go`) really writes for `GET .../index_meta/...`, with the
 * sentence at `body.error`. (The 2026-09-06 parity walk recorded a different
 * 400 — a missing per-project PgVector configuration — which no longer
 * exists: indexes need no vector store configuration.)
 */
const INDEX_META_400 = httpFailure(400, { error: 'Toolkit id is missing for toolkit 1' });

describe('readIndexesListErrorMessage', () => {
  it('returns the server sentence from an index-meta 400', () => {
    expect(readIndexesListErrorMessage(INDEX_META_400)).toBe('Toolkit id is missing for toolkit 1');
  });

  it.each([
    ['message', { message: 'Toolkit id is missing for toolkit 9' }, 'Toolkit id is missing for toolkit 9'],
    ['detail', { detail: 'Invalid index metadata request' }, 'Invalid index metadata request'],
  ])('reads the sentence from body.%s too', (_key, body, expected) => {
    expect(readIndexesListErrorMessage(httpFailure(400, body))).toBe(expected);
  });

  it('prefers `error` when a body carries more than one of the three keys', () => {
    expect(readIndexesListErrorMessage(httpFailure(400, { error: 'first', message: 'second', detail: 'third' }))).toBe('first');
  });

  it('reads a plain-text body, trimmed — a proxy error page is still a message', () => {
    expect(readIndexesListErrorMessage(httpFailure(502, '  Bad Gateway\n'))).toBe('Bad Gateway');
  });

  it.each([
    ['an empty string body', httpFailure(500, '   ')],
    ['a whitespace-only sentence', httpFailure(500, { error: '  ' })],
    ['a non-string sentence', httpFailure(500, { error: { nested: true } })],
    ['no body at all', httpFailure(500, undefined)],
    ['a null body', httpFailure(500, null)],
    ['an array body', httpFailure(500, ['a'])],
  ])('returns undefined for %s, so the generic heading is used', (_name, error) => {
    expect(readIndexesListErrorMessage(error)).toBeUndefined();
  });

  it.each([
    ['an auth failure, which carries no body by design', Object.assign(new Error('auth'), { failure: { kind: 'auth', status: 403, url: '/x' } })],
    ['a network failure', Object.assign(new Error('net'), { failure: { kind: 'network', url: '/x', message: 'boom', cause: null } })],
    ['an aborted request', Object.assign(new Error('abort'), { failure: { kind: 'aborted', url: '/x' } })],
    ['a plain Error with no failure', new Error('plain')],
    ['a string rejection', 'nope'],
    ['null', null],
    ['undefined', undefined],
    ['an object whose failure is not an object', { failure: 'http' }],
  ])('returns undefined for %s', (_name, error) => {
    expect(readIndexesListErrorMessage(error)).toBeUndefined();
  });
});

describe('readIndexesListErrorStatus', () => {
  it('returns the status of an http failure', () => {
    expect(readIndexesListErrorStatus(INDEX_META_400)).toBe(400);
  });

  it.each([
    ['a non-http failure', Object.assign(new Error('a'), { failure: { kind: 'auth', status: 403, url: '/x' } })],
    ['a non-numeric status', Object.assign(new Error('a'), { failure: { kind: 'http', status: '400', url: '/x', body: {} } })],
    ['a plain Error', new Error('plain')],
    ['undefined', undefined],
  ])('returns undefined for %s', (_name, error) => {
    expect(readIndexesListErrorStatus(error)).toBeUndefined();
  });
});
