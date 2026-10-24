import { describe, expect, it } from 'vitest';
import { http, HttpResponse } from 'msw';
import { z } from 'zod';

import { registerValidatedHandlers, type ValidatedHandlerEntry } from './register';

/**
 * R-M3 and fixture-metadata unit coverage for the registration-time validation wrapper.
 * The RED/GREEN fixture pair for these rules is driven by
 * scripts/check-gates-selftest.mjs; this suite pins the wrapper's behaviour
 * table so M1 can build on it.
 */

const userSchema = z.object({ id: z.number(), name: z.string() });

function entry(overrides: Partial<ValidatedHandlerEntry>): ValidatedHandlerEntry {
  return {
    id: 'users.get',
    handler: http.get('/api/v2/users/1', () => HttpResponse.json({ id: 1, name: 'Ada' })),
    schema: userSchema,
    fixture: { recordedAt: new Date().toISOString(), body: { id: 1, name: 'Ada' } },
    ...overrides,
  };
}

describe('registerValidatedHandlers', () => {
  it('returns the handlers when every fixture satisfies its schema', () => {
    const first = entry({});
    const handlers = registerValidatedHandlers([first]);
    expect(handlers).toEqual([first.handler]);
  });

  it('returns an empty list for an empty registry (Wave-0 state)', () => {
    expect(registerValidatedHandlers([])).toEqual([]);
  });

  it('throws R-M3 when a fixture body violates its schema', () => {
    const bad = entry({ fixture: { recordedAt: new Date().toISOString(), body: { id: 'not-a-number' } } });
    expect(() => registerValidatedHandlers([bad])).toThrow(/R-M3: handler "users\.get"/);
  });

  it('throws fixture-metadata when a fixture has no recordedAt', () => {
    const bad = entry({ fixture: { recordedAt: '', body: { id: 1, name: 'Ada' } } });
    expect(() => registerValidatedHandlers([bad])).toThrow(/fixture-metadata: handler "users\.get"/);
  });

  it('throws fixture-metadata when recordedAt is not a parseable date', () => {
    const bad = entry({ fixture: { recordedAt: 'yesterday-ish', body: { id: 1, name: 'Ada' } } });
    expect(() => registerValidatedHandlers([bad])).toThrow(/fixture-metadata/);
  });
});
