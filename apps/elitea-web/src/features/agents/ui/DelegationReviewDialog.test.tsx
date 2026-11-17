import { fireEvent, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { DelegationItem, DelegationPreview } from '@/shared/api/generated/model';

import { renderWithProviders } from '../__tests__/testUtils';

import { DelegationReviewDialog } from './DelegationReviewDialog';

const GIT: DelegationItem = { kind: 'toolkit', name: 'toolkit:12' };
const GIT_PUSH: DelegationItem = { kind: 'tool', name: 'push', toolkit: 'toolkit:12' };
const GIT_CREDENTIAL: DelegationItem = { kind: 'credential', name: 'ref-1', toolkit: 'toolkit:12' };
const DB: DelegationItem = { kind: 'toolkit', name: 'toolkit:13' };
const DB_ALL: DelegationItem = { kind: 'tool', name: '*', toolkit: 'toolkit:13' };
const HOST: DelegationItem = { kind: 'egress_host', name: 'git.example.com' };

const NAMES = new Map([
  ['toolkit:12', 'Git'],
  ['toolkit:13', 'Database'],
]);

function preview(overrides: Partial<DelegationPreview> = {}): DelegationPreview {
  return {
    callee_application_id: 7,
    callee_version_id: 70,
    callee_name: 'Git specialist',
    digest: 'abc',
    items: [GIT, GIT_PUSH, GIT_CREDENTIAL, DB, DB_ALL, HOST],
    approvable: [GIT, GIT_PUSH, GIT_CREDENTIAL, DB, DB_ALL, HOST],
    withheld: [],
    closure: [],
    governed: false,
    ...overrides,
  };
}

function renderDialog(props: Partial<React.ComponentProps<typeof DelegationReviewDialog>> = {}) {
  const onConfirm = vi.fn();
  const onCancel = vi.fn();
  renderWithProviders(
    <DelegationReviewDialog
      open
      calleeName="Git specialist"
      preview={preview()}
      names={NAMES}
      confirmLabel="Approve and attach"
      onCancel={onCancel}
      onConfirm={onConfirm}
      {...props}
    />,
  );
  return { onConfirm, onCancel };
}

describe('DelegationReviewDialog', () => {
  it('lists what the agent brings: toolkits by name with tools and credential, and the hosts', () => {
    renderDialog();

    const dialog = screen.getByRole('dialog', { name: 'What Git specialist will be able to use' });
    expect(within(dialog).getByRole('checkbox', { name: 'Git' })).toBeChecked();
    expect(within(dialog).getByRole('checkbox', { name: 'Database' })).toBeChecked();
    expect(within(dialog).getByText('Tools: push')).toBeVisible();
    expect(within(dialog).getByText('Tools: All tools')).toBeVisible();
    expect(within(dialog).getByText('Credential: ref-1')).toBeVisible();
    expect(within(dialog).getByText(/git\.example\.com/)).toBeVisible();
  });

  it('approves everything (no item list) when nothing is unticked', () => {
    const { onConfirm } = renderDialog();

    fireEvent.click(screen.getByRole('button', { name: 'Approve and attach' }));

    expect(onConfirm).toHaveBeenCalledWith(undefined);
  });

  it('approves only what stays ticked: an unticked toolkit leaves with its tools and credential', () => {
    const { onConfirm } = renderDialog();

    fireEvent.click(screen.getByRole('checkbox', { name: 'Git' }));
    fireEvent.click(screen.getByRole('button', { name: 'Approve and attach' }));

    expect(onConfirm).toHaveBeenCalledWith([DB, DB_ALL, HOST]);
  });

  it('shows a toolkit the approver cannot access disabled with the reason, and approves only the rest', () => {
    const { onConfirm } = renderDialog({
      preview: preview({ approvable: [DB, DB_ALL, HOST], withheld: [GIT, GIT_PUSH, GIT_CREDENTIAL] }),
    });

    const git = screen.getByRole('checkbox', { name: 'Git' });
    expect(git).toBeDisabled();
    expect(git).not.toBeChecked();
    expect(screen.getByTestId('delegation-withheld-reason')).toHaveTextContent('You cannot use this credential');
    expect(screen.getByRole('checkbox', { name: 'Database' })).toBeEnabled();

    fireEvent.click(screen.getByRole('button', { name: 'Approve and attach' }));
    expect(onConfirm).toHaveBeenCalledWith(undefined);
  });

  it('cannot be approved when the person holds nothing the agent brings', () => {
    renderDialog({
      preview: preview({ items: [GIT, GIT_PUSH], approvable: [], withheld: [GIT, GIT_PUSH] }),
    });

    expect(screen.getByRole('button', { name: 'Approve and attach' })).toBeDisabled();
    expect(screen.getByTestId('delegation-withheld-reason')).toHaveTextContent('access to this toolkit');
  });

  it('is read-only without a primary action: the button reads Close and nothing can be approved', () => {
    const { onCancel } = renderDialog({ confirmLabel: undefined, notice: 'A different approver decides this delegation in its change request.' });

    expect(screen.queryByRole('button', { name: 'Approve and attach' })).not.toBeInTheDocument();
    expect(screen.getByText(/A different approver decides/)).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onCancel).toHaveBeenCalled();
  });

  it('says Request approval for the governed profile and shows the closure as information', () => {
    renderDialog({
      confirmLabel: 'Request approval',
      preview: preview({
        governed: true,
        closure: [
          { application_id: 7, name: 'Git specialist', depth: 0, items: [GIT] },
          { application_id: 8, name: 'Deploy helper', depth: 1, items: [{ kind: 'toolkit', name: 'toolkit:20' }] },
        ],
      }),
    });

    expect(screen.getByRole('button', { name: 'Request approval' })).toBeEnabled();
    expect(screen.getByTestId('delegation-closure-node')).toHaveTextContent('Deploy helper: toolkit:20');
    expect(screen.getByText(/Information only/)).toBeVisible();
  });

  it('shows a failure and blocks the primary action while busy', () => {
    renderDialog({ error: 'The delegation could not be approved.', busy: true });

    expect(screen.getByText('The delegation could not be approved.')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Approve and attach' })).toBeDisabled();
  });
});
