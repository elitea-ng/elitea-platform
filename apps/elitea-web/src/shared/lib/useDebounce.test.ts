import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useDebounce } from './useDebounce';

describe('useDebounce', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('holds the old value until the new one has been steady for the delay', () => {
    const { result, rerender } = renderHook(({ value }) => useDebounce(value, 300), { initialProps: { value: 'a' } });
    expect(result.current).toBe('a');
    rerender({ value: 'ab' });
    act(() => {
      vi.advanceTimersByTime(299);
    });
    expect(result.current).toBe('a');
    rerender({ value: 'abc' });
    act(() => {
      vi.advanceTimersByTime(299);
    });
    expect(result.current).toBe('a');
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(result.current).toBe('abc');
  });
});
