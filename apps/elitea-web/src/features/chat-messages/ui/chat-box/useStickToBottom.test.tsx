import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useStickToBottom } from './useStickToBottom';

/**
 * jsdom has no layout: the scroll element's geometry is faked as plain
 * numbers, and `ResizeObserver` is captured so a test can say "the content
 * grew" the way the browser would.
 */
const VIEWPORT = 500;
let observerCallbacks: Array<() => void> = [];

function fakeScrollElement(initialHeight: number) {
  const el = document.createElement('div');
  const geometry = { scrollHeight: initialHeight, scrollTop: 0 };
  Object.defineProperty(el, 'clientHeight', { get: () => VIEWPORT });
  Object.defineProperty(el, 'scrollHeight', { get: () => geometry.scrollHeight });
  Object.defineProperty(el, 'scrollTop', {
    get: () => geometry.scrollTop,
    set: (value: number) => {
      geometry.scrollTop = Math.min(value, geometry.scrollHeight - VIEWPORT);
    },
  });
  const grow = (height: number): void => {
    geometry.scrollHeight = height;
    act(() => observerCallbacks.forEach((callback) => callback()));
  };
  const readerScrollsTo = (top: number): void => {
    el.scrollTop = top;
    act(() => {
      el.dispatchEvent(new Event('scroll'));
    });
  };
  return { el, grow, readerScrollsTo };
}

function setup(initialHeight = 1000) {
  const scroll = fakeScrollElement(initialHeight);
  const scrollRef = { current: { getScrollElement: () => scroll.el } };
  const contentRef = { current: document.createElement('ul') };
  const hook = renderHook(
    (shape: { firstId?: string; lastId?: string; count: number }) =>
      useStickToBottom({ scrollRef, contentRef, firstId: shape.firstId, lastId: shape.lastId, count: shape.count }),
    { initialProps: { firstId: 'm1', lastId: 'm4', count: 4 } },
  );
  return { ...scroll, rerender: hook.rerender };
}

beforeEach(() => {
  observerCallbacks = [];
  vi.stubGlobal(
    'ResizeObserver',
    class {
      constructor(callback: () => void) {
        observerCallbacks.push(callback);
      }
      observe(): void {}
      disconnect(): void {}
    },
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('useStickToBottom', () => {
  it('lands at the bottom when a conversation opens', () => {
    const { el } = setup(1000);
    expect(el.scrollTop).toBe(500);
  });

  it('follows a streaming reply that grows in place (message count unchanged)', () => {
    const { el, grow } = setup(1000);
    grow(1800);
    expect(el.scrollTop, 'a reader at the bottom must keep seeing the newest tokens').toBe(1300);
  });

  it('stops following once the reader scrolls up, and resumes when they return to the bottom', () => {
    const { el, grow, readerScrollsTo } = setup(1000);
    readerScrollsTo(100);
    grow(1800);
    expect(el.scrollTop, 'a reader who scrolled up must not be yanked down').toBe(100);

    readerScrollsTo(1300);
    grow(2400);
    expect(el.scrollTop).toBe(1900);
  });

  it('jumps to the bottom on send even when the reader had scrolled up', () => {
    const { el, grow, readerScrollsTo, rerender } = setup(1000);
    readerScrollsTo(100);
    grow(1200);
    rerender({ firstId: 'm1', lastId: 'm6', count: 6 });
    expect(el.scrollTop).toBe(700);
    grow(2000);
    expect(el.scrollTop, 'the new turn then streams with the pin engaged').toBe(1500);
  });

  it('does not yank a reader when an older page is prepended or a placeholder id settles', () => {
    const { el, grow, readerScrollsTo, rerender } = setup(1000);
    readerScrollsTo(0);
    grow(1600);
    rerender({ firstId: 'm0', lastId: 'm4', count: 8 });
    rerender({ firstId: 'm0', lastId: 'server-m4', count: 8 });
    expect(el.scrollTop).toBe(0);
  });
});
