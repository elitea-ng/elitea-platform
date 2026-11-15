import { describe, expect, it } from 'vitest';

import { statusBadgeLabel, workshopFacets } from '../lib/workshopFacets';

describe('workshopFacets', () => {
  it('offers only the values that occur, with the server counts', () => {
    const [status, origin] = workshopFacets({ statuses: { draft: 4, published: 0 }, origins: { own: 4 } });
    expect(status?.options).toEqual([{ value: 'draft', label: 'Draft', count: 4 }]);
    expect(origin?.options).toEqual([{ value: 'own', label: 'Own', count: 4 }]);
  });

  it('offers nothing before the list has answered, and never the three statuses no agent can hold', () => {
    for (const facet of workshopFacets(undefined)) expect(facet.options).toEqual([]);
    const [status] = workshopFacets({ statuses: { draft: 1, published: 2 }, origins: {} });
    expect(status?.options.map((option) => option.value)).toEqual(['draft', 'published']);
  });

  it('keeps a status the server sends that this build does not know, by its own name', () => {
    const [status] = workshopFacets({ statuses: { retired: 2, draft: 1 }, origins: {} });
    expect(status?.options.map((option) => option.label)).toEqual(['Draft', 'retired']);
  });

  it('names the status filter param and its array shape', () => {
    const [status, origin] = workshopFacets({ statuses: {}, origins: {} });
    expect(status).toMatchObject({ param: 'statuses', array: true });
    expect(origin?.param).toBe('origin');
  });
});

describe('statusBadgeLabel', () => {
  it('says the status as a word, and nothing for a row without one', () => {
    expect(statusBadgeLabel('published')).toBe('Published');
    expect(statusBadgeLabel('')).toBeUndefined();
    expect(statusBadgeLabel(undefined)).toBeUndefined();
  });
});
