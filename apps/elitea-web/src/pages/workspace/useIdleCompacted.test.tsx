import { act, renderHook, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { installCompactionIpc, type ContextCompactedEvent } from '@/shared/desktop/compactionSettings';
import { createFakeCompactionIpc } from '@/shared/desktop/compactionSettings.fake';
import type { ContextMeter } from '@/shared/desktop/workspaceIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { IdleCompactedNotice } from './CompactNotices';
import { useIdleCompacted, withIdleCompaction } from './useIdleCompacted';

const event = (over: Partial<ContextCompactedEvent> = {}): ContextCompactedEvent => ({
  workspace_id: 'w1',
  conversation_id: 'c1',
  reason: 'idle',
  before_tokens: 120_000,
  after_tokens: 30_000,
  ...over,
});

const meter = (measuredAt: string): ContextMeter => ({
  used_tokens: 120_000,
  window_tokens: 200_000,
  phase: 'measured',
  compacted: false,
  window_fallback: false,
  measured_at: measuredAt,
  parts: { system_tokens: 5_000, tool_tokens: 10_000, mcp_tool_tokens: 0, skill_tokens: 1_000, message_tokens: 104_000 },
});

describe('useIdleCompacted', () => {
  afterEach(() => installCompactionIpc(undefined));

  it('keeps the open thread’s idle compactions only', async () => {
    const ipc = createFakeCompactionIpc();
    installCompactionIpc(ipc);
    const hook = renderHook(() => useIdleCompacted('w1', 'c1'));
    await act(async () => Promise.resolve());
    act(() => {
      ipc.compact(event({ conversation_id: 'c2' }));
      ipc.compact(event({ reason: 'threshold' }));
    });
    expect(hook.result.current).toBeNull();
    act(() => ipc.compact(event()));
    expect(hook.result.current?.event).toEqual(event());
  });
});

describe('withIdleCompaction', () => {
  it('shows the smaller conversation until a model call measures again', () => {
    const compacted = { event: event(), at: '2026-10-27T12:00:00.000Z' };
    const before = withIdleCompaction(meter('2026-10-27T11:55:00.000Z'), compacted);
    expect(before).toMatchObject({ used_tokens: 30_000, percent: 15, phase: 'compacted', compacted: true, compacted_at: compacted.at });
    expect(before?.parts?.message_tokens).toBe(14_000);
    const after = meter('2026-10-27T12:01:00.000Z');
    expect(withIdleCompaction(after, compacted)).toBe(after);
    expect(withIdleCompaction(undefined, compacted)).toBeUndefined();
  });
});

describe('IdleCompactedNotice', () => {
  it('says the thread was summarized while away, and can be dismissed', async () => {
    const onClose = vi.fn();
    const { rerender } = renderWithTheme(<IdleCompactedNotice shown onClose={onClose} />);
    expect(screen.getByTestId('workspace-compacted-idle')).toHaveTextContent('Summarized while you were away to save cost');
    await userEvent.setup().click(screen.getByRole('button', { name: /close/i }));
    expect(onClose).toHaveBeenCalledOnce();
    rerender(<IdleCompactedNotice shown={false} onClose={onClose} />);
    expect(screen.queryByTestId('workspace-compacted-idle')).toBeNull();
  });
});
