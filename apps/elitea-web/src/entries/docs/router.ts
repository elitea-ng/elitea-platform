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

/** `true` for an href this router must never touch: it has its own scheme
 * (`https://…`, `mailto:…`) and belongs to the browser's normal link
 * handling, not the docs SPA's client-side routing. Deliberately narrow —
 * only the two schemes the MDX content convention (README.md) actually
 * uses — rather than a generic "has a colon" test, so a root-relative slug
 * is never misclassified. */
const EXTERNAL_HREF_RE = /^(?:https?:|mailto:)/i;
export function isExternalHref(href: string): boolean {
  return EXTERNAL_HREF_RE.test(href);
}

/** Prefixes a root-relative pathname (`/menus/chat`, `/content/img/x.webp`)
 * with BASE, unless it is already there — the fix for the defect
 * `docs-shots`/MDX content links land on: content is authored as
 * base-agnostic root-relative paths (README.md's convention), but the built
 * SPA is not always served from `/` (nginx: `/docs/`, GitHub Pages:
 * `/elitea-platform/`), so a raw `<a href="/menus/chat">` or `<img
 * src="/x.webp">` would leave the docs base entirely. Idempotent so it is
 * safe to call on a value that might already be based (e.g. one built by
 * `toPath` upstream). */
export function withBase(pathname: string): string {
  if (pathname.startsWith(BASE)) return pathname;
  return `${BASE}${pathname.replace(/^\//, '')}`;
}

/**
 * Resolves an MDX content `href` the way the README's link convention
 * intends: a root-relative slug (`/menus/chat`, optionally `#anchor`) gets
 * BASE prefixed via `withBase` so both the rendered attribute and the
 * click-delegation in `handleDocsLinkClick` (which only intercepts hrefs
 * already under BASE) resolve to the right page. Left untouched: an
 * external href (`isExternalHref`) — the caller decides `target`/`rel` for
 * those — a bare `#anchor` (native same-page scrolling, no routing
 * involved), and anything that is not root-relative at all (an unexpected
 * shape in content; passed through rather than guessed at).
 */
export function resolveContentHref(href: string): string {
  if (href.startsWith('#') || isExternalHref(href) || !href.startsWith('/')) return href;
  return withBase(href);
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

/** Navigate to `slug` (optionally with a `#anchor` hash), pushing a history
 * entry, and notify listeners. Scrolling is NOT this function's job: `App.tsx`
 * owns it, because the target page's content — and, for a hash link, the
 * heading it needs to scroll to — may still be loading (`content-registry.ts`
 * loads each page's component lazily), so scrolling has to wait for that
 * content to actually mount rather than happening synchronously here. */
function navigate(slug: string, hash = ''): void {
  const path = `${toPath(slug)}${hash}`;
  const current = `${window.location.pathname}${window.location.hash}`;
  if (path !== current) {
    window.history.pushState({}, '', path);
  }
  notify();
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
 * default handling untouched. `url.hash` (e.g. a content link written as
 * `/menus/chat#voice`) is passed through to `navigate` so the target page's
 * heading anchor survives the client-side transition instead of being
 * silently dropped.
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
  navigate(currentSlug(url.pathname) ?? '', url.hash);
}
