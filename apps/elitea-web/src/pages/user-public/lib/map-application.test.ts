import { describe, expect, it } from 'vitest';

import type { Application } from '@/shared/api/generated/model';

import { isPipelineApplication, mapApplicationToListItem } from './map-application';

function makeApplication(overrides: Partial<Application> = {}): Application {
  return {
    id: 'app-1',
    name: 'My Agent',
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    owner_id: 'owner-1',
    is_forked: false,
    meta: null,
    has_interrupt: false,
    tags: [],
    ...overrides,
  };
}

describe('isPipelineApplication', () => {
  it('is true when agent_type is "pipeline"', () => {
    expect(isPipelineApplication(makeApplication({ agent_type: 'pipeline' }))).toBe(true);
  });

  it('is false for a plain classic agent', () => {
    expect(isPipelineApplication(makeApplication({ agent_type: 'classic' }))).toBe(false);
  });

  it('is false when agent_type is absent', () => {
    expect(isPipelineApplication(makeApplication())).toBe(false);
  });
});

describe('mapApplicationToListItem', () => {
  it('maps the wire fields to the local list-item shape', () => {
    const application = makeApplication({
      description: 'Does agent things',
      status: 'published',
      authors: [{ id: 'a1', email: 'a1@example.com', name: 'Ada' }],
    });
    expect(mapApplicationToListItem(application)).toEqual({
      id: 'app-1',
      name: 'My Agent',
      description: 'Does agent things',
      status: 'published',
      authorNames: ['Ada'],
      createdAt: '2026-01-01T00:00:00Z',
      kind: 'agent',
    });
  });

  it('falls back to "Untitled" for a blank name', () => {
    expect(mapApplicationToListItem(makeApplication({ name: '   ' })).name).toBe('Untitled');
  });

  it('defaults description to an empty string and authorNames to an empty array when absent', () => {
    const result = mapApplicationToListItem(makeApplication());
    expect(result.description).toBe('');
    expect(result.authorNames).toEqual([]);
  });

  it('maps kind to "agent" for a plain classic application', () => {
    expect(mapApplicationToListItem(makeApplication({ agent_type: 'classic' })).kind).toBe('agent');
  });

  it('maps kind to "pipeline" when agent_type is "pipeline"', () => {
    expect(mapApplicationToListItem(makeApplication({ agent_type: 'pipeline' })).kind).toBe('pipeline');
  });
});
