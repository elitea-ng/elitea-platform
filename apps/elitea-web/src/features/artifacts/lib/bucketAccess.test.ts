import { describe, expect, it } from 'vitest';

import {
  accessFromPermissions,
  BUCKET_ACCESS,
  permissionsFromAccess,
  withBucketAccess,
} from './bucketAccess';

describe('accessFromPermissions', () => {
  // The distinction the whole module exists for: `undefined` (no entry) and
  // `[]` (an entry meaning no access) are opposite decisions and both falsy.
  it('reads an absent entry as the read/write default', () => {
    expect(accessFromPermissions(undefined)).toBe(BUCKET_ACCESS.readWrite);
  });

  it('reads an empty array as no access', () => {
    expect(accessFromPermissions([])).toBe(BUCKET_ACCESS.noAccess);
  });

  it('reads a read-only entry as read-only', () => {
    expect(accessFromPermissions(['read'])).toBe(BUCKET_ACCESS.read);
  });

  it('reads any entry carrying write as read/write', () => {
    expect(accessFromPermissions(['read', 'write'])).toBe(BUCKET_ACCESS.readWrite);
    // The reference grants read to any non-empty set, so a write-only entry is
    // read/write here too.
    expect(accessFromPermissions(['write'])).toBe(BUCKET_ACCESS.readWrite);
  });
});

describe('permissionsFromAccess', () => {
  it('stores no entry at all for the default', () => {
    expect(permissionsFromAccess(BUCKET_ACCESS.readWrite)).toBeUndefined();
  });

  it('stores an empty array for no access', () => {
    expect(permissionsFromAccess(BUCKET_ACCESS.noAccess)).toEqual([]);
  });

  it('stores read for read-only', () => {
    expect(permissionsFromAccess(BUCKET_ACCESS.read)).toEqual(['read']);
  });
});

describe('withBucketAccess', () => {
  it('keeps the member’s other exceptions, because the route replaces the map', () => {
    const next = withBucketAccess({ datasets: ['read'] }, 'reports', BUCKET_ACCESS.noAccess);
    expect(next).toEqual({ datasets: ['read'], reports: [] });
  });

  it('deletes the key when the choice is the default', () => {
    const next = withBucketAccess(
      { datasets: ['read'], reports: [] },
      'reports',
      BUCKET_ACCESS.readWrite,
    );
    expect(next).toEqual({ datasets: ['read'] });
    expect('reports' in next).toBe(false);
  });

  it('does not mutate the map it was given', () => {
    const existing = { reports: ['read'] };
    withBucketAccess(existing, 'reports', BUCKET_ACCESS.noAccess);
    expect(existing).toEqual({ reports: ['read'] });
  });

  it('adds the first exception to an empty map', () => {
    expect(withBucketAccess({}, 'reports', BUCKET_ACCESS.read)).toEqual({ reports: ['read'] });
  });
});
