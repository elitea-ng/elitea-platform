import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { TurnTranscript } from './TurnTranscript';

function renderText(text: string): HTMLElement {
  renderWithTheme(<TurnTranscript view={{ phase: null, items: [{ type: 'text', key: 't1', text }], approvals: [] }} busy={false} onCancel={vi.fn()} />);
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

  it('drops raw HTML the model writes instead of rendering it', () => {
    const text = renderText('before <img src="https://example.invalid/x.png" onerror="alert(1)"> after');
    expect(text.querySelector('img')).toBeNull();
  });

  it('folds a step\'s reasoning into a Thinking line with its length, and shows it as plain text on demand', async () => {
    const items = [
      { type: 'thinking' as const, key: 'r1', text: 'Check **the** tests', chars: 1234 },
      { type: 'thinking' as const, key: 'r2', text: 'still going' },
    ];
    renderWithTheme(<TurnTranscript view={{ phase: 'running', items, approvals: [] }} busy onCancel={vi.fn()} />);
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
    renderWithTheme(<TurnTranscript view={view} busy={false} onCancel={vi.fn()} onRetryCommit={onRetry} />);
    expect(screen.getByTestId('commit-failed')).toHaveTextContent('The turn could not be saved: later');
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => expect(onRetry).toHaveBeenCalledWith('e1'));
  });

  it('offers Retry on a failure another try may mend, never on a refusal, and shows a model retry while it waits', async () => {
    const onRetryTurn = vi.fn();
    const failed = (code: string) => ({ phase: 'error' as const, items: [], approvals: [], error: { code, message: `failed: ${code}` } });
    const { rerender } = renderWithTheme(<TurnTranscript view={failed('model_gateway.incomplete_stream')} busy={false} onCancel={vi.fn()} onRetryTurn={onRetryTurn} />);
    await userEvent.setup().click(screen.getByTestId('turn-error-retry'));
    expect(onRetryTurn).toHaveBeenCalledTimes(1);
    rerender(<TurnTranscript view={failed('model_gateway.member_budget_exhausted')} busy={false} onCancel={vi.fn()} onRetryTurn={onRetryTurn} />);
    expect(screen.getByText('failed: model_gateway.member_budget_exhausted')).toBeInTheDocument();
    expect(screen.queryByTestId('turn-error-retry')).toBeNull();
    rerender(<TurnTranscript view={{ phase: 'running', items: [], approvals: [], retrying: { attempt: 3, max_attempts: 25, message: 'x' } }} busy onCancel={vi.fn()} />);
    expect(screen.getByTestId('turn-retrying')).toHaveTextContent('retrying (3 of 25)');
  });

  it('shows a recorded commit failure without Retry when nothing can resend it', () => {
    const view = { phase: 'done' as const, items: [], approvals: [], commitFailed: { executionId: 'e1', code: 'x', message: 'Not saved.', seq: 3 } };
    renderWithTheme(<TurnTranscript view={view} busy={false} onCancel={vi.fn()} />);
    expect(screen.getByTestId('commit-failed')).toHaveTextContent('Not saved.');
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
  });
});
