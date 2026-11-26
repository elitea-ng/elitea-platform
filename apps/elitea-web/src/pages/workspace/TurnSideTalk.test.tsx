/**
 * A Local work turn's side talk (ADR-0061 decisions 3d, 3e and 3f, #3064 #3065): the host's `floor_handed` is a folded
 * "Mira → CI Doctor" side row, a consult is the question and the reply as side rows, a loop guard's `floor_stopped` is
 * the chat's platform line (not a warning), and the caller's avatar waits on the agent it consults.
 */
import { act, fireEvent, renderHook, screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { applyLocalAgentState, useAgentAvatarStates } from '@/features/chat-messages';
import { replayView, TurnTranscript } from '@/features/workspace';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { TurnSideTalk } from './subAgentAvatars';

function event(seq: number, rest: Omit<AgentEvent, 'turn_id' | 'seq'>): AgentEvent {
  return { turn_id: 't1', seq, ...rest } as AgentEvent;
}

const RUNNING = event(1, { kind: 'status', payload: { phase: 'running', floor: { position: 0, targets: 2, participant_id: 77, agent_name: 'Mira', executor: 'local', exchange: [] } } });
const HANDED = event(5, { kind: 'floor_handed', payload: { from: 77, from_name: 'Mira', to: [{ participant_id: 88, name: 'CI Doctor' }], message_excerpt: 'Can you take lint-web?' } });

function render(events: AgentEvent[]) {
  return renderWithTheme(<TurnTranscript view={replayView(events)} busy={false} sideTalk={TurnSideTalk} />);
}

function expandSideTalk(): HTMLElement[] {
  fireEvent.click(screen.getByTestId('chat-side-talk-chip'));
  return screen.getAllByTestId('chat-side-talk-row');
}

describe('TurnSideTalk', () => {
  it('renders a hand-off as a folded side row "Mira → CI Doctor"', () => {
    render([RUNNING, HANDED, HANDED]);
    expect(screen.getByTestId('chat-side-talk-chip')).toHaveTextContent('Side conversation (1)');
    const [row] = expandSideTalk();
    expect(within(row!).getByTestId('chat-side-talk-label')).toHaveTextContent('Mira → CI Doctor');
    expect(row).toHaveTextContent('Can you take lint-web?');
  });

  it('renders a guard stop as a system line, not a warning', () => {
    render([RUNNING, event(6, { kind: 'floor_stopped', payload: { code: 'agent_hops', message: 'The agents handed the floor too many times. Stopped.', guard: true } })]);
    expect(screen.getByTestId('chat-system-line')).toHaveTextContent('The agents handed the floor too many times. Stopped.');
    expect(screen.queryByTestId('turn-floor-stopped')).toBeNull();
  });

  it('renders a consult as the question and the reply, and the caller waits on the agent', () => {
    const waiting = { participant_id: 77, state: 'waiting', activity: { waiting_on: 'agent', peer_participant_id: 88 } };
    const events = [
      RUNNING,
      event(2, { kind: 'consult_started', payload: { caller: 77, callee: 88, callee_name: 'CI Doctor', question_excerpt: 'Is lint-web flaky?', local: true } }),
      event(3, { kind: 'agent_state', payload: waiting }),
    ];
    const view = replayView(events);
    const { result } = renderHook(() => useAgentAvatarStates('thread-consult'));
    act(() => { if (view.agentState !== undefined) applyLocalAgentState('thread-consult', view.agentState); });
    expect(result.current[77]).toMatchObject({ state: 'waiting', activity: { waiting_on: 'agent', peer_participant_id: 88 } });

    render([...events, event(4, { kind: 'consult_finished', payload: { caller: 77, callee: 88, callee_name: 'CI Doctor', reply_excerpt: 'Yes, since Tuesday.', ok: true, local: true } })]);
    expect(screen.getByTestId('chat-side-talk-chip')).toHaveTextContent('Side conversation (2)');
    const [question, reply] = expandSideTalk();
    expect(question).toHaveTextContent('Mira asked CI Doctor: Is lint-web flaky?');
    expect(within(reply!).getByTestId('chat-side-talk-label')).toHaveTextContent('CI Doctor → Mira');
    expect(reply).toHaveTextContent('Yes, since Tuesday.');
  });

  it('says a cloud consult ran without the folder, and a failed reply', () => {
    render([
      RUNNING,
      event(2, { kind: 'consult_started', payload: { caller: 77, callee: 99, callee_name: 'Scribe', question_excerpt: 'Summarise', local: false } }),
      event(3, { kind: 'consult_finished', payload: { caller: 77, callee: 99, callee_name: 'Scribe', reply_excerpt: 'refused', ok: false, local: false } }),
    ]);
    const [question, reply] = expandSideTalk();
    expect(question).toHaveTextContent('(in the cloud, without the folder)');
    expect(reply).toHaveTextContent('No reply: refused');
  });
});
