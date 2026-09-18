/**
 * #917 — the `SkillVersion` -> `AgentPipelineVersionOption` adapter. The
 * synthetic-id branch is the one that matters: a skill created client-side
 * has versions with no id at all, and the shared selector addresses rows by a
 * NUMBER, so an unmapped row would silently become `NaN` and never match the
 * selection.
 */
import { describe, expect, it } from 'vitest';

import type { SkillVersion } from '@/features/skills';

import { buildVersionKeyById, defaultVersionOptionId, selectedVersionOptionId, toSkillVersionOptions, versionOptionId } from './skillVersionOptions';

function version(overrides: Partial<SkillVersion> = {}): SkillVersion {
  return { name: 'base', instructions: '', tags: [], ...overrides };
}

const SAVED: readonly SkillVersion[] = [
  version({ id: '11', name: 'base', created_at: '2026-09-01T10:00:00Z' }),
  version({ id: 12, name: 'draft-v1', created_at: '2026-09-02T10:00:00Z', is_default: true, author: { id: '3', name: 'Ada', email: 'ada@example.com' } }),
];

describe('versionOptionId', () => {
  it('uses the wire id when there is one', () => {
    expect(versionOptionId(version({ id: '11' }), 0)).toBe(11);
    expect(versionOptionId(version({ id: 12 }), 1)).toBe(12);
  });

  it('falls back to a NEGATIVE synthetic for an unsaved version, which can never collide with a real id', () => {
    expect(versionOptionId(version(), 0)).toBe(-1);
    expect(versionOptionId(version({ id: 'not-a-number' }), 2)).toBe(-3);
  });
});

describe('toSkillVersionOptions', () => {
  it('carries the fields the shared selector renders — timestamp, default flag and creator', () => {
    const [base, draft] = toSkillVersionOptions(SAVED);
    expect(base).toEqual({ id: 11, name: 'base', created_at: '2026-09-01T10:00:00Z' });
    expect(draft?.is_default).toBe(true);
    expect(draft?.author).toEqual({ id: '3', name: 'Ada', email: 'ada@example.com' });
  });
});

describe('buildVersionKeyById', () => {
  it('maps a picked option back to the key the editor navigates by', () => {
    const map = buildVersionKeyById(SAVED);
    expect(map.get(11)).toBe('11');
    expect(map.get(12)).toBe('12');
  });

  it('maps an unsaved version back to its NAME, the key `skillVersionKey` gives it', () => {
    expect(buildVersionKeyById([version({ name: 'fresh' })]).get(-1)).toBe('fresh');
  });
});

describe('defaultVersionOptionId / selectedVersionOptionId', () => {
  it('names the default version, and nothing when no version claims it', () => {
    expect(defaultVersionOptionId(SAVED)).toBe(12);
    expect(defaultVersionOptionId([version({ id: '11' })])).toBeUndefined();
  });

  it('resolves the open version by its key, and answers undefined for a key no version has', () => {
    expect(selectedVersionOptionId(SAVED, '12')).toBe(12);
    expect(selectedVersionOptionId(SAVED, 'nope')).toBeUndefined();
  });
});
