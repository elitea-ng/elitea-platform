import { describe, expect, it } from 'vitest';

import {
  NOTIFICATION_DEFAULT_PAGE_SIZE,
  NOTIFICATION_DEFAULT_SORT,
  NOTIFICATION_PAGE_SIZE_OPTIONS,
  apiSearchTerm,
  nextSort,
  pageRange,
} from './table';

describe('defaults', () => {
  it('matches the reference: 50 rows a page, newest first, 5/10/50/100', () => {
    // A live deployment's footer reads "Rows per page: 50".
    expect(NOTIFICATION_DEFAULT_PAGE_SIZE).toBe(50);
    expect(NOTIFICATION_DEFAULT_SORT).toEqual({ field: 'created_at', direction: 'desc' });
    expect([...NOTIFICATION_PAGE_SIZE_OPTIONS]).toEqual([5, 10, 50, 100]);
  });
});

describe('pageRange', () => {
  it('reads "1 - 1 of 1" for the single-row case the live page shows', () => {
    const range = pageRange(0, 50, 1);
    expect(range.startRow).toBe(1);
    expect(range.endRow).toBe(1);
    expect(range.isFirstPage).toBe(true);
    expect(range.isLastPage).toBe(true);
  });

  it('numbers the second page from the right row', () => {
    expect(pageRange(1, 10, 25)).toEqual({
      startRow: 11,
      endRow: 20,
      isFirstPage: false,
      isLastPage: false,
    });
  });

  it('clamps the last page to the row count instead of the page size', () => {
    const range = pageRange(2, 10, 25);
    expect(range.startRow).toBe(21);
    expect(range.endRow).toBe(25);
    expect(range.isLastPage).toBe(true);
  });

  it('does not claim a row on an empty list', () => {
    // `page * pageSize + 1` would say "1 - 0 of 0".
    expect(pageRange(0, 50, 0)).toEqual({
      startRow: 0,
      endRow: 0,
      isFirstPage: true,
      isLastPage: true,
    });
  });

  it('treats an exactly-full last page as the last page', () => {
    expect(pageRange(1, 10, 20).isLastPage).toBe(true);
    expect(pageRange(0, 10, 20).isLastPage).toBe(false);
  });

  it('survives a zero page size rather than dividing by it', () => {
    expect(() => pageRange(0, 0, 10)).not.toThrow();
  });
});

describe('nextSort', () => {
  it('starts a NEW column ascending, whatever the previous column was doing', () => {
    // The bug this guards: inheriting the old direction, so clicking "Type"
    // while "Date & Time" is descending sorts Type descending on first click.
    expect(nextSort({ field: 'created_at', direction: 'desc' }, 'event_type')).toEqual({
      field: 'event_type',
      direction: 'asc',
    });
  });

  it('flips only the column already sorted', () => {
    expect(nextSort({ field: 'created_at', direction: 'desc' }, 'created_at')).toEqual({
      field: 'created_at',
      direction: 'asc',
    });
    expect(nextSort({ field: 'created_at', direction: 'asc' }, 'created_at')).toEqual({
      field: 'created_at',
      direction: 'desc',
    });
  });
});

describe('apiSearchTerm', () => {
  it('sends nothing until two characters are typed', () => {
    expect(apiSearchTerm('')).toBe('');
    expect(apiSearchTerm('a')).toBe('');
    expect(apiSearchTerm('ab')).toBe('ab');
    expect(apiSearchTerm('abc')).toBe('abc');
  });
});
