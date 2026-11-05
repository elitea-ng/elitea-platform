import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { fanoutFixture } from '../../__tests__/fanoutFixtures';
import { applyChatStreamFrame } from '../../lib/chatStreamReducer';
import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import type { SubAgentGroupable } from '../../lib/subAgentGrouping';
import { ApplicationAnswerThinking } from './ApplicationAnswerThinking';

// T-2A3: a Map of more than 16 items streams its members as `fanout_v1`
// `kind: "map"` frames (the Worker labelled them `parallel` before, which the
// 16-member Parallel bound refused). Through the real stream reducer, 17 Map
// member frames become 17 sub-agent instances that the panel groups by status.

const RESPONSE = '10000000-0000-4000-8000-000000000017';
const status = fanoutFixture<Record<string, unknown>>('fanout-member-status-v1');

function mapMember(index: number, total: number, state: 'start' | 'end'): Record<string, unknown> {
  const callId = `fo1_${(index + 1).toString(16).padStart(64, '0')}`;
  return {
    type: 'agent_fanout_member',
    message_id: RESPONSE,
    content: null,
    response_metadata: {
      fanout_member_status_v1: {
        ...status,
        status: state,
        open_interrupts: 0,
        fanout_v1: { activation: '0a1b2c3d4e5f6071', kind: 'map', member: String(index), node: 'map_items', ordinal: index + 1, total },
        parent_agent_call_id: callId,
        parent_agent_name: 'render',
        parent_agent_path: [{ call_id: callId, name: 'render', sibling_ordinal: index + 1 }],
      },
    },
  };
}

function streamed(total: number, ended: number): readonly SubAgentGroupable[] {
  const seed: readonly ChatMessage[] = [
    { id: RESPONSE, role: 'assistant', name: 'Pipeline', content: '', createdAt: '2026-11-05T00:00:00Z', isStreaming: true },
  ];
  const frames = [
    ...Array.from({ length: total }, (_, index) => mapMember(index, total, 'start')),
    ...Array.from({ length: ended }, (_, index) => mapMember(index, total, 'end')),
  ];
  const history = frames.reduce<readonly ChatMessage[]>((state, frame) => applyChatStreamFrame(state, frame), seed);
  return (history[0]?.toolActions ?? []) as unknown as readonly SubAgentGroupable[];
}

const groups = () =>
  screen
    .queryAllByTestId(/chat-answer-nested-agent-accordion-summary-/)
    .map((node) => node.textContent)
    .filter((text) => /^(Waiting|Running|Failed|Completed) \(\d+\)$/u.test(text ?? ''));

describe('Map member frames of kind "map" for more than 16 items', () => {
  it('stream 17 members into 17 instances that the panel groups by status', () => {
    const actions = streamed(17, 10);
    expect(actions).toHaveLength(17);
    renderWithTheme(<ApplicationAnswerThinking isStreaming actions={actions} />);
    expect(groups()).toEqual(['Running (7)', 'Completed (10)']);
  });

  it('stream all 64 members of the largest Map', () => {
    const actions = streamed(64, 64);
    expect(actions).toHaveLength(64);
    renderWithTheme(<ApplicationAnswerThinking isStreaming actions={actions} />);
    expect(groups()).toEqual(['Completed (64)']);
  });
});
