import { useState } from 'react';

import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { FlowNode } from '../lib/flow-editor/reactFlowTypes';
import { useFlowEditorReset } from './useFlowEditorLifecycle';

function resetArgs() {
  const initialNodes: FlowNode[] = [
    { id: 'authored', type: 'code', position: { x: 60, y: 870 }, data: {} },
    { id: 'END', type: 'END', position: { x: 60, y: 1043 }, data: {} },
  ];
  return {
    resetFlag: true,
    initialNodes,
    initialEdges: [{ id: 'authored-END', source: 'authored', target: 'END' }],
    setFlowNodes: vi.fn(),
    setFlowEdges: vi.fn(),
    onResetRunParseStatus: vi.fn(),
    onResetHandled: vi.fn(),
    persistNodes: vi.fn(),
    persistEdges: vi.fn(),
    fitView: vi.fn(),
  };
}

describe('reset completion and cancellation', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('completes persistence and fit when acknowledging the reset clears its flag', async () => {
    const args = resetArgs();
    const { result } = renderHook(() => {
      const [resetFlag, setResetFlag] = useState(true);
      useFlowEditorReset({ ...args, resetFlag, onResetHandled: () => setResetFlag(false) });
      return resetFlag;
    });
    await act(() => vi.advanceTimersByTime(149));
    expect(args.persistNodes).not.toHaveBeenCalled();
    expect(args.fitView).not.toHaveBeenCalled();
    expect(result.current).toBe(true);
    await act(() => vi.advanceTimersByTime(1));
    expect(args.persistNodes).toHaveBeenCalledWith(args.initialNodes);
    expect(args.persistEdges).toHaveBeenCalledWith(args.initialEdges);
    expect(args.fitView).toHaveBeenCalledTimes(1);
    expect(result.current).toBe(false);
  });

  it('cancels deferred persistence and fit when the reset flag is cleared before completion', async () => {
    const args = resetArgs();
    const { rerender } = renderHook(
      ({ resetFlag }) => useFlowEditorReset({ ...args, resetFlag }),
      { initialProps: { resetFlag: true } },
    );
    await act(() => vi.advanceTimersByTime(149));
    rerender({ resetFlag: false });
    await act(() => vi.advanceTimersByTime(1));
    expect(args.persistNodes).not.toHaveBeenCalled();
    expect(args.persistEdges).not.toHaveBeenCalled();
    expect(args.fitView).not.toHaveBeenCalled();
    expect(args.onResetHandled).not.toHaveBeenCalled();
  });

  it('cancels deferred persistence and fit when the editor unmounts before completion', async () => {
    const args = resetArgs();
    const { unmount } = renderHook(() => useFlowEditorReset(args));
    await act(() => vi.advanceTimersByTime(149));
    unmount();
    await act(() => vi.advanceTimersByTime(1));
    expect(args.persistNodes).not.toHaveBeenCalled();
    expect(args.persistEdges).not.toHaveBeenCalled();
    expect(args.fitView).not.toHaveBeenCalled();
    expect(args.onResetHandled).not.toHaveBeenCalled();
  });
});
