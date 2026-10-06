/**
 * `chunk-load-guard`: a route chunk that fails while the page is leaving must
 * not reload it; one that fails while the page stays keeps TanStack's
 * reload-once behaviour.
 *
 * The reload is observed through TanStack's own guard key: `lazyRouteComponent`
 * writes `tanstack_router_reload:<message>` to sessionStorage immediately
 * before `window.location.reload()` (jsdom's `location` is unforgeable, so the
 * call itself cannot be spied on). Key written = reload attempted.
 */
import { Component, Suspense, type ReactNode } from 'react';

import { lazyRouteComponent as tanstackLazyRouteComponent } from '@tanstack/react-router';
import { act, cleanup, render, screen } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { createChunkLoadGuard, lazyRouteComponent } from './chunk-load-guard';

/** WebKit's message when a navigation cancels a module script load. */
const WEBKIT_CANCELLED = 'Importing a module script failed.';
const RELOAD_KEY = `tanstack_router_reload:${WEBKIT_CANCELLED}`;

const flush = () => act(() => new Promise<void>((resolve) => setTimeout(resolve, 0)));

class Boundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  override state = { error: null as Error | null };
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  override render() {
    return this.state.error ? <p>boundary: {this.state.error.message}</p> : this.props.children;
  }
}

function renderLazy(Lazy: ReturnType<typeof tanstackLazyRouteComponent>) {
  render(
    <Boundary>
      <Suspense fallback={<p>pending</p>}>
        <Lazy />
      </Suspense>
    </Boundary>,
  );
}

const failingImporter = () => Promise.reject(new TypeError(WEBKIT_CANCELLED));

/** A guard bound to its own EventTarget, so tests never share leave state. */
function guardedRoute(importer: () => Promise<{ default: () => ReactNode }>) {
  const target = new EventTarget();
  const guard = createChunkLoadGuard(target);
  const Lazy = tanstackLazyRouteComponent(guard.guard(importer));
  return { target, guard, Lazy };
}

beforeEach(() => {
  window.sessionStorage.clear();
});

describe('createChunkLoadGuard', () => {
  it('passes a successful import through untouched', async () => {
    const guard = createChunkLoadGuard(new EventTarget());
    await expect(guard.guard(() => Promise.resolve({ ok: 1 }))()).resolves.toEqual({ ok: 1 });
  });

  it('rejects at once when the page is not leaving', async () => {
    const guard = createChunkLoadGuard(new EventTarget());
    await expect(guard.guard(failingImporter)()).rejects.toThrow(WEBKIT_CANCELLED);
  });

  it.each(['beforeunload', 'pagehide'])('never settles a failure after %s', async (type) => {
    const target = new EventTarget();
    const guard = createChunkLoadGuard(target);
    target.dispatchEvent(new Event(type));
    expect(guard.isLeaving()).toBe(true);

    const settled = vi.fn();
    guard.guard(failingImporter)().then(settled, settled);
    await flush();
    expect(settled).not.toHaveBeenCalled();
  });

  it.each([
    ['pointerdown', () => new Event('pointerdown')],
    ['keydown', () => new Event('keydown')],
    ['bfcache pageshow', () => Object.assign(new Event('pageshow'), { persisted: true })],
  ])('releases the deferred rejection when %s shows the page stayed', async (_name, makeEvent) => {
    const target = new EventTarget();
    const guard = createChunkLoadGuard(target);
    target.dispatchEvent(new Event('beforeunload'));
    const pending = guard.guard(failingImporter)();

    target.dispatchEvent(makeEvent());
    expect(guard.isLeaving()).toBe(false);
    await expect(pending).rejects.toThrow(WEBKIT_CANCELLED);
  });

  it('ignores the initial (non-persisted) pageshow', () => {
    const target = new EventTarget();
    const guard = createChunkLoadGuard(target);
    target.dispatchEvent(new Event('beforeunload'));
    target.dispatchEvent(Object.assign(new Event('pageshow'), { persisted: false }));
    expect(guard.isLeaving()).toBe(true);
  });
});

/*
 * Each case loads the chunk through `.preload()` (what the router itself calls
 * before rendering a match) and only then renders, so the assertion does not
 * depend on React's Suspense retry timing.
 */
describe('guarded lazy route component vs TanStack reload', () => {
  it('a chunk failure during unload does not reload: the route stays pending', async () => {
    const { target, Lazy } = guardedRoute(failingImporter);
    target.dispatchEvent(new Event('beforeunload'));

    const settled = vi.fn();
    void Lazy.preload?.()?.then(settled);
    renderLazy(Lazy);
    await flush();

    expect(settled).not.toHaveBeenCalled();
    expect(window.sessionStorage.getItem(RELOAD_KEY)).toBeNull();
    expect(screen.getByText('pending')).toBeInTheDocument();
  });

  it('a genuine chunk failure (page not leaving) keeps reload-once', async () => {
    const first = guardedRoute(failingImporter);
    await act(() => first.Lazy.preload?.());
    renderLazy(first.Lazy);
    // Reload attempted: TanStack set its once-per-tab key before reloading.
    expect(window.sessionStorage.getItem(RELOAD_KEY)).toBe('1');
    cleanup();

    // After that reload the same failure reaches the error boundary instead.
    const second = guardedRoute(failingImporter);
    await act(() => second.Lazy.preload?.());
    renderLazy(second.Lazy);
    expect(screen.getByText(`boundary: ${WEBKIT_CANCELLED}`)).toBeInTheDocument();
  });

  it('a failure deferred by a cancelled unload falls back to reload-once', async () => {
    const { target, Lazy } = guardedRoute(failingImporter);
    target.dispatchEvent(new Event('beforeunload'));
    const loading = Lazy.preload?.();
    await flush();
    expect(window.sessionStorage.getItem(RELOAD_KEY)).toBeNull();

    target.dispatchEvent(new Event('pointerdown'));
    await act(() => loading);
    renderLazy(Lazy);
    expect(window.sessionStorage.getItem(RELOAD_KEY)).toBe('1');
  });

  it('the exported lazyRouteComponent renders a loaded chunk normally', async () => {
    const Lazy = lazyRouteComponent(() => Promise.resolve({ Page: () => <p>loaded</p> }), 'Page');
    await act(() => Lazy.preload?.());
    renderLazy(Lazy);
    expect(screen.getByText('loaded')).toBeInTheDocument();
  });
});
