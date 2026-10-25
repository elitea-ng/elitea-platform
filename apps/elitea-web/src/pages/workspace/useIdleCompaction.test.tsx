import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ContextMeter } from '@/shared/desktop/workspaceIpc';

import { IDLE_COMPACT_MS, setIdleCompaction, useIdleCompaction, worthCompacting, type IdleCompactionInput } from './useIdleCompaction';

const NOW = Date.parse('2026-10-24T12:00:00.000Z');

const meter = (over: Partial<ContextMeter> = {}): ContextMeter => ({
  used_tokens: 90_000,
  window_tokens: 200_000,
  phase: 'measured',
  compacted: false,
  window_fallback: false,
  measured_at: new Date(NOW).toISOString(),
  parts: { system_tokens: 10_000, tool_tokens: 20_000, mcp_tool_tokens: 0, skill_tokens: 0, message_tokens: 60_000 },
  ...over,
});

function setup(over: Partial<IdleCompactionInput> = {}) {
  const arm = vi.fn();
  const props: IdleCompactionInput = { meter: meter(), busy: false, armed: false, arm, local: true, ...over };
  const hook = renderHook((input: IdleCompactionInput) => useIdleCompaction(input), { initialProps: props });
  return { arm, hook, props };
}

describe('useIdleCompaction', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });
  afterEach(() => {
    setIdleCompaction(true);
    vi.useRealTimers();
  });

  it('arms the next message once the thread was idle for five minutes', () => {
    const { arm } = setup();
    act(() => {
      vi.advanceTimersByTime(IDLE_COMPACT_MS - 60_000);
    });
    expect(arm).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(90_000);
    });
    expect(arm).toHaveBeenCalledTimes(1);
    act(() => {
      vi.advanceTimersByTime(IDLE_COMPACT_MS);
    });
    expect(arm).toHaveBeenCalledTimes(1);
  });

  it('counts from the end of the last turn, not from its measurement', () => {
    const { arm, hook, props } = setup({ busy: true });
    act(() => {
      vi.advanceTimersByTime(IDLE_COMPACT_MS);
    });
    hook.rerender({ ...props, busy: false });
    act(() => {
      vi.advanceTimersByTime(IDLE_COMPACT_MS - 60_000);
    });
    expect(arm).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(90_000);
    });
    expect(arm).toHaveBeenCalledTimes(1);
  });

  it('leaves a short conversation, a cloud thread and an armed message alone, and stops when switched off', () => {
    const short = setup({ meter: meter({ parts: { system_tokens: 10_000, tool_tokens: 20_000, mcp_tool_tokens: 0, skill_tokens: 0, message_tokens: 5_000 } }) });
    const cloud = setup({ local: false });
    const armed = setup({ armed: true });
    act(() => {
      setIdleCompaction(false);
    });
    const off = setup();
    act(() => {
      vi.advanceTimersByTime(IDLE_COMPACT_MS * 2);
    });
    for (const { arm } of [short, cloud, armed, off]) expect(arm).not.toHaveBeenCalled();
  });

  it('judges a host without parts by the whole request', () => {
    const { parts: _parts, ...bare } = meter();
    expect(worthCompacting({ ...bare, used_tokens: 70_000 })).toBe(true);
    expect(worthCompacting({ ...bare, used_tokens: 30_000 })).toBe(false);
  });
});
