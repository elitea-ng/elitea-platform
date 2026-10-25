import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { replayView } from '@/features/workspace';
import type { AgentEvent, ContextMeter as Meter } from '@/shared/desktop/workspaceIpc';

import { renderWithTheme as render } from '@/shared/ui/lib/testTheme';

import ContextPanel from './ContextPanel';
import { contextBreakdown, meterOfEvents, updatedAgo } from './contextBreakdown';

const meter = (over: Partial<Meter> = {}): Meter => ({
  used_tokens: 82_500,
  window_tokens: 200_000,
  phase: 'measured',
  compacted: false,
  window_fallback: false,
  compact_at_tokens: 184_000,
  parts: { system_tokens: 11_700, tool_tokens: 19_700, mcp_tool_tokens: 20_300, skill_tokens: 9_700, message_tokens: 21_100 },
  ...over,
});

/** A meter from a host that reports neither the parts nor the trigger. */
const bare = (over: Partial<Meter> = {}): Meter => {
  const { parts: _parts, compact_at_tokens: _at, ...rest } = meter(over);
  return rest;
};

describe('contextBreakdown', () => {
  it('splits the window into the parts, the compaction buffer and the free space', () => {
    const breakdown = contextBreakdown(meter());
    expect(breakdown.percent).toBe(41);
    expect(breakdown.rows.map((row) => row.key)).toEqual(['system', 'tools', 'mcp', 'skills', 'messages', 'buffer', 'free']);
    expect(breakdown.rows.reduce((sum, row) => sum + row.tokens, 0)).toBe(200_000);
    expect(breakdown.rows.find((row) => row.key === 'buffer')?.tokens).toBe(16_000);
    expect(breakdown.rows.find((row) => row.key === 'mcp')?.percent).toBeCloseTo(10.15);
  });

  it('shows one In use row for a host that reports no parts, and no buffer without a trigger', () => {
    const breakdown = contextBreakdown(bare());
    expect(breakdown.rows.map((row) => row.key)).toEqual(['used', 'free']);
    expect(breakdown.rows[1]?.tokens).toBe(117_500);
  });

  it('never shows negative free space past the trigger', () => {
    const breakdown = contextBreakdown({ ...bare({ used_tokens: 190_000 }), compact_at_tokens: 184_000 });
    expect(breakdown.rows.find((row) => row.key === 'free')?.tokens).toBe(0);
    expect(breakdown.rows.find((row) => row.key === 'buffer')?.tokens).toBe(10_000);
  });

  it('finds the newest meter of recorded events and says how old it is', () => {
    const usage = (seq: number, context?: Meter): AgentEvent => ({
      turn_id: 't',
      seq,
      kind: 'usage',
      payload: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2, ...(context === undefined ? {} : { context }) },
    });
    expect(meterOfEvents([usage(0, meter({ used_tokens: 10 })), usage(1, meter({ used_tokens: 20 })), usage(2)])?.used_tokens).toBe(20);
    expect(meterOfEvents([usage(0)])).toBeUndefined();
    const now = Date.parse('2026-10-21T10:15:00.000Z');
    expect(updatedAgo('2026-10-21T10:14:30.000Z', now)).toBe('Updated just now');
    expect(updatedAgo('2026-10-21T10:10:00.000Z', now)).toBe('Updated 5 min ago');
    expect(updatedAgo(undefined, now)).toBeNull();
  });

  it('follows the latest usage and context events of a turn', () => {
    const context = (seq: number, over: Partial<Meter>): AgentEvent => ({ turn_id: 't', seq, kind: 'context', payload: meter(over) });
    expect(replayView([context(0, { phase: 'compacting' }), context(1, { phase: 'compacted', used_tokens: 30_000, compacted: true })]).context?.used_tokens).toBe(30_000);
  });
});

describe('ContextPanel', () => {
  it('shows the share in use and a row per part', () => {
    render(<ContextPanel meter={meter()} />);
    expect(screen.getByTestId('context-panel')).toHaveAttribute('data-percent', '41');
    expect(screen.getByText('41% used')).toBeInTheDocument();
    expect(screen.getByTestId('context-row-mcp')).toHaveTextContent('MCP tools');
    expect(screen.getByTestId('context-row-mcp')).toHaveTextContent('10.2%');
    expect(screen.getByTestId('context-row-free')).toHaveTextContent('Free space');
  });

  it('says Summarizing while a summary call runs and names a guessed window', () => {
    render(<ContextPanel meter={meter({ phase: 'compacting', window_fallback: true })} />);
    expect(screen.getByText('Summarizing the conversation…')).toBeInTheDocument();
    expect(screen.getByText('The model’s window is unknown: 128K is assumed.')).toBeInTheDocument();
  });

  it('asks for a message before any model call reported', () => {
    render(<ContextPanel meter={undefined} />);
    expect(screen.getByTestId('context-panel-empty')).toBeInTheDocument();
  });
});
