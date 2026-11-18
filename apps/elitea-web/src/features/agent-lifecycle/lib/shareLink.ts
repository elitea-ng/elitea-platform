import { getConfig } from '@/shared/config';

/**
 * The "Share" half of the agent/pipeline lifecycle menu.
 *
 * Measured on production (`next.elitea.ai`, 2026-09-06, read-only): Share is
 * NOT a server call. Both menu items — VERSION → Share and AGENT → Share —
 * write a deep link to the clipboard and raise "The link has been copied to
 * the clipboard.". The two links differ only by the trailing version segment:
 *
 *   agent    https://next.elitea.ai/app/17015/agents/all/1?viewMode=owner&name=…
 *   version  https://next.elitea.ai/app/17015/agents/all/1/1?viewMode=owner&name=…
 *
 * There is no share endpoint, no share token and no `visibility` column behind
 * it (confirmed against the Go service: `applications.shared_owner_id` /
 * `shared_id` are provenance pointers, never a visibility flag). Visibility of
 * an agent is `application_versions.status = 'published'` and nothing else,
 * which is what Publish below controls. So a Share implementation that called
 * a server would be inventing a contract; this one copies a link.
 *
 * The link names its project (#2148). This app keeps the selected project in
 * the browser, not in the path, so `/{entity}/{tab}/{id}` alone opens in the
 * READER'S selected project: a link sent to a colleague (or opened from another
 * project) landed on a different agent that happens to share the id. The
 * project segment goes through the router's project switcher
 * (`routes/$projectId.$`), which selects that project and reloads on the same
 * path: `/{projectId}/{entity}/{tab}/{id}[/{versionId}]`. Without a project id
 * (none selected) the project-less form is the fallback.
 */

/** `agents` and `pipelines` are the two list routes that carry an editor. */
export type LifecycleEntity = 'agents' | 'pipelines';

export interface ShareLinkParts {
  /** `window.location.origin`, passed in so the builder stays pure. */
  readonly origin: string;
  /** The router basepath (`''` in dev, `/app` in a deployed build). */
  readonly basename: string;
  readonly entity: LifecycleEntity;
  /** The project the entity lives in; the link opens it in that project. */
  readonly projectId?: string;
  /** The list tab the editor was opened from; `latest` when the route has none. */
  readonly tab: string;
  readonly entityId: string;
  /** Present for a VERSION share, absent for an ENTITY share. */
  readonly versionId?: string;
  /** Carried as `?name=` the way the baseline does, so a pasted link shows the agent's name while it loads. */
  readonly name?: string;
}

export function buildEntityShareLink(parts: ShareLinkParts): string {
  const basename = parts.basename.endsWith('/') ? parts.basename.slice(0, -1) : parts.basename;
  const scope = parts.projectId === undefined || parts.projectId === '' ? [] : [parts.projectId];
  const segments = [...scope, parts.entity, parts.tab, parts.entityId];
  if (parts.versionId !== undefined && parts.versionId !== '') segments.push(parts.versionId);
  const path = segments.map((segment) => encodeURIComponent(segment)).join('/');
  const query = parts.name === undefined || parts.name === '' ? '' : `?name=${encodeURIComponent(parts.name)}`;
  return `${parts.origin}${basename}/${path}${query}`;
}

/**
 * The router basepath, read the same way `features/agents/lib/basename.ts`
 * reads it and for the same reason: `app/providers/basename.ts` is a layer
 * above `features/`, so this slice may not import it
 * (`no-upward-from-features`, `.dependency-cruiser.cjs`).
 */
export function getLifecycleBasename(): string {
  if (import.meta.env.DEV) return '';
  const result = getConfig();
  return result.status === 'ok' ? result.config.vite_base_uri : '';
}
