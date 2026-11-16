import { useCallback, type ReactNode } from 'react';

import { useNavigate, useSearch } from '@tanstack/react-router';

import type { PageToolbarPrimaryAction } from './PageToolbar';

/**
 * One filter a list page offers (ADR-0051): a URL search param with the
 * values that occur and how many rows each holds. Only values with rows are
 * offered, so a status nobody has set never appears as an empty filter.
 */
export interface ListFacet {
  /** The URL search param the choice lives in: `statuses`, `origin`. */
  readonly param: string;
  /** The param holds a list (`statuses`) and not one string. */
  readonly array?: boolean;
  readonly label: string;
  /** What the summary bar's chip says before the value: "Status". */
  readonly chipLabel: string;
  readonly options: readonly { readonly value: string; readonly label: string; readonly count: number }[];
}

/** One chip the page adds to the summary bar (the tag filter, a toolkit type). */
export interface ListFilterChip {
  readonly id: string;
  readonly label: string;
  readonly onRemove: () => void;
}

/** A page's modes that are not filters: Agents | Teams, Skills | Public. */
interface ListMode {
  readonly value: string;
  readonly label: string;
  readonly testId: string;
}

export interface EntityListSidebarProps {
  /** The page's name: the tab's own window (desktop) is titled "List — <subject>". */
  readonly subject: string;
  /** Prefix of every test id, singular: `agent`, `pipeline`. */
  readonly testIdPrefix: string;
  /** The `localStorage` bucket the card/table choice is remembered in. */
  readonly pageKey: string;
  readonly searchPlaceholder: string;
  /** Absent for a person who may not create. */
  readonly newAction?: PageToolbarPrimaryAction | undefined;
  /** Outlined buttons (Import…) after New. */
  readonly secondaryActions?: ReactNode;
  readonly modes?: { readonly items: readonly ListMode[]; readonly active: string; readonly onSelect: (value: string) => void } | undefined;
  readonly facets?: readonly ListFacet[];
  /** Sort fields (`sort_by`); absent where the page cannot sort. */
  readonly sortFields?: { readonly items: readonly ListSortField[]; readonly defaultField: string };
  /** The page lists rows to search, filter and switch to a table: false on a mode that has none (Teams, Public). */
  readonly listControls?: boolean;
  readonly extraChips?: readonly ListFilterChip[];
  /** The card/table switch. False on a page that has no table. */
  readonly viewToggle?: boolean;
  /** Clears what the page's own chips stand for when it lives outside the URL (a toolkit type filter). */
  readonly onClearExtra?: () => void;
  /** Params the page's own chips live in, cleared by the summary bar's Clear with the rest: `tags[]`. */
  readonly clearParams?: Readonly<Record<string, unknown>>;
  /** The folder management section. */
  readonly folders?: ReactNode;
  /** The page's own filters (tags, the author card), after the controls. */
  readonly filters?: ReactNode;
  /** `<ListSelectionSection>`, while rows are selected. */
  readonly selection?: ReactNode;
}

/** The search params the sidebar reads: the list's own and the ones its facets name. */
export type SearchRecord = Record<string, unknown>;

/** The values of a facet that are on: one for a single-choice facet, any number for a list (`statuses`). */
export function facetValues(search: SearchRecord, facet: ListFacet): string[] {
  const raw = search[facet.param];
  const all: unknown[] = Array.isArray(raw) ? (raw as unknown[]) : [raw];
  return all.filter((value): value is string => typeof value === 'string' && value !== '');
}

/** The next value of a facet when `value` is toggled: added to or removed from a list, or chosen or cleared when single. */
export function toggledFacet(facet: ListFacet, current: readonly string[], value: string): string | string[] {
  const on = current.includes(value);
  if (facet.array === true) return on ? current.filter((item) => item !== value) : [...current, value];
  return on ? '' : value;
}

/** The route's search and one write that merges into it, so a page's controls share one URL and never clobber each other. */
export function useListSearch(): { readonly search: SearchRecord; readonly patch: (next: SearchRecord) => void } {
  const navigate = useNavigate();
  const search = useSearch({ strict: false }) as SearchRecord;
  const patch = useCallback(
    (next: SearchRecord) => {
      void navigate({ to: '.', replace: true, search: (prev: SearchRecord) => ({ ...prev, ...next }) });
    },
    [navigate],
  );
  return { search, patch };
}

/** One sort field. A `directionless` one (a folder's own order) has no ascending/descending switch. */
interface ListSortField {
  readonly value: string;
  readonly label: string;
  readonly directionless?: boolean;
}
