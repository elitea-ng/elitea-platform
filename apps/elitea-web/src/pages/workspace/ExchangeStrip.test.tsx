/**
 * The exchange controls of a message addressed to several agents (ADR-0061 decision 2c, issue 3058): the strip reads
 * Now/Next from the host's order (`status.floor.exchange`, then `exchange`), Stop exchange and Skip go to the host,
 * the strip goes once the turn ends with no agent after it, and the person's stop is a system line. Steering and
 * approvals name the agent whose turn runs.
 */
import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { approval, replayView } from '@/features/workspace';
import { applyChainEvent } from '@/features/workspace/model/chainEvents';
import { StopNotices } from '@/features/workspace/ui/ChainNotices';
import type { AgentEvent, ApprovalRequestPayload } from '@/shared/desktop/workspaceIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ExchangeStrip, exchangeOf } from './ExchangeStrip';
import { addressedText } from './SteeringQueue';

const AGENTS = [
  { participantId: 1, name: 'Mira' },
  { participantId: 2, name: 'CI Doctor' },
  { participantId: 3, name: 'Docs Writer' },
];

function event(turnId: string, seq: number, rest: Omit<AgentEvent, 'turn_id' | 'seq'>): AgentEvent {
  return { turn_id: turnId, seq, ...rest } as AgentEvent;
}

function running(turnId: string, participantId: number, name: string, position: number, exchange: number[]): AgentEvent {
  return event(turnId, 1, { kind: 'status', payload: { phase: 'running', floor: { position, targets: 3, participant_id: participantId, agent_name: name, executor: 'local', exchange } } });
}

function done(turnId: string, continuing: boolean): AgentEvent {
  return event(turnId, 9, { kind: 'done', payload: { committed: false, conversation_id: 'c1', message_ids: [], changed_files: 0, ...(continuing ? { continuing: true } : {}) } });
}

function fakeIpc() {
  return { stopExchange: vi.fn().mockResolvedValue(undefined), skipInExchange: vi.fn().mockResolvedValue([3]) };
}

describe('the exchange strip (issue 3058)', () => {
  it('shows Now and Next from a three-target start, updates on the next turn and goes on a done with no agent after it', () => {
    const first = { turnId: 't1', view: replayView([running('t1', 1, 'Mira', 0, [2, 3])]) };
    expect(exchangeOf(first, AGENTS)).toEqual({ turnId: 't1', now: 'Mira', next: [{ participantId: 2, name: 'CI Doctor' }, { participantId: 3, name: 'Docs Writer' }] });

    const ipc = fakeIpc();
    const { rerender } = renderWithTheme(<ExchangeStrip exchange={exchangeOf(first, AGENTS)} ipc={ipc} />);
    expect(screen.getByTestId('workspace-exchange-now')).toHaveTextContent('Now: Mira');
    expect(screen.getAllByTestId('workspace-exchange-next').map((chip) => chip.textContent)).toEqual(['CI Doctor', 'Docs Writer']);

    // Mira is done and the host continues: still shown until the next turn starts.
    expect(exchangeOf({ turnId: 't1', view: replayView([running('t1', 1, 'Mira', 0, [2, 3]), done('t1', true)]) }, AGENTS)).toBeDefined();

    const second = { turnId: 't2', view: replayView([running('t2', 2, 'CI Doctor', 1, [3])]) };
    rerender(<ExchangeStrip exchange={exchangeOf(second, AGENTS)} ipc={ipc} />);
    expect(screen.getByTestId('workspace-exchange-now')).toHaveTextContent('Now: CI Doctor');
    expect(screen.getAllByTestId('workspace-exchange-next').map((chip) => chip.textContent)).toEqual(['Docs Writer']);

    const ended = { turnId: 't2', view: replayView([running('t2', 2, 'CI Doctor', 1, [3]), done('t2', false)]) };
    expect(exchangeOf(ended, AGENTS)).toBeUndefined();
    rerender(<ExchangeStrip exchange={exchangeOf(ended, AGENTS)} ipc={ipc} />);
    expect(screen.queryByTestId('workspace-exchange-strip')).toBeNull();
  });

  it('shows nothing for a message to one agent, or the last agent of a message', () => {
    expect(exchangeOf({ turnId: 't1', view: replayView([running('t1', 3, 'Docs Writer', 2, [])]) }, AGENTS)).toBeUndefined();
    expect(exchangeOf({ turnId: 't1', view: replayView([event('t1', 1, { kind: 'status', payload: { phase: 'running' } })]) }, AGENTS)).toBeUndefined();
  });

  it('sends Stop exchange and Skip to the host for the running turn', () => {
    const ipc = fakeIpc();
    renderWithTheme(<ExchangeStrip exchange={{ turnId: 't1', now: 'Mira', next: [{ participantId: 2, name: 'CI Doctor' }, { participantId: 3, name: 'Docs Writer' }] }} ipc={ipc} />);
    fireEvent.click(screen.getAllByTestId('workspace-exchange-skip')[0]!);
    expect(ipc.skipInExchange).toHaveBeenCalledWith('t1', 2);
    fireEvent.click(screen.getByTestId('workspace-exchange-stop'));
    expect(ipc.stopExchange).toHaveBeenCalledWith('t1');
  });

  it('folds the host\'s agent_state and exchange events into the turn', () => {
    const view = replayView([running('t1', 1, 'Mira', 0, [2, 3])]);
    const skipped = applyChainEvent(view, event('t1', 2, { kind: 'exchange', payload: { remaining: [3] } }) as never);
    expect(skipped.agent?.exchange).toEqual([3]);
    const state = applyChainEvent(skipped, event('t1', 3, { kind: 'agent_state', payload: { participant_id: 1, state: 'working', activity: { tool: 'read_file' } } }) as never);
    expect(state.agentState).toEqual({ participant_id: 1, state: 'working', activity: { tool: 'read_file' } });
  });

  it('says the person stopped the exchange as a system line', () => {
    const view = { ...replayView([running('t1', 1, 'Mira', 0, [2, 3])]), floorStopped: { code: 'stopped_by_person', message: 'You stopped the exchange.' } };
    renderWithTheme(<StopNotices view={view} onContinue={() => undefined} />);
    const line = screen.getByTestId('turn-floor-stopped');
    expect(line).toHaveAttribute('data-code', 'stopped_by_person');
    expect(line).toHaveTextContent('You stopped the exchange.');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('says the person skipped the rest by its code, whatever the host message says', () => {
    const view = { ...replayView([running('t1', 1, 'Mira', 0, [2])]), floorStopped: { code: 'skipped_by_person', message: 'reworded by the host' } };
    renderWithTheme(<StopNotices view={view} onContinue={() => undefined} />);
    expect(screen.getByTestId('turn-floor-stopped')).toHaveTextContent('You skipped the remaining agents.');
  });
});

describe('the agent is named (issue 3058)', () => {
  it('in a queued steering message', () => {
    expect(addressedText('use the cache', 'Mira')).toBe('To Mira: use the cache');
    expect(addressedText('use the cache', undefined)).toBe('use the cache');
  });

  it('on the approval of a two-agent thread', () => {
    const request: ApprovalRequestPayload = { request_id: 'r1', tool: 'run_command', title: 'Run a command', detail: 'cargo test', reason: 'runs code', can_remember: false };
    const { rerender } = renderWithTheme(<approval.Dialog request={request} queued={0} onRespond={() => undefined} agentName="CI Doctor" />);
    expect(screen.getByTestId('approval-agent')).toHaveTextContent('CI Doctor asks');
    rerender(<approval.Dialog request={request} queued={0} onRespond={() => undefined} />);
    expect(screen.queryByTestId('approval-agent')).toBeNull();
  });
});
