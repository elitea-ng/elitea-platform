import { renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

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

  it('clamps an oversized stored width to 50% of window width', () => {
    Object.defineProperty(window, 'innerWidth', { configurable: true, writable: true, value: 900 });
    createStorage('local').set('pipeline.testPanel.width', '700');
    const { result } = renderHook(() => useTestPanelWidth());
    expect(result.current.width).toBe(450);
  });
});
