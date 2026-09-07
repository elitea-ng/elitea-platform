/**
 * The invite dialog, at the three things the bulk port added: the address
 * chips, the role chips, and the per-address result list.
 *
 * The locators are the journey's own (`textbox name=/emails/i`,
 * `combobox name=/roles/i`, `select-option-<role>`). Asserting through them
 * here is deliberate: a rename that keeps this file green while breaking J22
 * is exactly the failure this pairing exists to stop.
 */
import { configure, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { InviteUserDialog } from './InviteUserDialog';
import { readInviteRows } from './inviteResults';

configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 30_000 });

const ROLES = [
  { label: 'Admin', value: 'admin' },
  { label: 'Editor', value: 'editor' },
];

function renderDialog(overrides: Partial<Parameters<typeof InviteUserDialog>[0]> = {}) {
  const onConfirm = vi.fn();
  const onClose = vi.fn();
  render(
    <InviteUserDialog
      open
      onClose={onClose}
      onConfirm={onConfirm}
      rolesOptions={ROLES}
      {...overrides}
    />,
  );
  return { onConfirm, onClose };
}

async function typeEmails(text: string): Promise<void> {
  const field = screen.getByRole('textbox', { name: /emails/i });
  await userEvent.clear(field);
  await userEvent.type(field, text);
}

async function pickRole(role: string): Promise<void> {
  await userEvent.click(screen.getByRole('combobox', { name: /roles/i }));
  await userEvent.click(await screen.findByTestId(`select-option-${role}`));
}

describe('InviteUserDialog — the address field', () => {
  it('shows one chip per address for a comma-separated paste', async () => {
    renderDialog();
    await typeEmails('a@x.io, b@x.io');

    expect(await screen.findByTestId('invite-email-chip-a@x.io')).toBeInTheDocument();
    expect(screen.getByTestId('invite-email-chip-b@x.io')).toBeInTheDocument();
  });

  it('splits a newline-separated paste, which a comma-only split would treat as one address', async () => {
    renderDialog();
    await typeEmails('a@x.io{Enter}b@x.io');

    expect(await screen.findByTestId('invite-email-chip-a@x.io')).toBeInTheDocument();
    expect(screen.getByTestId('invite-email-chip-b@x.io')).toBeInTheDocument();
  });

  it('keeps the reference wording for an invalid address AND chips it', async () => {
    renderDialog();
    await typeEmails('a@x.io, not-an-email');

    expect(await screen.findByText('Invalid email: not-an-email')).toBeInTheDocument();
    expect(screen.getByTestId('invite-email-chip-not-an-email')).toBeInTheDocument();
  });

  it('refuses to submit while any address is invalid', async () => {
    const { onConfirm } = renderDialog();
    await typeEmails('a@x.io, not-an-email');
    await pickRole('admin');

    expect(screen.getByRole('button', { name: /^invite$/i })).toBeDisabled();
    expect(onConfirm).not.toHaveBeenCalled();
  });
});

describe('InviteUserDialog — the role selection', () => {
  it('renders the picked role, which the SingleSelect mounted with value="" never did', async () => {
    renderDialog();
    await pickRole('editor');

    const chip = await screen.findByTestId('invite-role-chip-editor');
    // The LABEL, not the value: an operator picked "Editor" from the list.
    expect(chip).toHaveTextContent('Editor');
  });

  it('submits every picked role together with every address', async () => {
    const { onConfirm } = renderDialog();
    await typeEmails('a@x.io, b@x.io');
    await pickRole('admin');
    await pickRole('editor');

    await userEvent.click(screen.getByRole('button', { name: /^invite$/i }));

    expect(onConfirm).toHaveBeenCalledWith({
      emails: ['a@x.io', 'b@x.io'],
      roles: ['admin', 'editor'],
    });
  });

  it('removes a role from the chip', async () => {
    renderDialog();
    await pickRole('admin');
    await userEvent.click(within(await screen.findByTestId('invite-role-chip-admin')).getByTestId('CancelIcon'));

    await waitFor(() => expect(screen.queryByTestId('invite-role-chip-admin')).not.toBeInTheDocument());
  });
});

describe('InviteUserDialog — the per-address results', () => {
  const RESULTS = readInviteRows([
    { email: 'new@x.io', status: 'ok', outcome: 'invited', invitation_delivered: true },
    { email: 'member@x.io', status: 'error', outcome: 'already_member', msg: 'exists' },
    { email: 'oops', status: 'error', outcome: 'invalid_email', msg: 'Invalid email: oops' },
  ]);

  it('names every address and what happened to it', async () => {
    renderDialog({ results: RESULTS });

    const list = await screen.findByTestId('invite-results');
    expect(within(list).getByTestId('invite-result-new@x.io')).toHaveTextContent('Invited');
    expect(within(list).getByTestId('invite-result-member@x.io')).toHaveTextContent('Already a member');
    expect(within(list).getByTestId('invite-result-oops')).toHaveTextContent('Invalid address');
  });

  it('renders no result list before the first submit', () => {
    renderDialog();
    expect(screen.queryByTestId('invite-results')).not.toBeInTheDocument();
  });

  it('offers Close rather than Cancel once there is a result to read', async () => {
    renderDialog({ results: RESULTS });
    expect(await screen.findByRole('button', { name: /^close$/i })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^cancel$/i })).not.toBeInTheDocument();
  });

  it('does not close itself on submit — the page decides, because only it knows the answer', async () => {
    const { onConfirm, onClose } = renderDialog();
    await typeEmails('a@x.io');
    await pickRole('admin');
    await userEvent.click(screen.getByRole('button', { name: /^invite$/i }));

    expect(onConfirm).toHaveBeenCalledOnce();
    expect(onClose).not.toHaveBeenCalled();
  });
});
