import { describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import { ApplicationAnswer } from './ApplicationAnswer';

describe('ApplicationAnswer', () => {
  it('renders durable partial content with its terminal error', () => {
    const answer = {
      id: 'answer-1',
      role: 'assistant',
      content: 'The response reached this durable point.',
      exception: 'The runtime operation failed.',
    } as ChatMessage;

    renderWithTheme(<ApplicationAnswer answer={answer} messageId={answer.id} />);

    expect(screen.getByText('The response reached this durable point.')).toBeInTheDocument();
    expect(screen.getByText('The runtime operation failed.')).toBeInTheDocument();
  });

  it('renders a refusal that arrived before any content', () => {
    // The shape `recordStreamFailure` appends when a run is refused before it
    // streams anything (`lib/chatStreamSettle.ts`): no content, no tool
    // actions, only the reason. Nothing else on the row would be visible, so
    // if the exception did not render the turn would look like a lost message
    // — which is exactly what a live stack showed.
    const answer = {
      id: 'answer-2',
      role: 'assistant',
      content: '',
      exception: 'Configuration type is not supported.',
      isStreaming: false,
      isLoading: false,
    } as ChatMessage;

    renderWithTheme(<ApplicationAnswer answer={answer} messageId={answer.id} />);

    expect(screen.getByTestId('error-trace')).toBeInTheDocument();
    expect(screen.getByText('Configuration type is not supported.')).toBeInTheDocument();
  });
});

/**
 * The two elements this row lacked entirely, both visible in every reference
 * screenshot and measured on the live production transcript.
 */
describe('ApplicationAnswer caption line and reasoning summary', () => {
  const withActions = {
    id: 'answer-3',
    role: 'assistant',
    content: 'Hello! How can I help you today?',
    createdAt: '2026-08-29T10:00:00Z',
    toolActions: [
      {
        id: 'step-1',
        type: 'thinking_step',
        status: 'complete',
        name: 'Thinking step',
        content: 'The user is greeting me.',
        created_at: '2026-08-29T10:00:00Z',
        timestamp: '2026-08-29T10:00:01Z',
        ended_at: '2026-08-29T10:00:01Z',
      },
    ],
  } as unknown as ChatMessage;

  it('captions the answer with the participant, the reply-to link and the time', () => {
    renderWithTheme(
      <ApplicationAnswer
        answer={withActions}
        messageId={withActions.id}
        toolActions={withActions.toolActions}
        author={{ participantName: 'Elitea' }}
      />,
    );

    const header = screen.getByTestId('chat-message-header');
    expect(header.textContent).toContain('Elitea');
    expect(screen.getByTestId('chat-message-recipient').textContent).toBe('Message');
    expect(screen.getByTestId('chat-message-time')).toBeInTheDocument();
    expect(screen.getByTestId('chat-message-avatar')).toBeInTheDocument();
  });

  it('collapses the reasoning behind a "Thought for ..." summary instead of dumping it inline', () => {
    // Reasoning steps used to render as flat, always-open `ActionView` rows
    // under a bright "Thinking step" heading, so the whole chain of thought sat
    // above every answer. The production row shows ONE collapsed line.
    renderWithTheme(
      <ApplicationAnswer
        answer={withActions}
        messageId={withActions.id}
        toolActions={withActions.toolActions}
        author={{ participantName: 'Elitea' }}
      />,
    );

    const summary = screen.getByRole('button', { name: /Thought for/ });
    expect(summary).toBeInTheDocument();
    expect(summary.getAttribute('aria-expanded')).toBe('false');
    // The step text is still in the tree (MUI keeps a collapsed panel
    // mounted) but its panel is closed, so none of it is on screen — the
    // defect was that it had no panel at all and rendered flat above the
    // answer.
    const panel = screen.getByText('The user is greeting me.').closest('.MuiCollapse-root');
    expect(panel).not.toBeNull();
    expect(panel?.className).toContain('MuiCollapse-hidden');
  });

  it('still opens the reasoning panel of a turn the reader WATCHED stream', async () => {
    // The regression the toolkit journey caught, and the reason a click test
    // on a freshly-mounted row could not: this panel mounts with its first
    // action, which arrives while the turn is still streaming. MUI decides
    // controlled-vs-uncontrolled once, on that first render, so a panel that
    // took `expanded` only while `isStreaming` mounted CONTROLLED and stayed
    // controlled — and it was passed no `onChange`. When the turn settled the
    // panel closed and the summary became inert: every tool call and every
    // reasoning step of the turn was unreachable until a page reload.
    //
    // Reloading is what the earlier test above renders, which is why it kept
    // passing. The transition is the subject here, so the render starts
    // streaming and the flag is dropped exactly as the settle path drops it.
    const user = userEvent.setup();
    const { rerender } = renderWithTheme(
      <ApplicationAnswer
        answer={withActions}
        messageId={withActions.id}
        toolActions={withActions.toolActions}
        author={{ participantName: 'Elitea' }}
        status={{ isStreaming: true }}
      />,
    );

    // While the turn runs the panel is forced open, as in the baseline.
    expect(screen.getByRole('button', { name: /Thought for/ }).getAttribute('aria-expanded')).toBe('true');

    rerender(
      <ApplicationAnswer
        answer={withActions}
        messageId={withActions.id}
        toolActions={withActions.toolActions}
        author={{ participantName: 'Elitea' }}
        status={{ isStreaming: false }}
      />,
    );

    const summary = screen.getByRole('button', { name: /Thought for/ });
    expect(summary.getAttribute('aria-expanded')).toBe('false');

    await user.click(summary);
    expect(summary.getAttribute('aria-expanded')).toBe('true');
    expect(screen.getByText('The user is greeting me.')).toBeVisible();
  });
});
