import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { replayView } from '@/features/workspace';
import type { AgentEvent, ContextMeter as Meter } from '@/shared/desktop/workspaceIpc';

import ContextMeter, { describeMeter } from './ContextMeter';

const meter = (over: Partial<Meter> = {}): Meter => ({
  used_tokens: 124_000,
  window_tokens: 200_000,
  phase: 'measured',
  compacted: false,
  window_fallback: false,
  ...over,
});

describe('ContextMeter', () => {
  it('shows how full the window is', () => {
    render(<ContextMeter meter={meter()} />);
    const ring = screen.getByTestId('context-meter');
    expect(ring).toHaveAttribute('data-percent', '62');
    expect(ring).toHaveTextContent('62% of 200K');
    expect(screen.getByLabelText('Context in use: 62% of 200K')).toBe(ring);
  });

  it('names the last compaction and a guessed window in its tooltip text', () => {
    const text = describeMeter(meter({ used_tokens: 40_000, window_tokens: 32_768, compacted: true, compacted_at: '2026-10-21T10:15:00.000Z', window_fallback: true, phase: 'compacted' }));
    expect(text.percent).toBe(100);
    expect(text.detail).toContain('Summarized at');
    expect(text.detail).toContain('32K is assumed');
    expect(describeMeter(meter({ phase: 'compacting' })).detail).toContain('Summarizing');
    expect(describeMeter(meter()).detail).not.toContain('Summarized');
  });

  it('follows the latest usage event of the turn', () => {
    const at = (seq: number) => ({ turn_id: 't', seq });
    const usage = (seq: number, context?: Meter): AgentEvent => ({
      ...at(seq),
      kind: 'usage',
      payload: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2, ...(context === undefined ? {} : { context }) },
    });
    const view = replayView([usage(0, meter({ used_tokens: 10 })), usage(1, meter({ used_tokens: 20, phase: 'compacted', compacted: true })), usage(2)]);
    expect(view.context?.used_tokens).toBe(20);
    expect(view.context?.compacted).toBe(true);
  });
});
