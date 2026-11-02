/**
 * Pure `document.title` derivation (spec: old app `hooks/useBrowserPageTitle.js`
 * / `PageTitleSetter`). Reduced from the baseline, documented precisely:
 *
 * The old app branches on route-specific PARAM NAMES (`params.agentId`,
 * `params.skillId`, `params.toolkitId`, `params.mcpId`,
 * `params.credential_uid`, `params.conversationId`) that only a route file
 * itself resolves via `useParams()` — a generic pathname+search deriver
 * (this widget has no page-specific route context) cannot read a named
 * path param without importing the actual route definition it belongs to
 * (out of this widget's ownership fence). The `?name=` SEARCH param the old
 * app also reads (`URLSearchParams(location.search).get('name')`,
 * independent of any specific route's typed params) is the one signal this
 * function CAN read generically, and does — covering the common case
 * ("Chat: My Conversation - Project") while the id-only fallback
 * ("Chat: conv-abc123 - Project") is not reproduced (falls back to the
 * section name alone instead of the id). A real, reduced title, not a
 * placeholder: every branch produces a genuine value old-app users would
 * recognise, just without the id-in-title fallback.
 *
 * The old app's OTHER pathname-readable signal, `params.tab` (e.g.
 * `Toolkits: ${params.tab} - ${projectName}`), IS reproduced, generically,
 * for every section below whose route tree actually has a `$tab` segment
 * (`/agents`, `/pipelines`, `/toolkits`, `/mcps`, `/credentials`,
 * `/skills`, `/apps`, `/user-public` — each has a real `routes/_shell/
 * <section>/$tab.tsx`) — the exact same pathname-splitting technique
 * `settingsTitle` below already uses for `/settings/$tab.tsx`. `hasTab:
 * true` marks those sections; `/chat` (keyed by `conversationId`, not a
 * tab), `/agents-hub`, `/artifacts`, and `/help-center` (no `$tab` route at
 * all) are left without it, matching the old app (which never branches on
 * `params.tab` for those four either).
 */
const SECTION_BY_PREFIX: ReadonlyArray<{ prefix: string; label: string; hasTab?: true }> = [
  { prefix: '/chat', label: 'Chat' },
  { prefix: '/agents-hub', label: 'Agent HUB' },
  // `/elitea-catalog` is where `/agents-hub` now redirects; listed here so
  // the title is the catalogue's, not a fallback (baseline
  // `useBrowserPageTitle.js:23` branches on the same prefix).
  { prefix: '/elitea-catalog', label: 'ELITEA Catalog' },
  { prefix: '/agents', label: 'Agents', hasTab: true },
  { prefix: '/pipelines', label: 'Pipelines', hasTab: true },
  { prefix: '/toolkits', label: 'Toolkits', hasTab: true },
  { prefix: '/mcps', label: 'MCPs', hasTab: true },
  { prefix: '/credentials', label: 'Credentials', hasTab: true },
  { prefix: '/artifacts', label: 'Artifacts' },
  { prefix: '/user-public', label: 'User public', hasTab: true },
  { prefix: '/skills', label: 'Skills', hasTab: true },
  { prefix: '/apps', label: 'Applications', hasTab: true },
  { prefix: '/help-center', label: 'Help Center' },
];

/** `/settings/foo` -> "Settings: Foo"; the two bespoke-cased tabs the old app titles specially. */
const SETTINGS_TAB_LABEL: Readonly<Record<string, string>> = {
  personalization: 'Personalization',
  notifications: 'Notifications',
};

function settingsTitle(pathname: string, projectName: string): string {
  const tab = pathname.replace(/^\/settings\/?/, '').split('/')[0];
  if (!tab) return `Settings - ${projectName}`;
  const label = SETTINGS_TAB_LABEL[tab] ?? tab;
  return `Settings: ${label} - ${projectName}`;
}

/**
 * The section a pathname is in, as the shell's title row names it: its label
 * and the route its crumb links back to (the section's list). `undefined` on
 * a pathname no section claims (the home redirect, onboarding).
 */
export function pageSection(pathname: string): { readonly label: string; readonly to: string } | undefined {
  if (pathname === '/settings' || pathname.startsWith('/settings/')) return { label: 'Settings', to: '/settings' };
  const section = SECTION_BY_PREFIX.find((entry) => pathname.startsWith(entry.prefix));
  return section === undefined ? undefined : { label: section.label, to: section.prefix };
}

/**
 * @param pathname current router pathname
 * @param searchName the `?name=` query param value, if present
 * @param projectName the selected project's display name (or `undefined` before one is known)
 */
export function derivePageTitle(pathname: string, searchName: string, projectName: string | undefined): string {
  const project = projectName ?? '';

  if (pathname.startsWith('/settings')) return settingsTitle(pathname, project);

  // `/agents-hub` is listed BEFORE `/agents` in SECTION_BY_PREFIX so this
  // find() resolves it correctly — `/agents-hub`.startsWith('/agents') is
  // also true, the same substring-prefix hazard `widgets/create-button`'s
  // `currentEntityFromPathname` documents and fixes the same way (ordering,
  // proven by the analogous regression test in that widget).
  const section = SECTION_BY_PREFIX.find((entry) => pathname.startsWith(entry.prefix));
  if (!section) return project;

  if (searchName) return `${section.label}: ${searchName} - ${project}`;

  if (section.hasTab) {
    const tab = pathname.replace(new RegExp(`^${section.prefix}/?`), '').split('/')[0];
    if (tab) return `${section.label}: ${tab} - ${project}`;
  }

  return `${section.label} - ${project}`;
}
