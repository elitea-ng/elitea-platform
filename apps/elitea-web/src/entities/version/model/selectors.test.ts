import { describe, expect, it } from 'vitest';

import {
  LATEST_VERSION_NAME,
  defaultRunVersionId,
  isSetDefaultDisabled,
  isVersionNotFound,
  selectDefaultVersion,
  sortVersionsForPicker,
} from './selectors';
import type { VersionSummary } from './types';

const version = (id: string, overrides: Partial<VersionSummary> = {}): VersionSummary => ({
  id,
  name: `v${id}`,
  status: 'draft',
  agentType: 'openai',
  createdAt: '2026-01-01T00:00:00Z',
  ...overrides,
});

describe('selectDefaultVersion', () => {
  const base = version('1', { name: LATEST_VERSION_NAME });
  const named = version('2', { name: 'v2' });

  it('prefers the version matching defaultVersionId', () => {
    expect(selectDefaultVersion([base, named], '2')).toBe(named);
  });

  it('falls back to the "base" version when defaultVersionId is unresolved', () => {
    expect(selectDefaultVersion([base, named], 'missing')).toBe(base);
  });

  it('falls back to "base" when no defaultVersionId is given', () => {
    expect(selectDefaultVersion([base, named], undefined)).toBe(base);
  });

  it('returns undefined when neither a default id nor a "base" version exists', () => {
    expect(selectDefaultVersion([named], undefined)).toBeUndefined();
  });
});

describe('sortVersionsForPicker', () => {
  it('puts the default version first, "base" last, and sorts the rest by createdAt desc', () => {
    const base = version('1', { name: LATEST_VERSION_NAME, createdAt: '2026-01-01T00:00:00Z' });
    const def = version('2', { createdAt: '2026-01-02T00:00:00Z' });
    const older = version('3', { createdAt: '2026-01-03T00:00:00Z' });
    const newer = version('4', { createdAt: '2026-01-05T00:00:00Z' });
    const sorted = sortVersionsForPicker([base, older, def, newer], '2');
    expect(sorted.map((v) => v.id)).toEqual(['2', '4', '3', '1']);
  });

  it('does not mutate the input', () => {
    const list = [version('1'), version('2')];
    const copy = [...list];
    sortVersionsForPicker(list, undefined);
    expect(list).toEqual(copy);
  });
});

describe('isSetDefaultDisabled', () => {
  it('is disabled when the version is already the default', () => {
    expect(isSetDefaultDisabled(version('1'), '1')).toBe(true);
  });

  it('is disabled for the unnamed "base" fallback when no default is set yet', () => {
    expect(isSetDefaultDisabled(version('1', { name: LATEST_VERSION_NAME }), undefined)).toBe(true);
  });

  it('is disabled for a published version', () => {
    expect(isSetDefaultDisabled(version('1', { status: 'published' }), '2')).toBe(true);
  });

  it('is enabled for a non-default, non-base, non-published version', () => {
    expect(isSetDefaultDisabled(version('1', { name: 'v1', status: 'draft' }), '2')).toBe(false);
  });
});

describe('isVersionNotFound', () => {
  const versions = [version('1'), version('2')];

  it('is false when the id is present (compared as strings)', () => {
    expect(isVersionNotFound('1', versions)).toBe(false);
  });

  it('is true when the id is absent', () => {
    expect(isVersionNotFound('99', versions)).toBe(true);
  });

  it('is true for an empty version list', () => {
    expect(isVersionNotFound('1', [])).toBe(true);
  });
});

describe('defaultRunVersionId (issue 1941)', () => {
  it('prefers meta.default_version_id, then base, then the first usable version', () => {
    expect(defaultRunVersionId([{ id: 3, name: 'base' }, { id: '5', name: 'v5' }], 5)).toBe(5);
    expect(defaultRunVersionId([{ id: 2, name: 'draft' }, { id: 3, name: 'base' }], undefined)).toBe(3);
    expect(defaultRunVersionId([{ id: 0, name: 'base' }, { id: 7, name: 'x' }], '')).toBe(7);
    expect(defaultRunVersionId([], 1)).toBeUndefined();
  });

  it('ignores is_default: a disagreeing flag does not beat meta.default_version_id', () => {
    const versions = [
      { id: 4, name: 'v2', is_default: true },
      { id: 5, name: 'v3' },
    ];
    expect(defaultRunVersionId(versions, 5)).toBe(5);
  });
});
