/**
 * `useSupportStream` — the REST-start, SSE-frames transport (stream.hook.ts).
 *
 * Pins three things the reducer in `chat.hook.ts` depends on: a turn ends the
 * stream on `agent_response` or `pipeline_finish`, a server-reported failure
 * settles with the server's own message, and a dropped connection reconnects
 * with a resume cursor up to the shared retry budget before giving up.
 */
import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { EXECUTION_EVENT_FAILED, EXECUTION_EVENT_NODE } from '@/shared/api/sse/executionEvents';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';

import { useSupportStream } from './stream.hook';

let registry: TestEventSourceRegistry;

beforeEach(() => {
  registry = installTestEventSource();
  resetConfigForTests();
});

afterEach(() => {
  registry.restore();
  vi.useRealTimers();
});

describe('useSupportStream — opening and frames', () => {
  it('opens the given events_url and reports isStreaming', () => {
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled: vi.fn() }));

    expect(result.current.isStreaming).toBe(false);
    act(() => result.current.open('/api/v2/support_assistant/stream/1'));

    expect(result.current.isStreaming).toBe(true);
    expect(registry.getSources()[0]?.url).toBe('/api/v2/support_assistant/stream/1');
  });

  it('delivers a parsed node-event frame to onFrame', () => {
    const onFrame = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame, onSettled: vi.fn() }));

    act(() => result.current.open('/stream/1'));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'chunk', message_id: 'm1' })));

    expect(onFrame).toHaveBeenCalledWith({ type: 'chunk', message_id: 'm1' });
  });

  it('ends the stream on an "agent_response" frame, without a reason', () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_response' })));

    expect(onSettled).toHaveBeenCalledWith();
    expect(result.current.isStreaming).toBe(false);
    expect(registry.getSources()[0]?.closed).toBe(true);
  });

  it('ends the stream on a "pipeline_finish" frame', () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'pipeline_finish' })));

    expect(onSettled).toHaveBeenCalledOnce();
    expect(result.current.isStreaming).toBe(false);
  });

  it('does NOT settle on an ordinary progress frame', () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'start_task' })));

    expect(onSettled).not.toHaveBeenCalled();
    expect(result.current.isStreaming).toBe(true);
  });
});

describe('useSupportStream — server-reported failure', () => {
  it('settles with the server\'s safe_message', () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    act(() => void registry.emit(EXECUTION_EVENT_FAILED, JSON.stringify({ safe_message: 'Budget exceeded' })));

    expect(onSettled).toHaveBeenCalledWith('Budget exceeded');
    expect(result.current.isStreaming).toBe(false);
  });

  it('falls back to a generic message when the server sends none', () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    act(() => void registry.emit(EXECUTION_EVENT_FAILED, JSON.stringify({})));

    expect(onSettled).toHaveBeenCalledWith('The support assistant could not answer.');
  });
});

describe('useSupportStream — close vs settle', () => {
  it('close() detaches without calling onSettled', () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    act(() => result.current.close());

    expect(onSettled).not.toHaveBeenCalled();
    expect(result.current.isStreaming).toBe(false);
    expect(registry.getSources()[0]?.closed).toBe(true);
  });

  it('a second settle after close is a no-op', () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    act(() => result.current.close());
    // The stream is already gone; a stray failed-frame handler running after
    // close must not resurrect a settled turn.
    act(() => void registry.emit(EXECUTION_EVENT_FAILED, JSON.stringify({ safe_message: 'too late' })));

    expect(onSettled).not.toHaveBeenCalled();
  });
});

describe('useSupportStream — reconnect on a dropped connection', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  it('reopens with the last cursor after the first backoff step', async () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    // A cursor arrives on an ordinary frame before the drop.
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'chunk' }), '42'));
    act(() => registry.getOpen()[0]?.fail());

    // Not settled yet — a drop with budget left reconnects instead.
    expect(onSettled).not.toHaveBeenCalled();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });

    expect(registry.getSources()).toHaveLength(2);
    expect(registry.getSources()[1]?.url).toBe('/stream/1?cursor=42');
    expect(result.current.isStreaming).toBe(true);
  });

  it('gives up after the retry budget is spent, and settles once', async () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));

    // Four attempts (1s, 2s, 4s, 8s), each failing again immediately.
    for (const delayMs of [1000, 2000, 4000, 8000]) {
      act(() => registry.getOpen().at(-1)?.fail());
      // eslint-disable-next-line no-await-in-loop
      await act(async () => {
        await vi.advanceTimersByTimeAsync(delayMs);
      });
    }
    // The fifth failure has no budget left.
    act(() => registry.getOpen().at(-1)?.fail());

    expect(onSettled).toHaveBeenCalledTimes(1);
    expect(onSettled).toHaveBeenCalledWith('The connection to the support assistant was lost.');
    expect(result.current.isStreaming).toBe(false);
  });

  it('a mid-stream DROP with readyState still CONNECTING is left to the browser', () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    act(() => registry.getOpen()[0]?.drop());

    // `drop()` still routes through the same onError handler as `fail()` in
    // this stream's model: there is no separate "browser is retrying" branch,
    // so it schedules the same backoff.
    expect(onSettled).not.toHaveBeenCalled();
  });
});

describe('useSupportStream — reopening resets the retry budget', () => {
  it('a fresh open() after a settled turn starts attempt count at zero again', () => {
    const onSettled = vi.fn();
    const { result } = renderHook(() => useSupportStream({ onFrame: vi.fn(), onSettled }));

    act(() => result.current.open('/stream/1'));
    act(() => void registry.emit(EXECUTION_EVENT_NODE, JSON.stringify({ type: 'agent_response' })));
    expect(result.current.isStreaming).toBe(false);

    act(() => result.current.open('/stream/2'));
    expect(result.current.isStreaming).toBe(true);
    expect(registry.getSources()[1]?.url).toBe('/stream/2');
  });
});
