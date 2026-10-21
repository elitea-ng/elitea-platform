import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { ApprovalRequestPayload } from '@/shared/desktop/workspaceIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ApprovalDialog } from './ApprovalDialog';

const REQUEST: ApprovalRequestPayload = {
  request_id: 'r1',
  tool: 'shell',
  title: 'Run a command',
  detail: 'The agent wants to run the tests.',
  command: ['cargo', 'test', '--', 'it works'],
  paths: ['src/lib.rs'],
  reason: 'Not on the allow list',
  can_remember: true,
};

describe('ApprovalDialog', () => {
  it('renders nothing without a request', () => {
    renderWithTheme(<ApprovalDialog request={undefined} queued={0} onRespond={vi.fn()} />);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('shows the title, detail, argv (quoted where needed), paths and reason', () => {
    renderWithTheme(<ApprovalDialog request={REQUEST} queued={0} onRespond={vi.fn()} />);
    expect(screen.getByRole('dialog', { name: 'Run a command' })).toBeInTheDocument();
    expect(screen.getByText('The agent wants to run the tests.')).toBeInTheDocument();
    expect(screen.getByTestId('approval-command')).toHaveTextContent("cargo test -- 'it works'");
    expect(screen.getByTestId('approval-paths')).toHaveTextContent('src/lib.rs');
    expect(screen.getByText('Why: Not on the allow list')).toBeInTheDocument();
  });

  it("shows a rule's justification and which rule asks", () => {
    renderWithTheme(<ApprovalDialog request={{ ...REQUEST, source: 'workspace', rule: 'workspace#3', justification: 'Resets go through review.' }} queued={0} onRespond={vi.fn()} />);
    expect(screen.getByText('Why: Resets go through review.')).toBeInTheDocument();
    expect(screen.getByTestId('approval-rule-source')).toHaveTextContent('Asked by workspace rule 3');
  });

  it('names the defaults when no rule asks', () => {
    renderWithTheme(<ApprovalDialog request={{ ...REQUEST, source: 'default' }} queued={0} onRespond={vi.fn()} />);
    expect(screen.getByText('Why: Not on the allow list')).toBeInTheDocument();
    expect(screen.getByTestId('approval-rule-source')).toHaveTextContent('Asked by the defaults');
  });

  it('answers with each button', async () => {
    const onRespond = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<ApprovalDialog request={REQUEST} queued={0} onRespond={onRespond} />);

    await user.click(screen.getByRole('button', { name: 'Allow once' }));
    await user.click(screen.getByRole('button', { name: 'Always allow' }));
    await user.click(screen.getByRole('button', { name: 'Deny' }));
    expect(onRespond.mock.calls).toEqual([
      ['r1', 'allow_once'],
      ['r1', 'allow_always'],
      ['r1', 'deny'],
    ]);
  });

  it('declines and tells the agent why: the text goes only with the denial', async () => {
    const onRespond = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<ApprovalDialog request={REQUEST} queued={0} onRespond={onRespond} />);

    expect(screen.queryByTestId('approval-feedback')).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Deny' })).toHaveFocus());
    await user.click(screen.getByRole('button', { name: 'Decline and tell the agent why' }));
    expect(screen.getByTestId('approval-feedback')).toHaveFocus();
    await user.type(screen.getByTestId('approval-feedback'), '  Use npm test instead  ');
    await user.click(screen.getByRole('button', { name: 'Decline and send' }));
    expect(onRespond.mock.calls).toEqual([['r1', 'deny_with_feedback', 'Use npm test instead']]);
  });

  it('a decline with an empty reason is a plain deny, and a new request closes the box', async () => {
    const onRespond = vi.fn();
    const user = userEvent.setup();
    const { rerender } = renderWithTheme(<ApprovalDialog request={REQUEST} queued={0} onRespond={onRespond} />);

    await user.click(screen.getByRole('button', { name: 'Decline and tell the agent why' }));
    await user.click(screen.getByRole('button', { name: 'Decline and send' }));
    expect(onRespond.mock.calls).toEqual([['r1', 'deny']]);

    expect(screen.getByTestId('approval-feedback')).toBeInTheDocument();
    rerender(<ApprovalDialog request={{ ...REQUEST, request_id: 'r2' }} queued={0} onRespond={onRespond} />);
    expect(screen.queryByTestId('approval-feedback')).not.toBeInTheDocument();
  });

  it('offers "Always allow" only when the request can be remembered', () => {
    renderWithTheme(<ApprovalDialog request={{ ...REQUEST, can_remember: false }} queued={0} onRespond={vi.fn()} />);
    expect(screen.queryByRole('button', { name: 'Always allow' })).toBeNull();
  });

  it('focuses Deny, and Escape denies', async () => {
    const onRespond = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<ApprovalDialog request={REQUEST} queued={0} onRespond={onRespond} />);

    await waitFor(() => expect(screen.getByRole('button', { name: 'Deny' })).toHaveFocus());
    await user.keyboard('{Escape}');
    expect(onRespond).toHaveBeenCalledWith('r1', 'deny');
  });

  it('does not answer on a backdrop click', async () => {
    const onRespond = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<ApprovalDialog request={REQUEST} queued={0} onRespond={onRespond} />);

    const backdrop = document.querySelector('[role="presentation"] > [aria-hidden="true"]');
    expect(backdrop).not.toBeNull();
    await user.click(backdrop as Element);
    expect(onRespond).not.toHaveBeenCalled();
  });

  it('says how many more are waiting', () => {
    renderWithTheme(<ApprovalDialog request={REQUEST} queued={2} onRespond={vi.fn()} />);
    expect(screen.getByText('2 more waiting')).toBeInTheDocument();
  });

  it('renders the continue kind as a compact card, not a modal, that never remembers', async () => {
    const onRespond = vi.fn();
    const user = userEvent.setup();
    const request: ApprovalRequestPayload = {
      request_id: 'c1',
      kind: 'continue',
      steps: 100,
      tool: '',
      title: 'The agent used its 100 steps',
      detail: 'Renamed the symbol in 40 files; fixing the tests next.',
      reason: '',
      can_remember: false,
    };
    renderWithTheme(<ApprovalDialog request={request} queued={0} onRespond={onRespond} />);

    expect(screen.queryByRole('dialog')).toBeNull();
    const card = screen.getByRole('region', { name: 'The agent used its 100 steps' });
    expect(card).toHaveAttribute('data-testid', 'approval-continue-card');
    expect(screen.getByText('Renamed the symbol in 40 files; fixing the tests next.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Always allow' })).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Continue for 100 more steps' }));
    await user.click(screen.getByRole('button', { name: 'Stop here' }));
    expect(onRespond.mock.calls).toEqual([
      ['c1', 'allow_once'],
      ['c1', 'deny'],
    ]);
  });
});
