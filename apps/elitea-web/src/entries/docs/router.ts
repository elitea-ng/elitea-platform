/**
 * A minimal, base-aware History API router for the docs SPA (PREAMBLE
 * decision 1). No routing library: the whole surface is "given a slug, which
 * page" and "given a click, is it internal", which does not need one.
 *
 * Base-aware: `import.meta.env.BASE_URL` is Vite's own normalized form of
 * the `base` config value (`vite.config.ts`'s `docs` mode sets it from
 * `DOCS_BASE`), always leading- and trailing-slash. Nginx serves the build at
 * `/docs/`; the GitHub Pages workflow builds with `DOCS_BASE=/elitea-platform/`
 * (decision 6) — this module never hardcodes either.
 */

const BASE = import.meta.env.BASE_URL;

/** Slug (no leading slash, `''` for home) → absolute pathname under BASE. */
export function toPath(slug: string): string {
  if (slug === '') return BASE;
  return `${BASE}${slug}`;
}

/** The current location's slug, or `undefined` when the pathname is outside
 * BASE entirely (should not happen in a correctly configured deployment, but
 * a stray request is a 404, not a crash). */
export function currentSlug(pathname: string = window.location.pathname): string | undefined {
  if (!pathname.startsWith(BASE)) return undefined;
  const rest = pathname.slice(BASE.length);
  // Drop a trailing slash left over from `/docs/quick-start/` so it matches
  // the extensionless, no-trailing-slash slugs `nav.ts` declares.
  return rest.endsWith('/') ? rest.slice(0, -1) : rest;
}

export type RouteListener = () => void;

const listeners = new Set<RouteListener>();

function notify(): void {
  for (const listener of listeners) listener();
}

/** Subscribe to navigation (pushState or back/forward); returns the
 * unsubscribe function. */
export function onRouteChange(listener: RouteListener): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** Navigate to `slug`, pushing a history entry, and notify listeners. A
 * navigation to the CURRENT slug is a no-op push (same as clicking the
 * active nav link twice) but still scrolls to top, matching normal link
 * behaviour. */
function navigate(slug: string): void {
  const path = toPath(slug);
  if (path !== window.location.pathname) {
    window.history.pushState({}, '', path);
  }
  notify();
  window.scrollTo({ top: 0 });
}

let popstateWired = false;

/** Wires the `popstate` listener once (back/forward navigation) and returns
 * a matching teardown, for symmetry with `onRouteChange`. Idempotent: a
 * second call is a no-op wiring-wise but still returns a working teardown. */
export function initHistoryListener(): () => void {
  const onPopState = () => notify();
  if (!popstateWired) {
    window.addEventListener('popstate', onPopState);
    popstateWired = true;
  }
  return () => {
    window.removeEventListener('popstate', onPopState);
    popstateWired = false;
  };
}

/**
 * Intercepts a click on an internal `<a href>` (same-origin, under BASE) and
 * routes it through `navigate` instead of a full page load — the whole point
 * of a history-based SPA router. Externally-targeted links (`target="_blank"`,
 * a modifier key held, a `download` attribute) fall through to the browser's
 * default handling untouched.
 */
/** A modifier key or non-primary button means "open in a new tab/window" —
 * the browser's job, not the router's. */
function isModifiedClick(event: MouseEvent): boolean {
  return event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey;
}

/** The nearest `<a>` ancestor of the click target, or `null` when the click
 * did not land inside a link at all. */
function anchorOf(event: MouseEvent): HTMLAnchorElement | null {
  const target = event.target;
  if (!(target instanceof Element)) return null;
  return target.closest('a');
}

/** `anchor` opts itself out of interception: a new tab/window target, or a
 * `download` attribute that must trigger the browser's save flow. */
function anchorOptsOut(anchor: HTMLAnchorElement): boolean {
  return anchor.target !== '' || anchor.hasAttribute('download');
}

/** Resolves `anchor`'s href against the current location and returns it only
 * when it is a same-origin URL under BASE — the one case this router should
 * intercept. Anything else (a bare hash, an unparseable href, a different
 * origin, a path outside this app entirely) is `undefined`, and the caller
 * lets the browser handle the click as normal. */
function internalTargetUrl(anchor: HTMLAnchorElement): URL | undefined {
  const href = anchor.getAttribute('href');
  if (href === null || href.startsWith('#')) return undefined;

  let url: URL;
  try {
    url = new URL(href, window.location.href);
  } catch {
    return undefined;
  }
  if (url.origin !== window.location.origin || !url.pathname.startsWith(BASE)) return undefined;
  return url;
}

export function handleDocsLinkClick(event: MouseEvent): void {
  if (event.defaultPrevented || isModifiedClick(event)) return;

  const anchor = anchorOf(event);
  if (anchor === null || anchorOptsOut(anchor)) return;

  const url = internalTargetUrl(anchor);
  if (url === undefined) return;

  event.preventDefault();
  navigate(currentSlug(url.pathname) ?? '');
}
