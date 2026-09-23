import { describe, expect, it } from 'vitest';

import { filterVersionsBySearch, formatVersionTimestamp, searchableVersionName, versionCreatorLabel, versionMetaLine } from './AgentPipelineVersionSelector.search';
import type { SearchableVersion } from './AgentPipelineVersionSelector.search';

function version(overrides: Partial<SearchableVersion> = {}): SearchableVersion {
  return { name: 'v1', isLatest: false, ...overrides };
}

describe('searchableVersionName', () => {
  it('returns the literal LATEST_VERSION_NAME for the latest version, ignoring its own (possibly empty) name field', () => {
    expect(searchableVersionName(version({ isLatest: true, name: '' }))).toBe('base');
  });

  it('returns the version name otherwise', () => {
    expect(searchableVersionName(version({ name: 'Production-v1' }))).toBe('Production-v1');
  });
});

describe('filterVersionsBySearch — ELITEA-3278', () => {
  const versions: readonly SearchableVersion[] = [
    version({ name: 'base', isLatest: true }),
    version({ name: 'Production-v1', author: { name: 'Alice Smith', email: 'alice@example.com' } }),
    version({ name: 'staging-v2', author: { name: 'Bob Jones', email: 'bob@example.com' } }),
    version({ name: 'feature/update-v1', author: { name: 'Alice Johnson', email: 'alice.j@example.com' } }),
  ];

  it('returns every version, in order, for an empty or whitespace-only query', () => {
    expect(filterVersionsBySearch(versions, '')).toBe(versions);
    expect(filterVersionsBySearch(versions, '   ')).toBe(versions);
  });

  it('matches by name, case-insensitively and partially, including "base"', () => {
    expect(filterVersionsBySearch(versions, 'prod').map((v) => v.name)).toEqual(['Production-v1']);
    expect(filterVersionsBySearch(versions, 'PROD').map((v) => v.name)).toEqual(['Production-v1']);
    expect(filterVersionsBySearch(versions, 'bas').map((v) => v.name)).toEqual(['base']);
  });

  it('matches every version whose name contains "v" (partial, cross-cutting match)', () => {
    expect(filterVersionsBySearch(versions, 'v').map((v) => v.name)).toEqual(['Production-v1', 'staging-v2', 'feature/update-v1']);
  });

  it('matches special characters in a version name literally (no regex injection)', () => {
    expect(filterVersionsBySearch(versions, 'feature/update').map((v) => v.name)).toEqual(['feature/update-v1']);
  });

  it('matches by creator name', () => {
    expect(filterVersionsBySearch(versions, 'alice').map((v) => v.name)).toEqual(['Production-v1', 'feature/update-v1']);
  });

  it('matches by creator email, including a partial domain match', () => {
    expect(filterVersionsBySearch(versions, 'alice@example.com').map((v) => v.name)).toEqual(['Production-v1']);
    expect(filterVersionsBySearch(versions, '@example.com').map((v) => v.name)).toEqual(['Production-v1', 'staging-v2', 'feature/update-v1']);
  });

  it('matches by creator last name', () => {
    expect(filterVersionsBySearch(versions, 'jones').map((v) => v.name)).toEqual(['staging-v2']);
  });

  it('returns an empty list for a query that matches nothing', () => {
    expect(filterVersionsBySearch(versions, 'nonexistent-xyz-123')).toEqual([]);
  });

  it('never reorders — the caller\'s own timestamp sort survives filtering untouched (ELITEA-3280)', () => {
    const filtered = filterVersionsBySearch(versions, 'v');
    expect(filtered.map((v) => v.name)).toEqual(versions.filter((v) => filtered.includes(v)).map((v) => v.name));
  });
});

describe('formatVersionTimestamp — ELITEA-3279', () => {
  it('renders "MMM D, YYYY, h:mm AM/PM"', () => {
    expect(formatVersionTimestamp('2026-02-01T12:00:00Z')).toMatch(/[A-Za-z]{3}\s\d{1,2},\s\d{4},\s\d{1,2}:\d{2}\s(AM|PM)/);
  });

  it('is undefined for a missing or invalid timestamp', () => {
    expect(formatVersionTimestamp(undefined)).toBeUndefined();
    expect(formatVersionTimestamp('not-a-date')).toBeUndefined();
  });
});

describe('versionCreatorLabel / versionMetaLine', () => {
  it('prefers the creator name over the email', () => {
    expect(versionCreatorLabel(version({ author: { name: 'Alice Smith', email: 'alice@example.com' } }))).toBe('Alice Smith');
  });

  it('falls back to the email when no name is known', () => {
    expect(versionCreatorLabel(version({ author: { email: 'alice@example.com' } }))).toBe('alice@example.com');
  });

  it('is undefined when neither is known', () => {
    expect(versionCreatorLabel(version())).toBeUndefined();
  });

  it('joins creator and timestamp with " · " when both are known', () => {
    const expectedTimestamp = formatVersionTimestamp('2026-02-01T12:00:00Z');
    expect(versionMetaLine(version({ created_at: '2026-02-01T12:00:00Z', author: { name: 'Alice Smith' } }))).toBe(`Alice Smith · ${expectedTimestamp}`);
  });

  it('is undefined when neither creator nor timestamp is known', () => {
    expect(versionMetaLine(version())).toBeUndefined();
  });
});
