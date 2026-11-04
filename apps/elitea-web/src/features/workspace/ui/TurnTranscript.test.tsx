import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { TurnTranscript } from './TurnTranscript';

function renderText(text: string): HTMLElement {
  renderWithTheme(<TurnTranscript view={{ phase: null, items: [{ type: 'text', key: 't1', text }], approvals: [] }} busy={false} />);
  return screen.getByTestId('turn-text');
}

describe('TurnTranscript', () => {
  it('renders the answer as Markdown, as the chat does', () => {
    const text = renderText('## Elitea\n\n**Bold** and `code`\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\n- one\n- two');
    expect(screen.getByRole('heading', { name: 'Elitea' })).toBeInTheDocument();
    expect(screen.getByRole('table')).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(2);
    expect(text.querySelector('strong')?.textContent).toBe('Bold');
    expect(text.textContent).not.toContain('**');
  });

  it("links a step's forge page in a new tab, which the desktop hands to the system browser", () => {
    renderWithTheme(
      <TurnTranscript
        view={{
          phase: null,
          items: [
            {
              type: 'tool',
              key: 'c1',
              callId: 'c1',
              tool: 'open_pull_request',
              argsSummary: '',
              remote: true,
              result: { ok: true, summary: 'Pull request #7 opened', truncated: false },
              link: { href: 'https://github.com/o/r/pull/7', label: 'Open pull request #7' },
            },
          ],
          approvals: [],
        }}
        busy={false}
      />,
    );
    const link = screen.getByTestId('tool-row-link');
    expect(link).toHaveAttribute('href', 'https://github.com/o/r/pull/7');
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    expect(link).toHaveTextContent('Open pull request #7');
  });

  it('drops raw HTML the model writes instead of rendering it', () => {
    const text = renderText('before <img src="https://example.invalid/x.png" onerror="alert(1)"> after');
    expect(text.querySelector('img')).toBeNull();
  });

  it('folds a step\'s reasoning into a Thinking line with its length, and shows it as plain text on demand', async () => {
    const items = [
      { type: 'thinking' as const, key: 'r1', text: 'Check **the** tests', chars: 1234 },
      { type: 'thinking' as const, key: 'r2', text: 'still going' },
    ];
    renderWithTheme(<TurnTranscript view={{ phase: 'running', items, approvals: [] }} busy />);
    const [done, streaming] = screen.getAllByTestId('thinking-block');
    expect(done).toHaveTextContent(`Thinking · ${new Intl.NumberFormat().format(1234)} characters`);
    expect(streaming).toHaveTextContent('Thinking…');
    expect(screen.queryByTestId('thinking-text')).toBeNull();
    const toggle = screen.getByRole('button', { name: /characters/ });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const body = await screen.findByTestId('thinking-text');
    expect(body.textContent).toBe('Check **the** tests');
    expect(body.querySelector('strong')).toBeNull();
  });

  it('says a turn was not saved and offers Retry, which sends that execution again', async () => {
    const onRetry = vi.fn(() => Promise.resolve());
    const view = {
      phase: 'done' as const,
      items: [],
      approvals: [],
      commitFailed: { executionId: 'e1', code: 'local_turn_unavailable', message: 'The turn could not be saved: later', seq: 9 },
    };
    renderWithTheme(<TurnTranscript view={view} busy={false} onRetryCommit={onRetry} />);
    expect(screen.getByTestId('commit-failed')).toHaveTextContent('The turn could not be saved: later');
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(onRetry).toHaveBeenCalledWith('e1'));
  });

  it('offers Retry on a failure another try may mend, never on a refusal, and shows a model retry while it waits', async () => {
    const onRetryTurn = vi.fn();
    const failed = (code: string) => ({ phase: 'error' as const, items: [], approvals: [], error: { code, message: `failed: ${code}` } });
    const { rerender } = renderWithTheme(<TurnTranscript view={failed('model_gateway.incomplete_stream')} busy={false} onRetryTurn={onRetryTurn} />);
    await userEvent.setup().click(screen.getByTestId('turn-error-retry'));
    expect(onRetryTurn).toHaveBeenCalledTimes(1);
    rerender(<TurnTranscript view={failed('model_gateway.member_budget_exhausted')} busy={false} onRetryTurn={onRetryTurn} />);
    expect(screen.getByText('failed: model_gateway.member_budget_exhausted')).toBeInTheDocument();
    expect(screen.queryByTestId('turn-error-retry')).toBeNull();
    rerender(<TurnTranscript view={{ phase: 'running', items: [], approvals: [], retrying: { attempt: 3, max_attempts: 25, message: 'x' } }} busy />);
    expect(screen.getByTestId('turn-retrying')).toHaveTextContent('retrying (3 of 25)');
  });

  it('offers Continue on a turn stopped at the tool calls one turn can save', async () => {
    const onContinue = vi.fn();
    renderWithTheme(<TurnTranscript view={{ phase: 'done', note: 'tool_call_limit_reached', items: [], approvals: [] }} busy={false} onContinue={onContinue} />);
    await userEvent.setup().click(screen.getByTestId('turn-continue'));
    expect(onContinue).toHaveBeenCalledTimes(1);
  });

  it('offers no Continue on a turn the host continued by itself, and marks the continued turn', () => {
    const view = { phase: 'done' as const, note: 'tool_call_limit_reached', items: [], approvals: [], continuedTo: { nextTurnId: 't2', index: 1 } };
    renderWithTheme(<TurnTranscript view={view} busy={false} onContinue={vi.fn()} continuation={2} />);
    expect(screen.queryByTestId('turn-continue')).toBeNull();
    expect(screen.queryByTestId('turn-tool-call-limit')).toBeNull();
    expect(screen.getByTestId('turn-continued-marker')).toHaveTextContent('Continued automatically (2)');
  });

  it('marks a turn the host continued after an app restart', () => {
    const view = { phase: 'running' as const, items: [], approvals: [], continuation: 3, resumed: true as const };
    renderWithTheme(<TurnTranscript view={view} busy onContinue={vi.fn()} continuation={3} />);
    expect(screen.getByTestId('turn-continued-marker')).toHaveTextContent('Resumed after restart (3)');
  });

  it('says how an unattended run ended, with Continue only when it waits', async () => {
    const onContinue = vi.fn();
    const ended = (chainEnded: NonNullable<Parameters<typeof TurnTranscript>[0]['view']['chainEnded']>) => ({ phase: 'done' as const, items: [], approvals: [], chainEnded });
    const { rerender } = renderWithTheme(<TurnTranscript view={ended({ outcome: 'limit_reached', limit: 'deadline' })} busy={false} onContinue={onContinue} />);
    expect(screen.getByTestId('chain-ended')).toHaveTextContent('Unattended run ended: chain limit reached The run reached its deadline.');
    expect(screen.queryByTestId('turn-continue')).toBeNull();
    rerender(<TurnTranscript view={ended({ outcome: 'waiting' })} busy={false} onContinue={onContinue} />);
    await userEvent.setup().click(screen.getByTestId('turn-continue'));
    expect(onContinue).toHaveBeenCalledTimes(1);
  });

  it('shows the run brief above the first turn, and the goal check before the chain end', () => {
    const view = {
      phase: 'done' as const,
      items: [],
      approvals: [],
      runBrief: { state: 'ready' as const, brief: { goal: 'Fix the login test', done_when: [{ id: 'c1', text: 'The test passes' }], constraints: [], assumptions: [], ambiguity: 'low' as const } },
      goalCheck: { outcome: 'met' as const, conditions: [], met: 1, total: 1 },
      chainEnded: { outcome: 'completed' as const },
    };
    renderWithTheme(<TurnTranscript view={view} busy={false} />);
    expect(screen.getByTestId('run-brief-card')).toHaveTextContent('Goal: Fix the login test · Done when: 1 check');
    const notice = screen.getByTestId('goal-check-notice');
    expect(notice).toHaveTextContent('Done · 1 of 1 checks · model-reviewed');
    expect(notice.compareDocumentPosition(screen.getByTestId('chain-ended')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('shows a recorded commit failure without Retry when nothing can resend it', () => {
    const view = { phase: 'done' as const, items: [], approvals: [], commitFailed: { executionId: 'e1', code: 'x', message: 'Not saved.', seq: 3 } };
    renderWithTheme(<TurnTranscript view={view} busy={false} />);
    expect(screen.getByTestId('commit-failed')).toHaveTextContent('Not saved.');
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
  });
});
