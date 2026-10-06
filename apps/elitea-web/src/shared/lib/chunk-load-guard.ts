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
/** Signals that the document is still the one the user is looking at. */
const STAY_EVENTS = ['pointerdown', 'keydown'] as const;

export function createChunkLoadGuard(target: GuardTarget): ChunkLoadGuard {
  let leaving = false;
  const deferred = new Set<() => void>();

  const stay = () => {
    if (!leaving) return;
    leaving = false;
    const released = [...deferred];
    deferred.clear();
    for (const release of released) release();
  };

  for (const type of LEAVE_EVENTS) {
    target.addEventListener(type, () => {
      leaving = true;
    });
  }
  for (const type of STAY_EVENTS) {
    target.addEventListener(type, stay, { capture: true });
  }
  // A non-persisted pageshow is the initial load; only a bfcache restore
  // brings back a page whose leave signal already fired.
  target.addEventListener('pageshow', (event) => {
    if ((event as PageTransitionEvent).persisted) stay();
  });

  return {
    isLeaving: () => leaving,
    guard:
      <T>(importer: () => Promise<T>) =>
      () =>
        importer().catch((error: unknown) => {
          if (!leaving) throw error;
          // Settles only if the page turns out to stay (see `stay`).
          return new Promise<void>((release) => {
            deferred.add(release);
          }).then((): never => {
            throw error;
          });
        }),
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
