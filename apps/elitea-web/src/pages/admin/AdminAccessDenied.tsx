/**
 * The full-page 403 `AdminApp.tsx` renders in place of the admin router for a
 * caller `hasAnyAdminNavAccess()` refuses (smoke finding: a non-admin who
 * opened `/admin/app` saw the whole console shell — every write still came
 * back 403 from the server, but the page itself never said so).
 *
 * No `AdminLayout`, no `AdminNav`, no `Outlet` — this is what mounts INSTEAD
 * of the route tree, not a route inside it, so a stale bookmark or a typed
 * URL for any of the eleven pages lands here too.
 */
import { t } from '@/shared/i18n';

export function AdminAccessDenied() {
  return (
    <main data-testid="admin-access-denied">
      <h1>{t('pages.admin.accessDenied.title', 'Access denied')}</h1>
      <p>
        {t(
          'pages.admin.accessDenied.explanation',
          'Your account does not hold an administration permission for this console.',
        )}
      </p>
      <a href="/app/">{t('pages.admin.accessDenied.backToApp', 'Back to the app')}</a>
    </main>
  );
}
