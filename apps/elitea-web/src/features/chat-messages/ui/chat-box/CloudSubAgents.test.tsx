import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { CloudSubAgentLane } from '@elitea/client-core/stream';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory.types';
import { ApplicationAnswer } from './ApplicationAnswer';

const lane = (over: Partial<CloudSubAgentLane>, named = true): CloudSubAgentLane => ({
  agentId: 'a_1',
  parentCallId: 'call-spawn',
  ordinal: 1,
  ...(named ? { displayName: 'Owl' } : {}),
  status: 'running',
  items: [{ type: 'tool', callId: 'k1', tool: 'grep', argsSummary: 'run_agent', ok: true, summary: '2 matches' }],
  ...over,
});

const answer = (subAgents: CloudSubAgentLane[]): ChatMessage => ({ id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee', role: 'assistant', name: 'Agent', content: 'Done.', createdAt: '', subAgents });

describe('cloud sub-agents under a chat answer (client contract 1.48)', () => {
  it('shows a running child open, named, temporary, with its own tool calls', () => {
    renderWithTheme(<ApplicationAnswer answer={answer([lane({})])} messageId="m" />);
    const row = screen.getByTestId('cloud-subagent');
    expect(row).toHaveTextContent('Owl');
    expect(screen.getByTestId('cloud-subagent-temporary')).toHaveTextContent('Temporary');
    expect(screen.getByTestId('cloud-subagent-status')).toHaveTextContent('Running');
    expect(row).toHaveTextContent('✓ grep run_agent → 2 matches');
    expect(row.querySelector('[data-testid="chat-message-avatar"]')).not.toBeNull();
  });

  it('folds a settled child and opens its final answer on demand, as text', async () => {
    renderWithTheme(<ApplicationAnswer answer={answer([lane({ status: 'completed', result: '<b>Two</b> callers.' }, false)])} messageId="m" />);
    expect(screen.getByTestId('cloud-subagent')).toHaveTextContent('Sub-agent 1');
    expect(screen.getByTestId('cloud-subagent-status')).toHaveTextContent('Done');
    expect(screen.queryByTestId('cloud-subagent-result')).toBeNull();
    await userEvent.click(screen.getByRole('button', { name: /Sub-agent 1/ }));
    const result = screen.getByTestId('cloud-subagent-result');
    expect(result).toHaveTextContent('<b>Two</b> callers.');
    expect(result.querySelector('b')).toBeNull();
  });

  // Issue 1924: the chat answer draws the shared lane, so it shows what the workspace lane shows.
  it("shows a settled child's steps and tokens, as the workspace lane does", () => {
    renderWithTheme(<ApplicationAnswer answer={answer([lane({ status: 'completed', steps: 3, inputTokens: 100, outputTokens: 20 })])} messageId="m" />);
    expect(screen.getByTestId('cloud-subagent')).toHaveTextContent('3 steps · 120 tokens');
  });

  it('an answer without sub-agents shows none', () => {
    renderWithTheme(<ApplicationAnswer answer={answer([])} messageId="m" />);
    expect(screen.queryByTestId('cloud-subagents')).toBeNull();
  });
});
