import { describe, expect, it } from 'vitest';

import { EMPTY_MEMORY_FILTERS, dayStart, filtersToParams } from './memoryFilters';

describe('filtersToParams', () => {
  it('sends only the limit when nothing is filtered', () => {
    expect(filtersToParams(EMPTY_MEMORY_FILTERS, 0)).toEqual({ limit: 50 });
  });

  it('maps every filter to its query parameter', () => {
    const params = filtersToParams(
      { query: '  short  ', kind: 'fact', status: 'pending', personId: '12', source: 'person', from: '2026-11-01', to: '2026-11-03' },
      100,
      200,
    );
    expect(params).toEqual({
      query: 'short',
      kind: 'fact',
      status: 'pending',
      subject_user_id: 12,
      source: 'person',
      created_after: new Date(2026, 10, 1).toISOString(),
      // "to" covers the whole day: the bound is the start of the next one.
      created_before: new Date(2026, 10, 4).toISOString(),
      limit: 200,
      offset: 100,
    });
  });

  it('drops a person id that is not a positive integer', () => {
    expect(filtersToParams({ ...EMPTY_MEMORY_FILTERS, personId: '0' }, 0)).toEqual({ limit: 50 });
    expect(filtersToParams({ ...EMPTY_MEMORY_FILTERS, personId: 'x' }, 0)).toEqual({ limit: 50 });
  });
});

describe('dayStart', () => {
  it('refuses anything but a calendar day', () => {
    expect(dayStart('')).toBeUndefined();
    expect(dayStart('2026-11')).toBeUndefined();
  });
});
