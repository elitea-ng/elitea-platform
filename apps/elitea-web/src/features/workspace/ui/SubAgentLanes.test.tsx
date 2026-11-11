import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { TurnView } from '../model/turnReducer';
import { TurnTranscript } from './TurnTranscript';

const view = (status: 'running' | 'completed'): TurnView => ({
  phase: 'running',
  approvals: [],
  items: [{ type: 'tool', key: 'cc1', callId: 'c1', tool: 'spawn_agent', argsSummary: 'Find the callers', remote: false }],
  subAgents: [
    {
      agentId: 'a_1',
      parentCallId: 'c1',
      ordinal: 1,
      label: 'callers',
      status,
      items: [{ type: 'tool', key: 'ck1', callId: 'k1', tool: 'grep', argsSummary: 'run_agent', remote: false }],
      ...(status === 'completed' ? { steps: 3, inputTokens: 1000, outputTokens: 200, result: 'Two callers: turn.rs and tests.rs' } : {}),
    },
  ],
});

describe('SubAgentLanes', () => {
  it('shows a running child open under its spawn_agent row, with its own tool rows', () => {
    renderWithTheme(<TurnTranscript view={view('running')} busy />);
    const lane = screen.getByTestId('subagent-lane');
    expect(lane).toHaveTextContent('Sub-agent 1 · callers');
    expect(screen.getByTestId('subagent-status')).toHaveTextContent('Running');
    // The turn's row and the child's.
    expect(screen.getAllByTestId('tool-row')).toHaveLength(2);
    expect(screen.getByRole('button', { name: /Sub-agent 1/ })).toHaveAttribute('aria-expanded', 'true');
  });

  it('folds a settled child, with its spend, and opens its report on demand', async () => {
    renderWithTheme(<TurnTranscript view={view('completed')} busy={false} />);
    expect(screen.getByTestId('subagent-status')).toHaveTextContent('Done');
    expect(screen.getByTestId('subagent-lane')).toHaveTextContent('3 steps · 1200 tokens');
    expect(screen.queryByTestId('subagent-result')).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: /Sub-agent 1/ }));
    expect(screen.getByTestId('subagent-result')).toHaveTextContent('Two callers: turn.rs and tests.rs');
  });
});

describe('SubAgentLanes for a cloud temporary child (client contract 1.48)', () => {
  const cloud = (displayName: string): TurnView => {
    const base = view('running');
    return { ...base, subAgents: (base.subAgents ?? []).map((lane) => ({ ...lane, label: 'callers', displayName, temporary: true })) };
  };

  it('names it by its profile, with its avatar and a temporary badge', () => {
    const Avatar = ({ lane }: { lane: { agentId: string } }) => <span data-testid="lane-avatar">{lane.agentId}</span>;
    renderWithTheme(<TurnTranscript view={cloud('Owl')} busy subAgentAvatar={Avatar} />);
    const lane = screen.getByTestId('subagent-lane');
    expect(lane).toHaveTextContent('Owl · callers');
    expect(lane).not.toHaveTextContent('Sub-agent 1');
    expect(screen.getByTestId('subagent-temporary')).toHaveTextContent('Temporary');
    expect(screen.getByTestId('lane-avatar')).toHaveTextContent('a_1');
  });

  it('renders untrusted text as text', () => {
    renderWithTheme(<TurnTranscript view={cloud('<img src=x onerror=alert(1)>')} busy />);
    expect(screen.getByTestId('subagent-lane')).toHaveTextContent('<img src=x onerror=alert(1)>');
    expect(screen.getByTestId('subagent-lane').querySelector('img')).toBeNull();
  });

  it('a local child shows no badge', () => {
    renderWithTheme(<TurnTranscript view={view('running')} busy />);
    expect(screen.queryByTestId('subagent-temporary')).toBeNull();
  });
});
