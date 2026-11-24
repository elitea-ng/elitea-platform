import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createStorage } from '@/shared/lib/storage';

import {
  getTestPanelMaxWidth,
  TEST_PANEL_DEFAULT_WIDTH,
  TEST_PANEL_MIN_WIDTH,
  useTestPanelWidth,
} from './useTestPanelWidth';

beforeEach(() => {
  Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 1440 });
  createStorage('local').remove('pipeline.testPanel.width');
});

afterEach(() => {
  createStorage('local').remove('pipeline.testPanel.width');
});

describe('useTestPanelWidth', () => {
  it('computes max width as 50% of container width floored at TEST_PANEL_MIN_WIDTH', () => {
    expect(getTestPanelMaxWidth(1200)).toBe(600);
    expect(getTestPanelMaxWidth(500)).toBe(TEST_PANEL_MIN_WIDTH);
    expect(getTestPanelMaxWidth(0)).toBe(TEST_PANEL_MIN_WIDTH);
  });

  it('starts at TEST_PANEL_DEFAULT_WIDTH when nothing is stored', () => {
    const { result } = renderHook(() => useTestPanelWidth());
    expect(result.current.width).toBe(TEST_PANEL_DEFAULT_WIDTH);
  });

  it('restores a valid persisted width clamped to workspace max width', () => {
    createStorage('local').set('pipeline.testPanel.width', '520');
    const { result } = renderHook(() => useTestPanelWidth());
    expect(result.current.width).toBe(520);
  });

  it('clamps an oversized stored width to 50% of window width synchronously on initial render', () => {
    Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 900 });
    createStorage('local').set('pipeline.testPanel.width', '1e20');
    const { result } = renderHook(() => useTestPanelWidth());
    expect(result.current.width).toBe(450);
  });

  it.each(['NaN', 'Infinity', '-50', '100', 'abc'])('falls back to default width for malformed/out-of-range storage value %s', (bad) => {
    createStorage('local').set('pipeline.testPanel.width', bad);
    const { result } = renderHook(() => useTestPanelWidth());
    expect(result.current.width).toBe(TEST_PANEL_DEFAULT_WIDTH);
  });

  it('persists clamped width, clears inline style.width, and calls onResized when drag ends', () => {
    const onResized = vi.fn();
    const el = document.createElement('div');
    const { result } = renderHook(() => useTestPanelWidth({ onResized }));
    (result.current.containerRef as { current: HTMLDivElement | null }).current = el;

    act(() => {
      result.current.handleResizeStart({ preventDefault: vi.fn(), clientX: 600 } as never);
    });
    act(() => {
      document.dispatchEvent(new MouseEvent('mousemove', { clientX: 500 }));
    });
    act(() => {
      document.dispatchEvent(new MouseEvent('mouseup'));
    });

    expect(onResized).toHaveBeenCalledTimes(1);
    expect(el.style.width).toBe('');
    expect(createStorage('local').get('pipeline.testPanel.width')).toBe('473');
  });
});
