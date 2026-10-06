/**
 * Lazy route chunks that fail BECAUSE the page is leaving must not reload it.
 *
 * TanStack Router's `lazyRouteComponent` (installed
 * `@tanstack/react-router@1.170.41`, `dist/esm/lazyRouteComponent.js`) stores
 * a rejected chunk import and, on the next render, calls
 * `window.location.reload()` when `isModuleNotFoundError(error)` holds
 * (`@tanstack/router-core` `utils.js`: the message starts with "Failed to
 * fetch dynamically imported module", "error loading dynamically imported
 * module" or "Importing a module script failed"), once per message per tab
 * (sessionStorage key `tanstack_router_reload:<message>`). That is the right
 * answer to a stale deployment, where the hashed chunk is gone from the
 * server.
 *
 * It is the wrong answer when the import failed because a navigation away
 * from the page is in progress. WebKit stops the current document's loads as
 * soon as a cross-document navigation passes `beforeunload`, so a route
 * chunk still in flight rejects with "Importing a module script failed."
 * while the old page is still alive; the reload then REPLACES the navigation
 * the user (or a Playwright `page.goto`) asked for. In Safari the navigation
 * bounces back to the page being left; in the E2E suite it is
 * `page.goto: Frame load interrupted`.
 *
 * The fix sits at the importer, outside both Vite's `__vitePreload` wrapper
 * and TanStack's reload check: a chunk import that rejects while the page is
 * leaving is DEFERRED instead of rejected. If the page really goes, the
 * promise never settles and nothing reloads. If the page turns out to stay
 * (bfcache restore, or the user is interacting with it again after a
 * cancelled `beforeunload` prompt), every deferred rejection is released and
 * the stock behaviour follows: reload once on a missing chunk, error
 * boundary after that.
 *
 * `vite:preloadError`: no listener is registered on purpose. Vite dispatches
 * it from inside the importer (CSS preload failures and the module import
 * itself, `handlePreloadError`) and rethrows unless a listener calls
 * `preventDefault()`, which would resolve the import with `undefined`. The
 * guard wraps the importer from the OUTSIDE, so it sees Vite's rethrown error
 * exactly as it sees a dev-server rejection, and a CSS-preload failure caused
 * by the same unload is deferred the same way. Adding Vite's suggested
 * "reload on preloadError" listener would re-create this bug for every
 * dynamic import, which is why there is none.
 *
 * Wiring: `vite.config.ts` (`guardedLazyRouteComponent`) prepends an import
 * of `lazyRouteComponent` from THIS module to every route file, and the
 * TanStack code splitter uses an existing `lazyRouteComponent` binding
 * instead of injecting its own (`router-plugin` `compilers.js`:
 * `if (!hasImportedOrDefinedIdentifier(LAZY_ROUTE_COMPONENT_IDENT))`). The
 * admin router imports it from here directly.
 */
import { lazyRouteComponent as tanstackLazyRouteComponent } from '@tanstack/react-router';

type Listener = (event: Event) => void;

interface GuardTarget {
  addEventListener(type: string, listener: Listener, options?: AddEventListenerOptions): void;
}

export interface ChunkLoadGuard {
  /** Wraps a dynamic-import function so an unload-time rejection never settles. */
  guard<T>(importer: () => Promise<T>): () => Promise<T>;
  /** True between a leave signal and the next sign the page is still here. */
  isLeaving(): boolean;
}

/** Signals that a cross-document navigation (or tab close) has started. */
const LEAVE_EVENTS = ['beforeunload', 'pagehide'] as const;
/**
 * Signals that the document is still the one the user is looking at. `focus`
 * covers returning from the browser's own "leave this page?" prompt, whose
 * Stay button is browser chrome and dispatches no input to the page.
 */
const STAY_EVENTS = ['pointerdown', 'keydown', 'focus'] as const;

/**
 * How long a leave signal is believed without a page change. A real
 * navigation tears the document down well inside this; a navigation that
 * never happens (a cancelled unload prompt, a `mailto:` link, a download a
 * browser fires `beforeunload` for) must not hold route chunks pending
 * forever, so the flag clears itself and held loads retry.
 */
export const LEAVE_TIMEOUT_MS = 2000;

export interface ChunkLoadGuardOptions {
  readonly leaveTimeoutMs?: number;
  readonly setTimer?: (callback: () => void, ms: number) => unknown;
  readonly clearTimer?: (handle: unknown) => void;
}

export function createChunkLoadGuard(target: GuardTarget, options: ChunkLoadGuardOptions = {}): ChunkLoadGuard {
  const leaveTimeoutMs = options.leaveTimeoutMs ?? LEAVE_TIMEOUT_MS;
  const setTimer = options.setTimer ?? ((callback: () => void, ms: number) => setTimeout(callback, ms));
  const clearTimer = options.clearTimer ?? ((handle: unknown) => clearTimeout(handle as ReturnType<typeof setTimeout>));
  let leaving = false;
  let leaveTimer: unknown;
  const deferred = new Set<() => void>();

  const stay = () => {
    if (!leaving) return;
    leaving = false;
    if (leaveTimer !== undefined) clearTimer(leaveTimer);
    leaveTimer = undefined;
    const released = [...deferred];
    deferred.clear();
    for (const release of released) release();
  };

  for (const type of LEAVE_EVENTS) {
    target.addEventListener(type, () => {
      leaving = true;
      if (leaveTimer !== undefined) clearTimer(leaveTimer);
      leaveTimer = setTimer(stay, leaveTimeoutMs);
    });
  }
  for (const type of STAY_EVENTS) {
    target.addEventListener(type, stay, { capture: true });
  }
  // A bfcache restore is the same document coming back: it stayed. The
  // initial (non-persisted) pageshow says nothing about a leave.
  target.addEventListener('pageshow', (event) => {
    if ((event as PageTransitionEvent).persisted) stay();
  });

  // An import that fails while the page is leaving is held. If the page does
  // leave, it never settles, so nothing reloads. If the page stays, the import
  // is RETRIED rather than its failure re-thrown: the failure was caused by
  // the unload, so the retry normally succeeds; a genuinely missing chunk
  // fails again — with the page no longer leaving — and reaches TanStack's
  // reload-once path, so stale-deployment recovery is unchanged.
  const attempt = <T>(importer: () => Promise<T>): Promise<T> =>
    importer().catch((error: unknown) => {
      if (!leaving) throw error;
      return new Promise<void>((release) => {
        deferred.add(release);
      }).then(() => attempt(importer));
    });

  return {
    isLeaving: () => leaving,
    guard:
      <T>(importer: () => Promise<T>) =>
      () =>
        attempt(importer),
  };
}

let defaultGuard: ChunkLoadGuard | undefined;

function getDefaultGuard(): ChunkLoadGuard {
  defaultGuard ??= createChunkLoadGuard(window);
  return defaultGuard;
}

/**
 * Drop-in `lazyRouteComponent` whose importer is unload-guarded. Same
 * signature and behaviour as TanStack's in every other respect.
 */
export const lazyRouteComponent: typeof tanstackLazyRouteComponent = (importer, exportName) =>
  tanstackLazyRouteComponent(getDefaultGuard().guard(importer), exportName);
