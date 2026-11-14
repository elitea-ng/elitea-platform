import { describe, expect, it } from 'vitest';

import { FEATURED_COUNT, POPULAR_COUNT } from './constants';
import { agentCard, featuredRow, filterByCategories, kindFromSearch, listingParams, popularRow, skillCard } from './listing';
import type { ApplicationData, ListingCardData } from './types';

function app(overrides: Partial<ApplicationData> = {}): ApplicationData {
  return {
    project_id: '1',
    id: '7',
    name: 'Research Agent',
    description: 'Finds things',
    version_id: '70',
    version_name: 'v2',
    agent_type: 'agent',
    meta: { category: 'Development', icon_meta: { url: 'https://cdn.example/a.png' } },
    tags: [],
    likes: 3,
    is_liked: false,
    ...overrides,
  };
}

function card(overrides: Partial<ListingCardData> = {}): ListingCardData {
  return { kind: 'agents', id: '1', name: 'A', description: '', iconUrl: undefined, likes: 0, version: undefined, categories: ['Other'], recency: 1, ...overrides };
}

describe('kindFromSearch', () => {
  it('selects skills only for the literal value, so a stale or hand-typed value lands on agents', () => {
    expect(kindFromSearch('skills')).toBe('skills');
    expect(kindFromSearch('agents')).toBe('agents');
    expect(kindFromSearch('pipelines')).toBe('agents');
    expect(kindFromSearch(undefined)).toBe('agents');
  });
});

describe('agentCard', () => {
  it('maps a catalogue row to a card, reading the icon and category from meta', () => {
    expect(agentCard(app())).toEqual({
      kind: 'agents',
      id: '7',
      name: 'Research Agent',
      description: 'Finds things',
      iconUrl: 'https://cdn.example/a.png',
      likes: 3,
      version: 'v2',
      categories: ['Development'],
      recency: 70,
    });
  });

  it('has no icon and no version when the row carries none', () => {
    const mapped = agentCard(app({ meta: null, version_name: '' }));
    expect(mapped.iconUrl).toBeUndefined();
    expect(mapped.version).toBeUndefined();
    expect(mapped.categories).toEqual(['Other']);
  });
});

describe('skillCard', () => {
  it('maps a skill with a published version, filing it under its tags and carrying no likes', () => {
    const mapped = skillCard({
      id: 42,
      name: 'PR Reviewer',
      description: 'Reviews',
      tags: ['review', 'Development'],
      versions: [
        { id: 5, name: 'base', status: 'draft' },
        { id: 6, name: 'v1.0', status: 'published' },
      ],
    });
    expect(mapped).toMatchObject({ kind: 'skills', id: '42', version: 'v1.0', likes: undefined, categories: ['review', 'Development'], recency: 6 });
  });

  it('drops a skill with no published version or no id: there is nothing to open or add', () => {
    expect(skillCard({ id: 1, versions: [{ id: 5, status: 'draft' }] })).toBeNull();
    expect(skillCard({ versions: [{ id: 6, status: 'published' }] })).toBeNull();
  });
});

describe('listingParams', () => {
  it('names the listing page the card links to', () => {
    expect(listingParams(card({ kind: 'skills', id: '9' }))).toEqual({ kind: 'skills', listingId: '9' });
  });
});

describe('filterByCategories', () => {
  const cards = [card({ id: '1', categories: ['Development'] }), card({ id: '2', categories: ['DevOps', 'Other'] }), card({ id: '3', categories: ['Other'] })];

  it('keeps everything when nothing is picked', () => {
    expect(filterByCategories(cards, [])).toHaveLength(3);
  });

  it('is multi-select: a listing matches when it carries ANY picked category', () => {
    expect(filterByCategories(cards, ['Development', 'DevOps']).map((c) => c.id)).toEqual(['1', '2']);
  });
});

describe('featuredRow and popularRow', () => {
  const many = Array.from({ length: 12 }, (_, index) => card({ id: String(index), recency: index, likes: index }));

  it('features the most recently published listings, newest first', () => {
    const row = featuredRow(many);
    expect(row).toHaveLength(FEATURED_COUNT);
    expect(row[0]?.id).toBe('11');
  });

  it('ranks the popular row by likes and leaves out listings nobody liked', () => {
    const row = popularRow(many);
    expect(row).toHaveLength(POPULAR_COUNT);
    expect(row[0]?.id).toBe('11');
    expect(row.every((c) => (c.likes ?? 0) > 0)).toBe(true);
  });

  it('has no popular row for skills, which have no like store', () => {
    expect(popularRow([card({ kind: 'skills', likes: undefined })])).toEqual([]);
  });
});
