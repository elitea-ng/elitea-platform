import { getConfig } from '@/shared/config';

/**
 * The absolute URL a share token opens.
 *
 * # The bug this closes
 *
 * The dialog built `${origin}/shared/chat/${token}`, and that address does not
 * exist. `/shared/chat/:token` is a ROUTER path, and this router is mounted at
 * `VITE_BASE_URI` (`/app/` in every deployed build), so the page really lives
 * at `/app/shared/chat/:token`. Nothing in the app noticed, because the person
 * who creates a link never opens it: the URL was copied to the owner's
 * clipboard and shown on the owner's screen, both of which looked perfect. Its
 * RECIPIENT got the edge's own plain-text 404 — the deployment edge routes
 * `/app` to the SPA and nothing at the root to anything, so the request never
 * reached a page that could even report a bad token.
 *
 * That is why a share link is built here rather than inline: the one string in
 * this app whose only reader is somebody else has to be assembled from the
 * same basename the router is mounted with.
 *
 * # Why the basename is read here and not imported
 *
 * `app/providers/basename.ts` is the layer above `features/`
 * (`no-upward-from-features`, `.dependency-cruiser.cjs`), so this slice reads
 * `shared/config` directly, exactly as `features/agents/lib/basename.ts` and
 * `features/agent-lifecycle/lib/shareLink.ts` already do — same
 * `import.meta.env.DEV` branch, same `vite_base_uri` key, same safe `''`
 * fallback.
 */
export function getConversationShareBasename(): string {
  if (import.meta.env.DEV) return '';
  const result = getConfig();
  return result.status === 'ok' ? result.config.vite_base_uri : '';
}

export interface ShareConversationUrlParts {
  /** `window.location.origin`, passed in so the builder stays pure. */
  readonly origin: string;
  /** The router basepath (`''` in dev, `/app/` in a deployed build). */
  readonly basename: string;
  /** The one-shot token, as the create response handed it back. */
  readonly token: string;
}

export function buildSharedConversationUrl({ origin, basename, token }: ShareConversationUrlParts): string {
  const base = basename.endsWith('/') ? basename.slice(0, -1) : basename;
  return `${origin}${base}/shared/chat/${encodeURIComponent(token)}`;
}
