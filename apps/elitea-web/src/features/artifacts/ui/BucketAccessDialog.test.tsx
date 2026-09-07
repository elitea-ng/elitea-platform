/**
 * The dialog's own rendering rules — the states its composition-root test
 * (`BucketAccessPanel.test.tsx`) cannot reach through the server, because they
 * are decisions about what to draw rather than about what to fetch.
 */
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '../__tests__/testUtils';
import { BucketAccessDialog } from './BucketAccessDialog';

function props(overrides: Partial<Parameters<typeof BucketAccessDialog>[0]> = {}):
Parameters<typeof BucketAccessDialog>[0] {
  return {
    open: true,
    bucket: 'reports',
    rows: [],
    candidates: [{ id: 11, name: 'Blocked Member', email: 'blocked@example.test' }],
    isLoading: false,
    isSaving: false,
    onClose: vi.fn(),
    onSetAccess: vi.fn(),
    onRemove: vi.fn(),
    ...overrides,
  };
}

describe('BucketAccessDialog', () => {
  it('says it is loading rather than saying there are no exceptions', async () => {
    renderWithProviders(<BucketAccessDialog {...props({ isLoading: true })} />);
    expect(await screen.findByText('Loading exceptions…')).toBeInTheDocument();
    // The empty state must NOT be on screen while the read is in flight: "no
    // exceptions" and "not read yet" are different answers.
    expect(screen.queryByTestId('bucket-access-empty')).toBeNull();
  });

  it('shows the error it is given', async () => {
    renderWithProviders(<BucketAccessDialog {...props({ errorMessage: 'Nope.' })} />);
    expect(await screen.findByTestId('bucket-access-error')).toHaveTextContent('Nope.');
  });

  it('names a member by id when the user row is gone', async () => {
    renderWithProviders(<BucketAccessDialog {...props({
      rows: [{ user_id: 42, bucket_permissions: { reports: ['read'] } }],
    })} />);
    expect(await screen.findByText('#42')).toBeInTheDocument();
  });

  // The Add button must stay inert until a member is chosen; firing with no
  // subject would send `user_id: undefined` and 400.
  it('cannot add an exception before a member is picked', async () => {
    const onSetAccess = vi.fn();
    renderWithProviders(<BucketAccessDialog {...props({ onSetAccess })} />);
    expect(await screen.findByRole('button', { name: 'Add exception' })).toBeDisabled();
    expect(onSetAccess).not.toHaveBeenCalled();
  });

  it('adds an exception for the picked member with the chosen permission', async () => {
    const onSetAccess = vi.fn();
    renderWithProviders(<BucketAccessDialog {...props({ onSetAccess })} />);

    await userEvent.click(screen.getByLabelText('Users'));
    await userEvent.click(await screen.findByRole('option', { name: 'Blocked Member' }));
    await userEvent.click(screen.getByLabelText('New exception permissions'));
    await userEvent.click(await screen.findByRole('option', { name: 'No access' }));
    await userEvent.click(screen.getByRole('button', { name: 'Add exception' }));

    expect(onSetAccess).toHaveBeenCalledWith(11, 'no_access');
  });

  // A member who already has an exception is not offered again — adding a
  // second one for the same person would silently replace the first.
  it('does not offer a member who is already listed', async () => {
    renderWithProviders(<BucketAccessDialog {...props({
      rows: [{ user_id: 11, name: 'Blocked Member', email: 'blocked@example.test', bucket_permissions: { reports: [] } }],
    })} />);
    await userEvent.click(screen.getByLabelText('Users'));
    expect(screen.queryByRole('option', { name: 'Blocked Member' })).toBeNull();
  });

  it('closes on the Close button', async () => {
    const onClose = vi.fn();
    renderWithProviders(<BucketAccessDialog {...props({ onClose })} />);
    await userEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalled();
  });

  it('disables the row controls while a write is in flight', async () => {
    renderWithProviders(<BucketAccessDialog {...props({
      isSaving: true,
      rows: [{ user_id: 11, name: 'Blocked Member', email: 'blocked@example.test', bucket_permissions: { reports: [] } }],
    })} />);
    expect(await screen.findByLabelText('Remove exception for Blocked Member')).toBeDisabled();
  });
});
