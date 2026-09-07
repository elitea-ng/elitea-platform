import { describe, expect, it } from 'vitest';

import { OTHER_CATEGORY, TRENDING_CATEGORY, MY_LIKED_CATEGORY } from './constants';
import { buildAgentShareLink, buildAllCategories, calculateNewLikesCount, getCategoryForApplication } from './helpers';
import type { ApplicationData } from './types';

function makeApp(overrides: Partial<ApplicationData> = {}): ApplicationData {
  return {
    project_id: '1',
    id: 'app-1',
    name: 'Research Agent',
    description: '',
    version_id: 'v-1',
    version_name: 'v1',
    agent_type: 'agent',
    meta: null,
    tags: [],
    likes: 0,
    is_liked: false,
    ...overrides,
  };
}

describe('buildAllCategories', () => {
  it('puts Trending/My Liked first, sorts the rest, and moves Other to the end', () => {
    expect(buildAllCategories(['Zeta', 'Alpha', OTHER_CATEGORY])).toEqual([
      TRENDING_CATEGORY,
      MY_LIKED_CATEGORY,
      'Alpha',
      'Zeta',
      OTHER_CATEGORY,
    ]);
  });
});

describe('getCategoryForApplication (adversarial-review fix, cluster A13-agents-hub, finding 4)', () => {
  it('reads the category from meta.category — the field the real bulk-list endpoint actually populates', () => {
    const app = makeApp({ meta: { category: 'Productivity' } });
    expect(getCategoryForApplication(app)).toBe('Productivity');
  });

  it('falls back to a flat category field when meta has none (back-compat / test fixtures)', () => {
    const app = makeApp({ meta: null, category: 'Legacy Flat Category' });
    expect(getCategoryForApplication(app)).toBe('Legacy Flat Category');
  });

  it('falls back to Other when neither meta.category nor a flat category is set', () => {
    const app = makeApp({ meta: null });
    expect(getCategoryForApplication(app)).toBe(OTHER_CATEGORY);
  });

  it('falls back to Other when meta.category is an empty string', () => {
    const app = makeApp({ meta: { category: '' } });
    expect(getCategoryForApplication(app)).toBe(OTHER_CATEGORY);
  });
});

describe('calculateNewLikesCount', () => {
  it('trusts a positive server-reported count', () => {
    expect(calculateNewLikesCount(5, true, 0)).toBe(5);
  });

  it('optimistically increments when liked and the server count is not yet known', () => {
    expect(calculateNewLikesCount(0, true, 3)).toBe(4);
  });

  it('optimistically decrements (clamped at 0) when unliked and the server count is not yet known', () => {
    expect(calculateNewLikesCount(0, false, 0)).toBe(0);
  });
});

describe('buildAgentShareLink', () => {
  it('builds the baseline link shape, absolute and readable', () => {
    expect(
      buildAgentShareLink({ origin: 'https://elitea.example', catalogHref: '/app/elitea-catalog', agentId: '42' }),
    ).toBe('https://elitea.example/app/elitea-catalog?tab=agents&agentId=42');
  });

  /*
   * The router's own href would be `/app/elitea-catalog?agentId=%2242%22` if
   * the search went through its stringifier. A person pastes this link into a
   * message, so it carries the plain id — and it may carry only ONE query
   * string, so whatever the router put on the href is dropped first.
   */
  it('replaces a query the router already put on the href', () => {
    expect(
      buildAgentShareLink({
        origin: 'https://elitea.example',
        catalogHref: '/app/elitea-catalog?tab=skills',
        agentId: '42',
      }),
    ).toBe('https://elitea.example/app/elitea-catalog?tab=agents&agentId=42');
  });

  it('keeps the basepath the router resolved', () => {
    expect(
      buildAgentShareLink({ origin: 'https://elitea.example', catalogHref: '/elitea-catalog', agentId: 7 }),
    ).toBe('https://elitea.example/elitea-catalog?tab=agents&agentId=7');
  });

  it('reports no link for an agent with no id', () => {
    const common = { origin: 'https://elitea.example', catalogHref: '/app/elitea-catalog' };
    expect(buildAgentShareLink({ ...common, agentId: undefined })).toBe('');
    expect(buildAgentShareLink({ ...common, agentId: null })).toBe('');
    expect(buildAgentShareLink({ ...common, agentId: '' })).toBe('');
  });

  it('escapes an id that would otherwise break the query', () => {
    expect(
      buildAgentShareLink({ origin: 'https://elitea.example', catalogHref: '/app/elitea-catalog', agentId: 'a&b=c' }),
    ).toBe('https://elitea.example/app/elitea-catalog?tab=agents&agentId=a%26b%3Dc');
  });
});
