/**
 * ROUTE-076 anomaly: `/settings/:tab` with an unknown tab (spec §8.1 D4:
 * "`/settings/:tab` is declared but the Settings children are explicit, so
 * an unknown tab renders the Settings layout with an empty outlet — no
 * 404, no redirect"). Reproduced bug-for-bug (decision D4).
 *
 * Deliberately a dynamic catch-all SIBLING of the explicit literal children
 * above (`model-configuration.tsx`, `environment.tsx`, ...), not a
 * replacement for them: TanStack Router ranks static path segments above
 * dynamic `$param` segments at the same tree level unconditionally
 * (verified — see `-ui/ExclusiveOutlet.tsx`'s header for the same
 * static-over-dynamic verification method), so a known tab always resolves
 * to its own literal route file and only a genuinely unrecognised tab
 * falls through to this one.
 *
 * Necessary in THIS router (not just faithful-for-its-own-sake): without
 * an explicit catch-all here, TanStack's file-based tree has no route that
 * matches `/settings/<unknown>` at all, and the match would fall through
 * past `settings/route.tsx` entirely to unit R3's `$projectId/$` splat
 * (`projectId="settings"`) — a real behavioural divergence, not merely a
 * missed nicety. `component: () => null` renders nothing into
 * `settings/route.tsx`'s `<Outlet/>`, matching the old app's empty-outlet
 * observation exactly.
 */
import { createFileRoute } from '@tanstack/react-router';

import { DesktopSettingsEntry } from '@/pages/workspace/desktopEntry';

/**
 * The desktop tabs (`DESKTOP_SETTINGS_TABS`, desktop build only) ride on this catch-all instead of route files of their
 * own, so the web build's route tree does not grow; each entry renders nothing outside `desktop`. The redirect reads the
 * same list (#3078), so it never bounces them.
 */
function SettingsTab(): React.JSX.Element | null {
  const { tab } = Route.useParams();
  return <DesktopSettingsEntry tab={tab} />;
}

export const Route = createFileRoute('/_shell/settings/$tab')({
  component: SettingsTab,
});
