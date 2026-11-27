import { describe, expect, it } from 'vitest';

import type { MarketplaceListingSummary } from '@/shared/api/generated/model';

import { apiKind, kindFromSearch, listingCard, listingParams, ratingText } from './listing';

function row(overrides: Partial<MarketplaceListingSummary> = {}): MarketplaceListingSummary {
  return {
    id: '7',
    kind: 'agent',
    entity_id: '42',
    name: 'Changelog Writer',
    description: 'Drafts release notes.',
    icon_url: 'https://cdn.example/a.png',
    category: 'Development',
    tags: ['github'],
    publisher: { id: '3', name: 'Dana Owner' },
    tier: 'medium',
    current_version: { id: '311', name: 'v2.3', published_at: '2026-10-30T09:00:00Z' },
    hires: 120,
    rating_avg: null,
    rating_count: 0,
    likes: 31,
    is_liked: false,
    featured: false,
    required_types: ['github'],
    has_write_tools: true,
    state: 'listed',
    hired: [],
    ...overrides,
  };
}

describe('kindFromSearch', () => {
  it('selects skills only for the literal value, so a stale or hand-typed value lands on agents', () => {
    expect(kindFromSearch('skills')).toBe('skills');
    expect(kindFromSearch('agents')).toBe('agents');
    expect(kindFromSearch('pipelines')).toBe('agents');
    expect(kindFromSearch(undefined)).toBe('agents');
  });

  it('names the route kind for a page kind', () => {
    expect(apiKind('skills')).toBe('skill');
    expect(apiKind('agents')).toBe('agent');
    expect(apiKind('teams')).toBe('team');
  });

  // Issue 3137: saved teams are a third kind.
  it('selects teams for the literal value, and maps a team row to a teams card', () => {
    expect(kindFromSearch('teams')).toBe('teams');
    expect(listingCard(row({ kind: 'team', entity_id: '0' })).kind).toBe('teams');
    expect(listingParams(listingCard(row({ kind: 'team' })))).toEqual({ kind: 'teams', listingId: '7' });
  });
});

describe('listingCard', () => {
  it('maps a search row to a card, keyed by the listing id', () => {
    expect(listingCard(row())).toEqual({
      kind: 'agents',
      id: '7',
      entityId: '42',
      name: 'Changelog Writer',
      description: 'Drafts release notes.',
      iconUrl: 'https://cdn.example/a.png',
      publisher: 'Dana Owner',
      ratingAvg: null,
      ratingCount: 0,
      hires: 120,
      likes: 31,
      isLiked: false,
      version: 'v2.3',
      versionId: '311',
      tier: 'medium',
      requiredTypes: ['github'],
      hiredVersion: undefined,
      categories: ['Development'],
      delisted: false,
    });
  });

  it('carries the caller\'s own like, for a skill as for an agent', () => {
    expect(listingCard(row({ kind: 'skill', is_liked: true })).isLiked).toBe(true);
  });

  it('marks a skill, a hire, and a delisted listing', () => {
    const card = listingCard(
      row({
        kind: 'skill',
        state: 'delisted',
        category: '',
        hired: [{ application_id: '9', mode: 'install', version_id: '311', version_name: 'v2.3' }],
      }),
    );
    expect(card.kind).toBe('skills');
    expect(card.hiredVersion).toBe('v2.3');
    expect(card.delisted).toBe(true);
    expect(card.categories).toEqual([]);
  });

  it('reads a listing with no current version as a card with no version label', () => {
    expect(listingCard(row({ current_version: null })).version).toBeUndefined();
  });
});

describe('listingParams', () => {
  it('names the listing page by kind and listing id', () => {
    expect(listingParams(listingCard(row()))).toEqual({ kind: 'agents', listingId: '7' });
  });
});

describe('ratingText', () => {
  it('says there are no ratings yet instead of showing a zero', () => {
    expect(ratingText({ ratingAvg: null, ratingCount: 0 })).toBe('No ratings yet');
  });

  it('reads a rating as text with its count', () => {
    expect(ratingText({ ratingAvg: 4.64, ratingCount: 31 })).toBe('Rated 4.6 out of 5 by 31');
  });
});
