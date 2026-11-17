/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 * The ONLY file `pages/apps/**` (or any other slice) may import from.
 *
 * Curated to exactly what `pages/apps/{Apps,AppDetail}.tsx` consume today
 * (verified via `npx knip --max-issues 0`) — `defaultAppsTab` and the
 * `AppDetailState`/`HasApplicationsState`/`AppsTab` types are real,
 * tested symbols (covered directly via `./model/tabs`, `./api/*`'s own
 * test files) but not part of this barrel: unlike Wave-1 infrastructure
 * (`shared/ui`, icons, ...) with many not-yet-landed consumers, this
 * slice's only consumer (`pages/apps`) is written in this same unit, so
 * there is no "ahead of a future consumer" case for symbols nothing
 * imports through this barrel right now.
 */
export { ApplicationCatalog } from './ui/catalog/ApplicationCatalog';

export { useAppDetail } from './api/useAppDetail';

export { appDetailErrorMessage } from './lib/errorMessage';

export { useHasApplications } from './api/useHasApplications';

export { appsTabByIndex, appsTabIndex, isApplicationsTab, normalizeAppsTab, searchForAppsTab } from './model/tabs';

export { useApplicationToolkits } from './api/useApplicationToolkits';

export { WIKI_TOOLKIT_TYPES } from './lib/constants';
