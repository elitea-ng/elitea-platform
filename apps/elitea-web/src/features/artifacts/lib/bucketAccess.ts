/**
 * The mapping between what the bucket-access dialog shows and what the wire
 * carries.
 *
 * The wire is the legacy `bucket_permissions` map: bucket name to the verbs a
 * member keeps on it. Three states, and the reference UI names all three
 * (`[fsd]/features/artifacts/lib/constants/bucketAccess.constants.js`):
 *
 *   absent key            -> "Read/write (default)" — NO exception
 *   ["read"] (or any set
 *    without "write")     -> "Read-only"
 *   []                    -> "No access"
 *
 * THE TRAP THIS MODULE EXISTS TO CLOSE. `[]` and "absent" are opposite
 * decisions and both are falsy in JavaScript. `permissions?.length ? … : …`
 * reads a block as a default, and `permissions ?? []` writes a default as a
 * block. Every conversion goes through the two functions below so neither
 * spelling can appear at a call site.
 */

/** The three states the dialog offers, spelled as the reference spells them. */
export const BUCKET_ACCESS = {
  readWrite: 'read_write',
  read: 'read',
  noAccess: 'no_access',
} as const;

export type BucketAccess = (typeof BUCKET_ACCESS)[keyof typeof BUCKET_ACCESS];

/**
 * Which state one member holds for one bucket.
 *
 * `undefined` in means the map carries no entry, which is the default and
 * therefore `read_write`. Note that a caller must pass `undefined`, not `[]`,
 * for the absent case — see this module's header.
 */
export function accessFromPermissions(permissions: readonly string[] | undefined): BucketAccess {
  if (permissions === undefined) return BUCKET_ACCESS.readWrite;
  if (permissions.length === 0) return BUCKET_ACCESS.noAccess;
  return permissions.includes('write') ? BUCKET_ACCESS.readWrite : BUCKET_ACCESS.read;
}

/**
 * The verbs one access state stores, or `undefined` for the state that stores
 * NO entry at all.
 *
 * `read_write` returning `undefined` is the reference's own behaviour: its
 * edit dialog labels that option "Read/write (default)" and treats choosing it
 * as REMOVING the exception (`BucketAccessTable.jsx`: `const isRemoval =
 * permission === PERMISSION_OPTIONS.READ_WRITE`). Storing `["read","write"]`
 * instead would work, and would leave a row in the exceptions table saying
 * "this member has the default", which is the row the operator just asked to
 * delete.
 */
export function permissionsFromAccess(access: BucketAccess): readonly string[] | undefined {
  if (access === BUCKET_ACCESS.readWrite) return undefined;
  if (access === BUCKET_ACCESS.noAccess) return [];
  return ['read'];
}

/**
 * The member's whole map after changing ONE bucket.
 *
 * The routes REPLACE the map (the Go handler and legacy both), so the caller
 * must send the member's other exceptions back unchanged. Building the whole
 * map here is what stops a call site from sending only the bucket it touched
 * and silently clearing the rest.
 */
export function withBucketAccess(
  existing: Readonly<Record<string, readonly string[]>>,
  bucket: string,
  access: BucketAccess,
): Record<string, readonly string[]> {
  const next: Record<string, readonly string[]> = { ...existing };
  const permissions = permissionsFromAccess(access);
  if (permissions === undefined) {
    delete next[bucket];
    return next;
  }
  next[bucket] = permissions;
  return next;
}
