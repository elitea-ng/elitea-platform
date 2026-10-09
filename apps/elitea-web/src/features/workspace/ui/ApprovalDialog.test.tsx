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
});
