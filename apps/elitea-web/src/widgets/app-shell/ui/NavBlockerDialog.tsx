import type { ReactNode } from 'react';
import { useEffect } from 'react';

import { useBlocker } from '@tanstack/react-router';

import { BaseModal } from '@/shared/ui/BaseModal';
import { t } from '@/shared/i18n';

import { useNavBlockerStore } from '../model/navBlocker.store';

/**
 * The navigation blocker (task brief: "a navigation blocker (unsaved-changes
 * guard)"). Ported from `components/UnsavedDialog.jsx`.
 *
 * TanStack Router's `useBlocker({ shouldBlockFn, withResolver: true })`
 * (verified against the installed `@tanstack/react-router@1.170.18`
 * typings, `useBlocker.d.ts`) is a strictly more robust replacement for the
 * old app's react-router `useBlocker` + `BLOCK_NAV_PATTERNS`
 * path-allowlist: it fires for EVERY navigation attempt through the router
 * (including `widgets/sidebar`'s `<Link>` clicks and its per-row "+"
 * `navigate()` calls) with no per-call-site
 * wiring needed, so `SidebarBody`'s old per-click `navigateToPage` special
 * case (SHELL-011's "defers API cache reset" half — see `widgets/sidebar`'s
 * header) does not need re-implementing here.
 *
 * `BLOCK_NAV_PATTERNS` (the old app's path-allowlist — only certain routes
 * are "blockable") is NOT reproduced: no landed Wave-2 unit owns an editor
 * page yet, so there is no path list to allowlist against. `shouldBlockFn`
 * therefore blocks on `isBlockNav || isStreaming` alone, for ANY
 * navigation — the safe default once a real editor sets those flags
 * (blocking too broadly is the fail-safe direction; the old app's
 * allowlist existed to avoid blocking on genuinely safe pages, which today
 * never set the flags in the first place, so the allowlist has no
 * observable effect yet either way).
 *
 * `UnsavedDialog.jsx`'s `blockerFn` DOES reproduce one piece exactly,
 * though: it only blocks when `currentLocation.pathname !==
 * nextLocation.pathname` — a same-pathname navigation (e.g. a search-param
 * update on the route the user is already on) is explicitly let through
 * even while blockable/streaming. TanStack Router's `shouldBlockFn` args
 * (`ShouldBlockFnArgs`, `useBlocker.d.ts`) carry `current`/`next` each with
 * their own `pathname`, so that same pathname-equality check applies here
 * unchanged — this is live today via `processes/chat/model/
 * useStreamingNavBlocker.ts` setting `isStreaming` on the real `/chat`
 * route, which declares several search-only params.
 */
export function NavBlockerDialog(): ReactNode {
  const isBlockNav = useNavBlockerStore((state) => state.isBlockNav);
  const isStreaming = useNavBlockerStore((state) => state.isStreaming);
  const warningMessage = useNavBlockerStore((state) => state.warningMessage);
  const setBlockNav = useNavBlockerStore((state) => state.setBlockNav);
  const setStreamingBlockNav = useNavBlockerStore((state) => state.setStreamingBlockNav);

  /*
   * `getState()` — the LIVE store snapshot — not the `isBlockNav`/
   * `isStreaming` values selected above (#133).
   *
   * `shouldBlockFn` is invoked by the router at navigation time, but a
   * closure over the selected values can only ever see the values from the
   * last COMMITTED render. That is stale exactly when it matters: an editor
   * that lowers the flag and navigates in the same event handler (a
   * successful save — `pages/pipelines/CreatePipeline.tsx`, `pages/agents/
   * CreateApplication.tsx`) sets the store and calls `navigate()` before
   * React has re-rendered this component, so the closure still said "block"
   * and the page's own post-save navigation was blocked by a prompt about
   * changes it had just persisted. Measured: J16 (`pipelines.lifecycle.spec.ts`)
   * timed out on `waitForURL(/\/app\/pipelines\/latest\/\d+/)` the moment
   * the create page started arming the guard.
   *
   * Reading the store here is strictly more correct for every caller —
   * including the chat-embedded editors, which had the same latent
   * staleness — because the question "is there unsaved work RIGHT NOW" can
   * only be answered at the instant the navigation is attempted.
   *
   * The same reasoning applies to `enableBeforeUnload` below. The selected
   * values above still drive the dialog's own render (`warningMessage`) and
   * the `beforeunload` listener, which do need to re-render on change.
   */
  const { status, proceed, reset } = useBlocker({
    shouldBlockFn: ({ current, next }) => {
      const live = useNavBlockerStore.getState();
      return (live.isBlockNav || live.isStreaming) && current.pathname !== next.pathname;
    },
    /*
     * `enableBeforeUnload` DEFAULTS TO `true`, AND THAT PROMPTED ON EVERY PAGE.
     *
     * `useBlocker` installs its own `beforeunload` listener, separate from the
     * hand-rolled one this component adds below. The router's listener does NOT
     * consult `shouldBlockFn`: it walks the registered blockers and prompts
     * when any of them has `enableBeforeUnload !== false`
     * (`useBlocker.d.ts:35` types it `boolean | (() => boolean)`). This
     * component is mounted by the app shell on every route. Leaving the
     * option unset therefore armed the browser's "Leave site?" prompt across
     * the whole application. It fired on every reload, tab close and external
     * navigation, with nothing unsaved and nothing streaming.
     *
     * Measured on a live deployment: on a freshly loaded /app/toolkits,
     * `window.dispatchEvent(new Event('beforeunload', {cancelable: true}))`
     * came back with `defaultPrevented === true`.
     *
     * The predicate reads the LIVE store for the same reason `shouldBlockFn`
     * does: the browser asks at unload time, not at render time.
     */
    enableBeforeUnload: () => {
      const live = useNavBlockerStore.getState();
      return live.isBlockNav || live.isStreaming;
    },
    withResolver: true,
  });

  /*
   * The router installs its `beforeunload` listener inside `createBrowserHistory`,
   * so it exists only under a real browser history. This listener covers the
   * memory-history case as well. It has always been correctly gated. The
   * router's ungated one beside it produced the spurious prompt.
   * Two listeners calling preventDefault on one event still yield one prompt.
   */
  useEffect(() => {
    function onBeforeUnload(event: BeforeUnloadEvent): void {
      if (!isBlockNav && !isStreaming) return;
      event.preventDefault();
    }
    window.addEventListener('beforeunload', onBeforeUnload);
    return () => window.removeEventListener('beforeunload', onBeforeUnload);
  }, [isBlockNav, isStreaming]);

  // Old app: `useNavBlocker.js`'s `resetApiState` has every real reset
  // dispatch commented out — it's a complete no-op (`console.debug` only)
  // — and `UnsavedDialog.jsx`'s `confirmNavigate` only even calls it when
  // `isResetApiState` (a Redux flag defaulting `false`, set by no landed
  // reducer path) is true. Net effect: confirming a blocked navigation in
  // the old app never clears cached API/query data. Reproduced faithfully
  // by NOT touching the query cache here at all, rather than an
  // unconditional `queryClient.clear()` this widget sits under every page
  // and would otherwise force an app-wide refetch/loading flash on every
  // confirmed nav-away.
  const handleConfirm = (): void => {
    proceed?.();
    setBlockNav(false);
    setStreamingBlockNav(false, 'prompt');
  };

  // `reset` is `(() => void) | undefined` per `BlockerResolver`'s
  // discriminated union (`undefined` only in the 'idle' branch — reachable
  // here only in the instant before `status` itself has caught up, since
  // `open` is driven by that same `status`). `BaseModalProps.onClose` is
  // `(() => void) | undefined` too (no-`exactOptionalPropertyTypes`
  // widening needed) but TS cannot narrow one destructured binding from
  // another's value, so a same-shaped fallback keeps the boundary honest
  // instead of asserting reset is always defined.
  const handleClose = reset ?? (() => {});

  return (
    <BaseModal
      variant="simple"
      open={status === 'blocked'}
      title={t('widgets.appShell.navBlocker.title', 'Warning')}
      content={warningMessage}
      onClose={handleClose}
      onConfirm={handleConfirm}
      actions={{ alarm: true }}
      data-testid="nav-blocker-dialog"
    />
  );
}
