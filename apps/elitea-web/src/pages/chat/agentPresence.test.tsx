/**
 * Live agent presence in a participants panel (ADR-0061 decision 4c, issue 3067): only the active agent animates,
 * none does under reduced motion while the labels still change, the hand-off baton is never drawn under reduced
 * motion, and the polite live region says each change once, at most once per agent per second.
 */
import { createRef } from 'react';

import { act, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AvatarStates } from '@elitea/client-core/conversation';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AgentStateAnnouncer, ANNOUNCE_INTERVAL_MS, agentRowViews, announcements, handoffOf, HandoffBaton, type PresenceAgent } from './agentPresence';
import type { ParticipantRowView } from './ParticipantRowControls';

const AGENTS: PresenceAgent[] = [
  { participantId: 1, name: 'Mira', cloud: false },
  { participantId: 2, name: 'CI Doctor', cloud: true },
  { participantId: 3, name: 'Docs Writer', cloud: false },
];

const STATES: AvatarStates = {
  1: { state: 'waiting', since: 1_000, activity: { waiting_on: 'agent', peer_participant_id: 2 } },
  2: { state: 'working', since: 2_000, activity: { tool: 'run_command', tool_kind: 'code_execution', program: 'cargo' } },
  3: { state: 'thinking', since: 1_500 },
};

function mockReducedMotion(reduce: boolean): void {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: reduce && query.includes('prefers-reduced-motion'),
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    onchange: null,
    dispatchEvent: () => false,
  }));
}

beforeEach(() => mockReducedMotion(false));
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('agent rows (issue 3067)', () => {
  it('animates only the active agent of a three-agent thread and labels every one', () => {
    const rows = agentRowViews(AGENTS, STATES, 1, false);
    expect(rows.map((row) => row.motion)).toEqual(['still', 'working', 'still']);
    expect(rows.map((row) => row.label)).toEqual(['Waiting for CI Doctor', 'Running cargo', 'Thinking']);
    expect(rows.map((row) => row.isLead)).toEqual([true, false, false]);
  });

  it('animates none under reduced motion, and the labels still change', () => {
    const before = agentRowViews(AGENTS, STATES, 0, true);
    expect(before.every((row) => row.motion === 'still')).toBe(true);
    const after = agentRowViews(AGENTS, { ...STATES, 2: { state: 'speaking', since: 3_000 } }, 0, true);
    expect(after.every((row) => row.motion === 'still')).toBe(true);
    expect(after[1]?.label).toBe('Writing');
  });

  it('leaves an agent without any state unlabelled', () => {
    expect(agentRowViews(AGENTS, {}, 0, false).map((row) => row.label)).toEqual([undefined, undefined, undefined]);
  });
});

describe('the hand-off baton (issue 3067)', () => {
  it('reads the newest hand-off from a finished turn', () => {
    expect(handoffOf({ 1: { state: 'success', since: 5, activity: { handoff_to: [3, 2] } }, 2: { state: 'working', since: 9 } })).toEqual({ from: 1, to: 3, since: 5 });
    expect(handoffOf(STATES)).toBeUndefined();
  });

  it('is not drawn under reduced motion', () => {
    mockReducedMotion(true);
    const container = createRef<HTMLDivElement>();
    renderWithTheme(
      <div ref={container}>
        <HandoffBaton container={container} states={{ 1: { state: 'success', since: 5, activity: { handoff_to: [2] } } }} />
      </div>,
    );
    expect(screen.queryByTestId('handoff-baton')).toBeNull();
  });
});

describe('the live region (issue 3067)', () => {
  const row = (participantId: number, name: string, label: string): ParticipantRowView => ({ participantId, name, cloud: false, isLead: false, label, motion: 'still' });

  it('announces a change once for a burst of identical frames, and at most once per agent per second', () => {
    let now = 10_000;
    vi.spyOn(Date, 'now').mockImplementation(() => now);
    const { rerender } = render(<AgentStateAnnouncer rows={[row(2, 'CI Doctor', 'Running cargo')]} />);
    const region = screen.getByTestId('agent-state-announcer');
    expect(region).toHaveAttribute('aria-live', 'polite');
    expect(region).toHaveTextContent('CI Doctor: Running cargo');

    // The same state again, and again: nothing new to say.
    rerender(<AgentStateAnnouncer rows={[row(2, 'CI Doctor', 'Running cargo')]} />);
    rerender(<AgentStateAnnouncer rows={[row(2, 'CI Doctor', 'Running cargo')]} />);
    expect(region).toHaveTextContent('CI Doctor: Running cargo');

    // A change inside the second is not read out.
    now += ANNOUNCE_INTERVAL_MS / 2;
    rerender(<AgentStateAnnouncer rows={[row(2, 'CI Doctor', 'Thinking')]} />);
    expect(region).toHaveTextContent('CI Doctor: Running cargo');

    // After it, the next change is.
    now += ANNOUNCE_INTERVAL_MS;
    act(() => {
      rerender(<AgentStateAnnouncer rows={[row(2, 'CI Doctor', 'Writing')]} />);
    });
    expect(region).toHaveTextContent('CI Doctor: Writing');
  });

  it('says a change it held back once the second is over, so the final state is never lost', () => {
    vi.useFakeTimers({ now: 20_000 });
    try {
      const { rerender } = render(<AgentStateAnnouncer rows={[row(2, 'CI Doctor', 'Thinking')]} />);
      const region = screen.getByTestId('agent-state-announcer');
      act(() => { vi.advanceTimersByTime(300); });
      rerender(<AgentStateAnnouncer rows={[row(2, 'CI Doctor', 'Running cargo')]} />);
      expect(region).toHaveTextContent('CI Doctor: Thinking');
      act(() => { vi.advanceTimersByTime(ANNOUNCE_INTERVAL_MS); });
      expect(region).toHaveTextContent('CI Doctor: Running cargo');
    } finally {
      vi.useRealTimers();
    }
  });

  it('reads the changes of several agents at once as one line', () => {
    const rows = [row(2, 'CI Doctor', 'Running cargo'), row(3, 'Mira', 'Thinking')];
    expect(announcements(rows, new Map(), 5_000).lines).toEqual(['CI Doctor: Running cargo', 'Mira: Thinking']);
  });
});
