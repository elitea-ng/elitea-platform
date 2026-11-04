import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import { ApplicationAnswerThinking } from './ApplicationAnswerThinking';

// T-2A3 commit 8: a Map can run up to 64 items. Above 16 sub-agent
// instances, the thinking panel groups them by status (each group one
// existing sub-agent accordion) instead of one row each; a Parallel (at most
// 16 branches) keeps its rows.

const at = '2026-11-04T10:00:00Z';
const item = (ordinal: number, status: string) => ({
  id: `call-${ordinal}`, type: 'tool', name: 'Worker', original_name: 'Worker', parent_agent_call_id: `call-${ordinal}`,
  sibling_ordinal: ordinal, status, toolMeta: { toolkit_type: 'application' }, created_at: at, timestamp: at,
});
// A member's own work; a failed member is one whose inner work errored (a
// wrapper in `error` is a member paused for resume, shown as Waiting).
const work = (ordinal: number, status: string) => ({
  id: `inner-${ordinal}`, type: 'tool', name: 'search', status, parent_agent_name: 'Worker', parent_agent_call_id: `call-${ordinal}`,
  parent_agent_path: [{ name: 'Worker', call_id: `call-${ordinal}`, sibling_ordinal: ordinal }], created_at: at, timestamp: at,
});
const actions = (list: object[]) => list as unknown as SubAgentGroupable[];
const summaries = () => screen.queryAllByTestId(/chat-answer-nested-agent-accordion-summary-/).map((node) => node.textContent);

describe('ApplicationAnswerThinking status grouping', () => {
  it('groups more than 16 instances by status, in a fixed order, with counts', () => {
    const list = [
      ...Array.from({ length: 10 }, (_, index) => item(index + 1, 'complete')),
      ...Array.from({ length: 4 }, (_, index) => [item(index + 11, 'complete'), work(index + 11, 'error')]).flat(),
      ...Array.from({ length: 3 }, (_, index) => item(index + 15, 'processing')),
      ...Array.from({ length: 2 }, (_, index) => item(index + 18, 'error')),
    ];
    renderWithTheme(<ApplicationAnswerThinking isStreaming actions={actions(list)} />);
    const groups = summaries().filter((text) => /^(Waiting|Running|Failed|Completed) \(\d+\)$/u.test(text ?? ''));
    expect(groups).toEqual(['Waiting (2)', 'Running (3)', 'Failed (4)', 'Completed (10)']);
  });

  it('keeps one row per instance up to 16 instances', () => {
    const list = Array.from({ length: 16 }, (_, index) => item(index + 1, 'complete'));
    renderWithTheme(<ApplicationAnswerThinking isStreaming actions={actions(list)} />);
    expect(summaries().some((text) => /^(Waiting|Running|Failed|Completed) \(/u.test(text ?? ''))).toBe(false);
    expect(summaries()).toHaveLength(16);
  });
});
