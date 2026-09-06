/**
 * Pure state maths for the Notifications table — the parts that decide what
 * the footer says and which way a column sorts.
 *
 * Kept out of the component on purpose: every one of these is an off-by-one
 * or an inverted-ternary waiting to happen ("1 - 0 of 0", a Next button live
 * on the last page, a first click that sorts descending), and none of them
 * needs a DOM to check.
 *
 * Ported from `EliteaUI/src/pages/NotificationCenter/NotificationTable.jsx`
 * (`totalPages`/`isFirstPage`/`isLastPage`/`startRow`/`endRow`,
 * `handleSort`).
 */

type SortOrder = 'asc' | 'desc';

/** The two sortable columns of the reference's table. */
export type NotificationSortField = 'event_type' | 'created_at';

export interface NotificationSort {
  readonly field: NotificationSortField;
  readonly direction: SortOrder;
}

/** `NotificationTable.jsx:27-29`. */
export const NOTIFICATION_PAGE_SIZE_OPTIONS = [5, 10, 50, 100] as const;

/** `NotificationCenter.jsx:22-25` — the reference opens on 50 rows, newest first. */
export const NOTIFICATION_DEFAULT_PAGE_SIZE = 50;
export const NOTIFICATION_DEFAULT_SORT: NotificationSort = {
  field: 'created_at',
  direction: 'desc',
};

export interface PageRange {
  readonly startRow: number;
  readonly endRow: number;
  readonly isFirstPage: boolean;
  readonly isLastPage: boolean;
}

/**
 * The footer's "start - end of total" figures and the two arrow states, for a
 * zero-based `page`.
 *
 * `startRow` is 0 (not 1) on an EMPTY list: `page * pageSize + 1` would read
 * "1 - 0 of 0", which claims a row that is not there. The footer is hidden at
 * `total === 0` anyway, but a helper that returns a false number when its
 * caller happens not to render it is a trap for the next caller.
 */
export function pageRange(page: number, pageSize: number, total: number): PageRange {
  const safeSize = pageSize > 0 ? pageSize : 1;
  const totalPages = Math.max(1, Math.ceil(total / safeSize));
  return {
    startRow: total === 0 ? 0 : page * safeSize + 1,
    endRow: Math.min((page + 1) * safeSize, total),
    isFirstPage: page <= 0,
    isLastPage: page >= totalPages - 1,
  };
}

/**
 * What one click on a column header does.
 *
 * A NEW column starts ASCENDING, and only a repeat click on the column
 * already sorted flips it — `NotificationTable.jsx:112-121`. Clicking a
 * different column must not inherit the previous column's direction, which is
 * what a naive "toggle direction" implementation does.
 */
export function nextSort(current: NotificationSort, field: NotificationSortField): NotificationSort {
  if (current.field !== field) return { field, direction: 'asc' };
  return { field, direction: current.direction === 'asc' ? 'desc' : 'asc' };
}

/**
 * The reference's search gate (`NotificationCenter.jsx:16,34-37`): fewer than
 * two characters is sent as no search at all, so a single stray keystroke
 * does not empty the table.
 */
const NOTIFICATION_MIN_SEARCH_LENGTH = 2;

export function apiSearchTerm(search: string): string {
  return search.length < NOTIFICATION_MIN_SEARCH_LENGTH ? '' : search;
}
