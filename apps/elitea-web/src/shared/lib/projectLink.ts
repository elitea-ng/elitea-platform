import { getConfig } from '@/shared/config';

import { normalizeBasename } from './basename';

/**
 * A link to an entity that names ITS project (#2148).
 *
 * The selected project lives in the browser (session/local storage), not in the
 * URL, so a bare `/toolkits/all/12` opens the toolkit in whatever project the
 * reader has selected: a link copied from project A opened from project B lands
 * on B's toolkit 12 (or a blank page). The router already owns the answer: a
 * path under `/{projectId}/...` is the project switcher (`routes/$projectId.$`),
 * which selects that project and reloads on the same path without the segment.
 * So the shareable form is `{origin}{basename}/{projectId}{path}`.
 */
export interface ProjectLinkParts {
  /** The origin the link is opened on (`window.location.origin` in a browser). */
  readonly origin: string;
  /** The router basepath (`''` in dev, `/app` or `/app/` in a deployed build). */
  readonly basename: string;
  readonly projectId: string;
  /** The page's own path, with or without the basename already in front. */
  readonly pathname: string;
  readonly search?: string;
  readonly hash?: string;
}

export function buildProjectLink({ origin, basename, projectId, pathname, search = '', hash = '' }: ProjectLinkParts): string {
  const base = normalizeBasename(basename);
  const inApp = base !== '' && (pathname === base || pathname.startsWith(`${base}/`)) ? pathname.slice(base.length) : pathname;
  const path = inApp.startsWith('/') ? inApp : `/${inApp}`;
  const projectPrefix = `/${encodeURIComponent(projectId)}`;
  // A page already addressed through the project segment keeps it once.
  const alreadyScoped = path === projectPrefix || path.startsWith(`${projectPrefix}/`);
  const scoped = alreadyScoped ? path : `${projectPrefix}${path === '/' ? '' : path}`;
  return `${origin}${base}${scoped}${search}${hash}`;
}

function deploymentOrigin(serverUrl: string): string | undefined {
  try {
    return new URL(serverUrl).origin;
  } catch {
    return undefined;
  }
}

/**
 * The link to the page the reader is on, in project `projectId`. The desktop
 * shell has no browser origin of its own (`tauri://`), so it shares the
 * deployment's address instead. Without a project the page's own address is
 * the best there is.
 */
export function currentPageProjectLink(projectId: string | undefined): string {
  const { href, origin, pathname, search, hash } = window.location;
  if (projectId === undefined || projectId === '') return href;
  const config = getConfig();
  const deployment = config.status === 'ok' ? config.config : undefined;
  const desktopOrigin = import.meta.env.MODE === 'desktop' && deployment ? deploymentOrigin(deployment.vite_server_url) : undefined;
  const basename = import.meta.env.DEV || !deployment ? '' : deployment.vite_base_uri;
  return buildProjectLink({ origin: desktopOrigin ?? origin, basename, projectId, pathname, search, hash });
}
