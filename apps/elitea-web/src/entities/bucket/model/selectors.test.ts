import { describe, expect, it } from 'vitest';

import { filterBucketsByQuery, isPinnedBucket, sortBucketsPinnedFirst } from './selectors';
import type { Bucket } from './types';

const bucket = (id: string, name: string, isPinned = false): Bucket => ({
  id,
  name,
  isPinned,
  createdAt: '2026-01-01T00:00:00Z',
  retentionDays: null,
  sizeBytes: 0,
});

describe('sortBucketsPinnedFirst', () => {
  it('puts pinned buckets before unpinned ones', () => {
    const buckets = [bucket('1', 'zeta'), bucket('2', 'alpha', true)];
    expect(sortBucketsPinnedFirst(buckets).map((b) => b.id)).toEqual(['2', '1']);
  });

  it('sorts alphabetically within the same pin state', () => {
    const buckets = [bucket('1', 'zeta', true), bucket('2', 'alpha', true)];
    expect(sortBucketsPinnedFirst(buckets).map((b) => b.id)).toEqual(['2', '1']);
  });

  it('does not mutate the input', () => {
    const buckets = [bucket('1', 'b'), bucket('2', 'a')];
    const copy = [...buckets];
    sortBucketsPinnedFirst(buckets);
    expect(buckets).toEqual(copy);
  });
});

describe('isPinnedBucket', () => {
  it('reflects the isPinned field', () => {
    expect(isPinnedBucket(bucket('1', 'a', true))).toBe(true);
    expect(isPinnedBucket(bucket('1', 'a', false))).toBe(false);
  });
});

describe('filterBucketsByQuery', () => {
  const buckets = [bucket('1', 'Artifacts'), bucket('2', 'art-history'), bucket('3', 'Uploads')];

  it('matches case-insensitive substrings', () => {
    expect(filterBucketsByQuery(buckets, 'ART').map((b) => b.id)).toEqual(['1', '2']);
  });

  it('returns every bucket for a blank query', () => {
    expect(filterBucketsByQuery(buckets, '')).toEqual(buckets);
  });
});
