import { describe, expect, it } from 'vitest';

import type { MarketplaceMergeField, MarketplaceUpstreamReview } from '@/shared/api/generated/model';

import {
  blockingFields,
  fieldText,
  parseEdit,
  readPolicy,
  resolutionsOf,
  statusText,
  wideningText,
} from './upgradeModel';

function field(key: string, status: string, overrides: Partial<MarketplaceMergeField> = {}): MarketplaceMergeField {
  return { key, label: key, status, default: 'ours', base: null, theirs: null, ours: null, ...overrides };
}

function review(fields: MarketplaceMergeField[]): MarketplaceUpstreamReview {
  return {
    mode: 'three_way',
    listing_id: '7',
    listing_name: 'Writer',
    theirs: {
      id: '2',
      name: 'v2',
      state: 'available',
      state_reason: '',
      release_notes: '',
      published_at: '2026-11-01T00:00:00Z',
    },
    fields,
    changes: [],
    widening: [],
    needs_approval: false,
    hireable: true,
    our_version_name: 'draft',
    open_change_request: false,
  };
}

describe('wideningText', () => {
  it('says what each kind of widening adds', () => {
    expect(wideningText({ kind: 'tool_added', detail: 'github: create_issue' })).toBe(
      'A new tool: github: create_issue',
    );
    expect(wideningText({ kind: 'tier_raised', detail: 'low to medium' })).toBe('A higher risk tier: low to medium');
    expect(wideningText({ kind: 'capability_added', detail: 'write tools' })).toBe('A new capability: write tools');
    expect(wideningText({ kind: 'requirement_added', detail: 'toolkit jira' })).toBe('A new requirement: toolkit jira');
  });
});

describe('readPolicy', () => {
  it('reads the three policies and falls back to pinned', () => {
    expect(readPolicy('auto')).toBe('auto');
    expect(readPolicy('notify')).toBe('notify');
    expect(readPolicy(undefined)).toBe('pinned');
    expect(readPolicy('')).toBe('pinned');
  });
});

describe('the three-way choices', () => {
  const fields = [
    field('instructions', 'conflict', { ours: 'a', theirs: 'b' }),
    field('welcome_message', 'theirs_only', { default: 'theirs' }),
    field('llm_settings', 'ours_only'),
  ];

  it('blocks the merge while a conflict has no choice', () => {
    expect(blockingFields(review(fields), {})).toEqual(['instructions']);
    expect(blockingFields(review(fields), { instructions: { choice: 'ours' } })).toEqual([]);
  });

  it('blocks an edit of a JSON field that does not parse, and nothing else', () => {
    const picks = { llm_settings: { choice: 'edit', text: '{not json' } } as const;
    expect(blockingFields(review(fields), { ...picks, instructions: { choice: 'theirs' } })).toEqual(['llm_settings']);
    expect(blockingFields(review(fields), { instructions: { choice: 'edit', text: 'any text is fine' } })).toEqual([]);
  });

  it('sends only what the person touched, with the edited value parsed', () => {
    const out = resolutionsOf(review(fields), {
      instructions: { choice: 'edit', text: 'merged' },
      llm_settings: { choice: 'edit', text: '{"temperature": 0.2}' },
      welcome_message: { choice: 'ours' },
    });
    expect(out).toEqual({
      instructions: { choice: 'edit', value: 'merged' },
      llm_settings: { choice: 'edit', value: { temperature: 0.2 } },
      welcome_message: { choice: 'ours' },
    });
  });
});

describe('field text', () => {
  it('shows a string as it is and anything else as JSON', () => {
    expect(fieldText('hello')).toBe('hello');
    expect(fieldText(null)).toBe('');
    expect(fieldText({ a: 1 })).toBe('{\n  "a": 1\n}');
  });

  it('parses an edit by the kind of field', () => {
    expect(parseEdit('instructions', '{plain}')).toEqual({ value: '{plain}' });
    expect(parseEdit('soul', '')).toEqual({ value: null });
    expect(parseEdit('soul', '{"role": "x"}')).toEqual({ value: { role: 'x' } });
    expect(parseEdit('soul', '{')).toBeUndefined();
  });

  it('names a status in words, not in colour', () => {
    expect(statusText('conflict')).toContain('Conflict');
    expect(statusText('theirs_only')).toContain('upstream');
    expect(statusText('same')).toContain('Same');
  });
});
