import { describe, expect, it } from 'vitest';

import { OTHER_CATEGORY } from './constants';
import { getCategoryForApplication } from './helpers';
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

describe('getCategoryForApplication', () => {
  it('reads the category from meta.category — the field the real bulk-list endpoint actually populates', () => {
    expect(getCategoryForApplication(makeApp({ meta: { category: 'Productivity' } }))).toBe('Productivity');
  });

  it('falls back to a flat category field when meta has none (back-compat / test fixtures)', () => {
    expect(getCategoryForApplication(makeApp({ meta: null, category: 'Legacy Flat Category' }))).toBe('Legacy Flat Category');
  });

  it('falls back to Other when neither meta.category nor a flat category is set', () => {
    expect(getCategoryForApplication(makeApp({ meta: null }))).toBe(OTHER_CATEGORY);
  });

  it('falls back to Other when meta.category is an empty string', () => {
    expect(getCategoryForApplication(makeApp({ meta: { category: '' } }))).toBe(OTHER_CATEGORY);
  });
});
