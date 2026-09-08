/**
 * The per-bucket access list, over the generated client.
 *
 * `eliteaFetch` returns the transport ENVELOPE ({data, status, …}), not the
 * body, so every read here goes through `unwrapListPage`, which accepts either
 * and reports an unrecognised shape rather than silently answering "no
 * exceptions" — the failure that would render as "everyone has access" over a
 * project that restricts three people (#132).
 */
import {
  deleteBucketPermission,
  listBucketPermissions,
  setBucketPermissions,
} from '@/shared/api/generated/artifacts/artifacts';
import type { BucketPermissionMap } from '@/shared/api/generated/model';
import { unwrapListPage } from '@/shared/api/unwrap';

/** One member's exceptions, as the route serves them. */
export interface BucketPermissionRow {
  readonly user_id: number;
  readonly name?: string;
  readonly email?: string;
  readonly bucket_permissions: Readonly<Record<string, readonly string[]>>;
}

function toProjectID(projectId: string): number {
  const parsed = Number(projectId);
  if (!Number.isInteger(parsed)) throw new Error(`Project id "${projectId}" is not numeric.`);
  return parsed;
}

export async function fetchBucketPermissions(projectId: string): Promise<readonly BucketPermissionRow[]> {
  const response = await listBucketPermissions(toProjectID(projectId));
  const { rows } = unwrapListPage<BucketPermissionRow>(response, 'listBucketPermissions');
  // A row whose map is missing is a row with no exceptions. Normalising it
  // here keeps `{}` out of every consumer's optional chaining, where the
  // absent/empty distinction is easiest to lose.
  return rows.map((row) => ({ ...row, bucket_permissions: row.bucket_permissions ?? {} }));
}

/**
 * REPLACE one member's whole exception map. The caller must pass the map it
 * wants to end with — see `lib/bucketAccess.withBucketAccess`.
 */
export async function saveBucketPermissions(
  projectId: string,
  userId: number,
  bucketPermissions: Readonly<Record<string, readonly string[]>>,
): Promise<void> {
  // No status check: `eliteaFetch` REJECTS on a non-2xx answer, so a refusal
  // reaches the caller as a thrown error and a returned value always means the
  // write landed. A `result.status !== 200` guard here would be a branch no
  // response can take — the dead-code shape this repository keeps finding.
  await setBucketPermissions(toProjectID(projectId), {
    user_id: userId,
    // The generated body narrows the verbs to the read/write union. The map
    // this module carries is `string[]` because it is assembled from what the
    // server already sent, which the server has already constrained.
    bucket_permissions: bucketPermissions as BucketPermissionMap,
  });
}

/** Remove ONE exception. Used by the row's delete control. */
export async function removeBucketPermission(
  projectId: string,
  userId: number,
  bucket: string,
): Promise<void> {
  // Rejects on 404 (no such exception) and on 403, for the reason above.
  await deleteBucketPermission(toProjectID(projectId), { user_id: userId, bucket });
}
