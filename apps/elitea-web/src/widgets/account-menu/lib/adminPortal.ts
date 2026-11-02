/**
 * The admin portal: who is offered the link and where it goes.
 *
 * "Admin" means the caller holds at least one administration-mode
 * permission (`GET /auth/permissions/administration`). It is the same rule
 * the admin console's boot gate applies to the list its handler injects
 * (`entries/admin/AdminApp.tsx`), so the link shows for exactly the people
 * the console lets in. It is presentation only: every admin route checks its
 * own permission again.
 */

/** The admin console's base (vite.config.ts's admin mode, the Go adminui handler's mount). */
const ADMIN_PORTAL_PATH = '/admin/app/';

interface PermissionRow {
  readonly name?: string | null;
  readonly enabled?: boolean | null;
}

/** Whether the administration-mode list holds any granted permission. Anything but an array is "no". */
export function holdsAdministrationPermission(permissions: unknown): boolean {
  if (!Array.isArray(permissions)) return false;
  return permissions.some((row: PermissionRow | null) => typeof row?.name === 'string' && row.name !== '' && row.enabled !== false);
}

/**
 * The portal's address. The web app links to it on its own origin; the
 * desktop app passes the deployment's origin and opens it in the browser,
 * where the console's browser session lives.
 */
export function adminPortalUrl(origin?: string): string {
  if (origin === undefined || origin === '') return ADMIN_PORTAL_PATH;
  return `${origin.replace(/\/+$/, '')}${ADMIN_PORTAL_PATH}`;
}
