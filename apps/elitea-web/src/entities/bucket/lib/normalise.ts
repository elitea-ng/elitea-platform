import type { Bucket, BucketWire } from '../model/types';

/** snake_case wire shape -> camelCase domain type (handler.go:27-37). */
export function normaliseBucket(wire: BucketWire): Bucket {
  return {
    // The handler exposes no surrogate id; the name is the bucket's identity.
    id: wire.name,
    name: wire.name,
    isPinned: wire.is_pinned,
    createdAt: wire.created_at,
    retentionDays: wire.retention_days ?? null,
    sizeBytes: wire.size_bytes ?? 0,
  };
}

export function normaliseBuckets(wire: readonly BucketWire[]): Bucket[] {
  return wire.map(normaliseBucket);
}
