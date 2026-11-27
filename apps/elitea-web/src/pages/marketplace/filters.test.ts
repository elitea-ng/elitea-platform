import { describe, expect, it } from 'vitest';

import { apiParamsFromFilters, filtersFromSearch, hasActiveFilter, searchFromFilters, sortFromSearch } from './filters';

describe('marketplace filters', () => {
  it('reads every facet from the URL and writes it back unchanged', () => {
    const search = {
      kind: 'skills',
      query: 'notes',
      category: 'Development,DevOps',
      tag: 'github',
      publisher_id: '3',
      max_tier: 'medium',
      requires: 'github,jira',
      min_rating: '4',
      hired_here: '1',
      sort_by: 'hires',
    };
    const filters = filtersFromSearch(search);
    expect(filters.categories).toEqual(['Development', 'DevOps']);
    expect(filters.requires).toEqual(['github', 'jira']);
    expect(filters.minRating).toBe(4);
    expect(filters.hiredHere).toBe(true);
    expect(searchFromFilters(filters)).toEqual(search);
  });

  it('keeps the address short: a default is left out', () => {
    expect(searchFromFilters(filtersFromSearch({}))).toEqual({});
  });

  it('takes Most liked as a sort, in the address and in the request', () => {
    expect(sortFromSearch('likes')).toBe('likes');
    expect(searchFromFilters(filtersFromSearch({ sort_by: 'likes' })).sort_by).toBe('likes');
    expect(apiParamsFromFilters(filtersFromSearch({ sort_by: 'likes' }))['sort']).toBe('likes');
  });

  it('degrades a bad sort or rating to the default', () => {
    expect(sortFromSearch('nonsense')).toBe('relevance');
    expect(filtersFromSearch({ min_rating: 'high' }).minRating).toBe(0);
  });

  it('asks the route with its own words', () => {
    expect(
      apiParamsFromFilters(
        filtersFromSearch({ kind: 'skills', query: ' notes ', hired_here: '1', requires: 'github' }),
      ),
    ).toEqual({
      kind: 'skill',
      sort: 'relevance',
      query: 'notes',
      requires: 'github',
      hired_here: true,
    });
  });

  // Issue 3137: the Teams kind travels in the address and reaches the route as `team`.
  it('keeps the Teams kind in the address and asks the route for teams', () => {
    const filters = filtersFromSearch({ kind: 'teams' });
    expect(searchFromFilters(filters)).toEqual({ kind: 'teams' });
    expect(apiParamsFromFilters(filters)['kind']).toBe('team');
  });

  it('counts a search or a facet as active, but not the kind or the sort', () => {
    expect(hasActiveFilter(filtersFromSearch({ kind: 'skills', sort_by: 'name' }))).toBe(false);
    expect(hasActiveFilter(filtersFromSearch({ max_tier: 'low' }))).toBe(true);
    expect(hasActiveFilter(filtersFromSearch({ query: '  ' }))).toBe(false);
  });
});
