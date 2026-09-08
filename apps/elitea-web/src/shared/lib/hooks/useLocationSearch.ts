import { useSyncExternalStore } from 'react';

/**
 * `window.location.search`, as a value a component can RE-READ when it
 * changes.
 *
 * DEFECT this closes. `shared/ui/EntityCardList` decided the URL was its
 * source of truth for `?view=` and read it at render time — correct as far as
 * it went, and completely inert in practice: a search-only navigation
 * re-renders whichever components subscribed to the router, and the list body
 * is not one of them. E2E measured it on the agents and pipelines list pages:
 * the toggle took `aria-pressed="true"`, the address bar carried `view=table`,
 * and the tab panel below kept drawing cards for as long as the test waited
 * (`e2e/journeys/agents/agents.list.spec.ts` J14e,
 * `e2e/journeys/pipelines/pipelines.dashboard.spec.ts` J16d).
 *
 * A render-time read of a value that changes without a re-render is a value
 * nobody reads a second time. This hook makes the same source reactive.
 *
 * NOT the router's `useSearch`: the readers are `shared/ui` components that
 * page unit tests mount WITHOUT a `RouterProvider`, where `useSearch` throws
 * mid-render — see `EntityCardList`'s own note. `window.location` is available
 * to every one of them, and the router writes it synchronously, so it stays
 * the one key both the toggle and the list read.
 *
 * The subscription covers both ways the value can move: `popstate` (Back /
 * Forward, and `history.go`) and the `pushState`/`replaceState` calls the
 * router itself makes, which fire no event of their own and are therefore
 * wrapped once, process-wide, the first time anything subscribes.
 */
const listeners = new Set<() => void>();
let historyPatched = false;

function notify(): void {
  for (const listener of listeners) listener();
}

function patchHistory(): void {
  if (historyPatched || typeof window === 'undefined') return;
  historyPatched = true;
  const patch = (method: 'pushState' | 'replaceState'): void => {
    const original = window.history[method].bind(window.history);
    window.history[method] = (...args: Parameters<History['pushState']>): void => {
      original(...args);
      notify();
    };
  };
  patch('pushState');
  patch('replaceState');
}

function subscribe(listener: () => void): () => void {
  patchHistory();
  listeners.add(listener);
  window.addEventListener('popstate', listener);
  return () => {
    listeners.delete(listener);
    window.removeEventListener('popstate', listener);
  };
}

function getSnapshot(): string {
  return typeof window === 'undefined' ? '' : window.location.search;
}

/** No URL exists while pre-rendering; every reader must already handle the "key absent" case. */
function getServerSnapshot(): string {
  return '';
}

/** @public The live query string, re-read on every history change. */
export function useLocationSearch(): string {
  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}
