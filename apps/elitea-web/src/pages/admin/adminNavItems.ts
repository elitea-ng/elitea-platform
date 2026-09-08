/**
 * The admin SPA's navigation model — items, permission filtering, and
 * active-route resolution (issue #225).
 *
 * Unit A14 ported ten admin pages and #223 retired `apps/admin-ui`, which made
 * this SPA the one that ships. Its root route was a bare `<Outlet/>`: every page
 * worked by URL and NOTHING linked them, so an operator opening `/admin/app/`
 * landed on Users with no path to the other nine. This module is the data half
 * of the fix; `AdminNav.tsx` renders it and `AdminLayout.tsx` mounts it.
 *
 * Reference (read-only): `frontends/admin_ui/frontend/src/components/Layout/
 * Sidebar.jsx` + `constants/permissions.js`. Its two groups, order, icons and
 * collapse behaviour are reproduced. Four things are NOT:
 *
 *  1. **LiteLLM is absent.** The reference's top group ends with it. That page
 *     was deliberately not ported (`./router.tsx`'s header: #201 replaces
 *     LiteLLM with Bifrost), so there is no `/litellm` route in this bundle. A
 *     nav entry pointing at a route that does not exist is the dead-wiring class
 *     this whole port exists to remove — and `AdminNav.test.tsx` derives the
 *     legal path set from the ROUTER, so re-adding one cannot pass CI.
 *
 *  2. **Service Descriptors is present**, though the reference puts it in no nav
 *     group at all. In `admin_ui` that page is dead code (`routes.js` declares
 *     no route and nothing imports it); we gave it a route, so today it is
 *     reachable only by typing a URL — the exact invisibility #225 is about. It
 *     renders the SERVER's 501 reason ("this platform has no provider hub"),
 *     which is a real answer to a real operator question, and materially
 *     different from a control that silently does nothing: "this platform
 *     cannot do that, and here is why" is information, and an operator who
 *     cannot find out is left to guess. It sits in the bottom group beside the
 *     other platform-wide pages.
 *
 *  3. **`schedules` is labelled "Schedules & Tasks", not "System".** The
 *     reference labels that item "System" while the page it opens is headed
 *     "Schedules & Tasks" (`SchedulesTasks.tsx`). A nav label that does not name
 *     its destination costs the operator a click to find out what it was, and
 *     "System" names nothing in particular on a page that is entirely about
 *     schedules and tasks. The page heading wins; the nav follows it.
 *
 *  4. **`isActiveTab`'s last-path-segment comparison is gone.** The reference
 *     compares the LAST segment of `location.pathname` to the item id, so
 *     `/users/123` — or any nested route — highlights nothing at all. Here the
 *     active item comes from the ROUTER's matched route ids (see
 *     `activeAdminNavItemId`), which include every ancestor of the match, so a
 *     nested route highlights its section by construction.
 *
 * ## `permissions` here is PRESENTATION, never authorisation
 *
 * `./adminUiConfig.ts`'s header has the full account. The Go handler resolves
 * the caller's real administration-mode grants, and it injects an empty list
 * when the resolver refuses. The array is a hint about what to render. It is
 * not a gate. Hiding an item whose page would answer 403 is good UX, and it is
 * all that happens here. The server refuses the request either way, and a typed
 * URL still reaches the page (and still gets refused).
 */
import { adminUiShowsControlFor } from './adminUiConfig';
import { navGroups } from './adminNavGroups';
import type { AdminNavGroup } from './adminNavGroups';

// The item and group SHAPES live in `./adminNavGroups`, beside the table that
// declares them, and are re-exported here so every existing importer keeps one
// module to reach both the types and the functions that read them.
export type { AdminNavItem, AdminNavGroup } from './adminNavGroups';

/**
 * The route id the ADMIN INDEX resolves to. `router.tsx` renders `AdminUsers`
 * at `/` rather than redirecting to `/users` (its header explains why), so on
 * `/admin/app/` the Users item must light up even though `/users` was never
 * matched. Without this the landing screen is the one screen with no active
 * item, which reads as "the nav is broken" on first contact.
 */
const ADMIN_INDEX_ROUTE_ID = '/';

/** Which item the index route stands in for. Kept next to the alias it serves. */
const INDEX_ALIAS_ITEM_ID = 'users';


/**
 * Every item, ungated. Exists so the drift test can assert that EVERY nav
 * target is a real route regardless of which permissions the current session
 * happens to carry — a permission-filtered list would let a broken path hide
 * behind a missing permission.
 */
export function adminNavGroups(): readonly AdminNavGroup[] {
  return navGroups();
}

/**
 * The groups an operator can see, given a permission probe. Groups that lose
 * every item are dropped entirely, so no empty group renders a stray divider.
 *
 * `shows` is injected rather than imported so tests can drive it directly; it
 * defaults to the real (presentation-only) probe.
 */
export function visibleAdminNavGroups(
  shows: (permission: string) => boolean = adminUiShowsControlFor,
): readonly AdminNavGroup[] {
  return navGroups()
    .map((group) => ({
      ...group,
      items: group.items.filter(
        (item) => item.anyPermission.length === 0 || item.anyPermission.some((permission) => shows(permission)),
      ),
    }))
    .filter((group) => group.items.length > 0);
}

/**
 * Which nav item is active, from the ROUTER's matched route ids.
 *
 * `matchedRouteIds` is `routerState.matches.map((match) => match.routeId)` —
 * TanStack Router's own answer to "what is active", containing the leaf match
 * AND every ancestor. That is what makes nesting work with no string surgery:
 * a future `/users/$userId` match arrives as `['__root__', '/users',
 * '/users/$userId']`, and `/users` is in it, so Users highlights. The reference
 * compared the last PATH SEGMENT (`'123'`) to the item id (`'users'`) and
 * highlighted nothing.
 *
 * Returns the item id, or `undefined` when no nav item owns the current route
 * (a 404, say) — which must stay `undefined` rather than defaulting to the first
 * item, since a nav claiming you are on a page you are not on is worse than one
 * claiming nothing.
 */
export function activeAdminNavItemId(matchedRouteIds: readonly string[]): string | undefined {
  const matched = new Set(matchedRouteIds);
  if (matched.has(ADMIN_INDEX_ROUTE_ID)) return INDEX_ALIAS_ITEM_ID;
  for (const group of navGroups()) {
    for (const item of group.items) {
      if (matched.has(item.path)) return item.id;
    }
  }
  return undefined;
}

/**
 * Whether the caller unlocks ANY sidebar section — the boot-time gate
 * `AdminApp.tsx` uses to decide whether the admin router mounts at all.
 *
 * `visibleAdminNavGroups` already computes exactly this per item, by
 * construction: an item survives its filter only when the (presentation-only)
 * probe shows one of its `anyPermission` names. A caller for whom every group
 * comes back empty has no reachable page in this bundle, which used to render
 * as a full console shell around an empty sidebar — the router mounted
 * regardless, so `/admin/app/` itself, and every page a URL could name, still
 * rendered. This collapses the same computation to the boolean that lets the
 * boot gate skip mounting the router at all, rather than leaving that shell
 * on screen for a caller with nothing to do in it.
 *
 * Still presentation, not authorisation, for the reason `adminUiConfig.ts`'s
 * header gives: the server refuses every write on its own account regardless
 * of what renders here. What changes is WHERE that fact is acted on — one
 * boot-time check instead of eleven pages each finding out from a 403.
 */
export function hasAnyAdminNavAccess(
  shows: (permission: string) => boolean = adminUiShowsControlFor,
): boolean {
  return visibleAdminNavGroups(shows).length > 0;
}
