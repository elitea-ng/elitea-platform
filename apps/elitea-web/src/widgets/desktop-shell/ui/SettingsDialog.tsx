/**
 * Settings as a dialog over the app, not a page in the main column.
 *
 * The URL stays `/settings/…`: every link, redirect and search parameter the
 * settings routes carry works as on the web, and the dialog only decides
 * where the route draws. A router has one location, so the page the person
 * came from cannot stay mounted behind it; the main column is kept as a still
 * picture instead ({@link PageSnapshot}), taken as the router sets off, and
 * closing the dialog goes back to that page's address.
 */
import { useEffect, useLayoutEffect, useRef, type ReactNode, type RefObject } from 'react';

import { useNavigate, useRouter, useRouterState, type AnyRoute } from '@tanstack/react-router';

import CloseIcon from '@mui/icons-material/Close';
import Box from '@mui/material/Box';
import Dialog from '@mui/material/Dialog';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

function isSettingsPath(pathname: string): boolean {
  return pathname === '/settings' || pathname.startsWith('/settings/');
}

/** Elements a copy would restart (a frame reloads, media plays) rather than show. */
const LIVE_ELEMENTS = 'iframe, video, audio, object, embed';

export interface Snapshot {
  readonly node: HTMLElement;
  readonly restoreScroll: () => void;
}

type Scrolled = readonly [copy: Element, top: number, left: number];

function outside(rect: DOMRect, view: DOMRect): boolean {
  return rect.bottom < view.top || rect.top > view.bottom || rect.right < view.left || rect.left > view.right;
}

/**
 * Copies only what is on screen. A long thread is tens of thousands of
 * elements, and drawing all of them again cost ~160ms on every open; an
 * element wholly off screen keeps its box (its own classes, its measured
 * size) and loses its content, so the visible part and the scroll offsets
 * stay where they were. An empty box (`display: contents`, a hidden one)
 * says nothing about its children's place, so those are walked.
 */
function prune(original: Element, copy: Element, view: DOMRect, scrolled: Scrolled[]): void {
  if (original.scrollTop !== 0 || original.scrollLeft !== 0) scrolled.push([copy, original.scrollTop, original.scrollLeft]);
  const originals = original.children;
  const copies = copy.children;
  for (let index = 0; index < originals.length; index += 1) {
    const from = originals[index];
    const to = copies[index];
    if (from === undefined || !(to instanceof HTMLElement)) continue;
    const rect = from.getBoundingClientRect();
    if (rect.width > 0 && rect.height > 0 && outside(rect, view)) {
      to.replaceChildren();
      to.style.boxSizing = 'border-box';
      to.style.width = `${rect.width}px`;
      to.style.height = `${rect.height}px`;
    } else {
      prune(from, to, view, scrolled);
    }
  }
}

/** A still, inert copy of what `source` shows, with the scroll offsets it had. */
function freeze(source: HTMLElement): Snapshot {
  const node = source.cloneNode(true) as HTMLElement;
  const scrolled: Scrolled[] = [];
  const view = source.firstElementChild?.getBoundingClientRect() ?? source.getBoundingClientRect();
  prune(source, node, view, scrolled);
  for (const copy of node.querySelectorAll('[id], [data-testid]')) {
    // The live page comes back on close: the copy must not answer to its ids or test ids meanwhile.
    copy.removeAttribute('id');
    copy.removeAttribute('data-testid');
  }
  for (const live of node.querySelectorAll(LIVE_ELEMENTS)) live.remove();
  const restoreScroll = (): void => {
    for (const [copy, top, left] of scrolled) {
      copy.scrollTop = top;
      copy.scrollLeft = left;
    }
  };
  return { node, restoreScroll };
}

/**
 * Takes the copy of `main` as the router starts towards Settings, while the
 * page is still on screen and laid out. Taken any later — as the page
 * unmounts — the settings page's new styles are already in the document and
 * measuring the old page restyles all of it (~180ms on a long thread, the
 * first time). Leaving Settings drops the copy.
 */
export function useSnapshotOnOpen(main: RefObject<HTMLElement | null>, into: RefObject<Snapshot | null>): void {
  const router = useRouter();
  useEffect(
    () =>
      router.subscribe('onBeforeNavigate', ({ toLocation }) => {
        if (!isSettingsPath(toLocation.pathname)) {
          into.current = null;
        } else if (into.current === null && main.current !== null) {
          into.current = freeze(main.current);
        }
      }),
    [router, main, into],
  );
}

/** The page from before Settings opened, as a picture: nothing in it can be focused, clicked or read out. */
export function PageSnapshot({ from }: { from: RefObject<Snapshot | null> }): React.JSX.Element {
  const ref = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    const host = ref.current;
    const snapshot = from.current;
    if (host === null || snapshot === null) return undefined;
    host.replaceChildren(snapshot.node);
    snapshot.restoreScroll();
    return () => host.replaceChildren();
  }, [from]);
  // In the main column's place; empty when Settings was the first page (nothing to show behind it).
  return <Box ref={ref} aria-hidden inert sx={{ flex: 1, minWidth: 0, height: '100vh', display: 'flex', overflow: 'clip' }} />;
}

/** How long after the frame mounts Settings' code is loaded, out of the way of the first page's own. */
const PRELOAD_DELAY_MS = 1000;

/**
 * Loads the code of every Settings route ahead of the first open. Each route
 * is a chunk of its own (`autoCodeSplitting`), and the first open waited
 * ~0.8s on them in the app (its route log: `/settings [pending]` to the
 * redirect); loaded, the dialog opens at once. `preloadRoute` was not enough:
 * it loads one route's chain and drops it after a while.
 */
export function usePreloadSettings(): void {
  const router = useRouter();
  useEffect(() => {
    const timer = window.setTimeout(() => {
      // Route ids carry the pathless shell layout (`/_shell/settings/secrets`); the path is what is left.
      for (const [id, route] of Object.entries(router.routesById as unknown as Record<string, AnyRoute>)) {
        if (isSettingsPath(id.replace(/^\/_shell/, ''))) void Promise.resolve(router.loadRouteChunk(route)).catch(() => undefined);
      }
    }, PRELOAD_DELAY_MS);
    return () => window.clearTimeout(timer);
  }, [router]);
}

/**
 * Settings is on screen once its route has resolved, not when the address
 * changes: while the router loads it, the matches are still the page before,
 * and a dialog opened on the address alone would mount that page again inside it.
 */
export function useSettingsOnScreen(): boolean {
  return useRouterState({ select: (state) => isSettingsPath(state.matches.at(-1)?.pathname ?? '') });
}

/** The last address outside Settings, to return to on close. */
export function useReturnAddress(): RefObject<string | undefined> {
  const href = useRouterState({ select: (state) => (isSettingsPath(state.location.pathname) ? undefined : state.location.href) });
  const last = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (href !== undefined) last.current = href;
  }, [href]);
  return last;
}

export interface SettingsDialogProps {
  readonly children: ReactNode;
  /** Where closing goes: the page Settings opened over, else the app's start. */
  readonly returnTo: RefObject<string | undefined>;
  /** The frame's notices, shown in the dialog while the page behind is a picture. */
  readonly banners?: ReactNode;
}

export function SettingsDialog({ children, returnTo, banners }: SettingsDialogProps): React.JSX.Element {
  const navigate = useNavigate();
  const close = (): void => {
    void navigate({ href: returnTo.current ?? '/' });
  };
  const label = t('desktop.shell.settings', 'Settings');
  return (
    <Dialog
      open
      onClose={close}
      maxWidth={false}
      aria-label={label}
      data-testid="settings-dialog"
      slotProps={{
        // The app's dialogs sit near the top (`MuiDialog` theme); this one is a window over the app, centred.
        container: { sx: { alignItems: 'center' } },
        paper: {
          sx: (theme: Theme) => ({
            position: 'relative',
            width: 'min(68rem, calc(100vw - 4rem))',
            height: 'min(48rem, calc(100vh - 4rem))',
            margin: 0,
            overflow: 'hidden',
            borderRadius: theme.vars.shape.radiusMd,
          }),
        },
      }}
    >
      <Box sx={{ height: '100%', minHeight: 0, overflow: 'auto' }}>{children}</Box>
      {banners}
      <IconButton
        size="small"
        aria-label={t('common.closeAriaLabel', 'Close')}
        data-testid="settings-dialog-close"
        onClick={close}
        sx={{ position: 'absolute', top: 8, insetInlineEnd: 8 }}
      >
        <CloseIcon fontSize="small" />
      </IconButton>
    </Dialog>
  );
}
