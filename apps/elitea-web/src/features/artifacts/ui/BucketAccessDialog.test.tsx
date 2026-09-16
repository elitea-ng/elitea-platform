/**
 * The dialog's own rendering rules — the states its composition-root test
 * (`BucketAccessPanel.test.tsx`) cannot reach through the server, because they
 * are decisions about what to draw rather than about what to fetch.
 */
import { screen, waitFor } from '@testing-library/react';
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

/**
 * Issue 940/A10 (ELITEA-2480) — bulk edit: a header "Select all" checkbox,
 * a per-row checkbox, and a pencil icon above the table that opens a modal
 * applying one permission to every selected row via the same `onSetAccess`
 * a single row's own `<Select>` already calls.
 */
describe('BucketAccessDialog — bulk edit (issue 940/A10)', () => {
  const threeRows = [
    { user_id: 1, name: 'User A', email: 'a@example.test', bucket_permissions: { reports: ['read'] } },
    { user_id: 2, name: 'User B', email: 'b@example.test', bucket_permissions: { reports: ['read'] } },
    { user_id: 3, name: 'User C', email: 'c@example.test', bucket_permissions: { reports: [] } },
  ];

  it('keeps the bulk-edit icon disabled until at least one row is selected', async () => {
    renderWithProviders(<BucketAccessDialog {...props({ rows: threeRows })} />);
    expect(await screen.findByTestId('bucket-access-bulk-edit-open')).toBeDisabled();

    await userEvent.click(screen.getByLabelText('Select User A'));
    expect(screen.getByTestId('bucket-access-bulk-edit-open')).not.toBeDisabled();
  });

  it('"Select all" selects every row, and clicking it again deselects every row', async () => {
    renderWithProviders(<BucketAccessDialog {...props({ rows: threeRows })} />);

    await userEvent.click(await screen.findByLabelText('Select all exceptions'));
    expect(screen.getByLabelText('Select User A')).toBeChecked();
    expect(screen.getByLabelText('Select User B')).toBeChecked();
    expect(screen.getByLabelText('Select User C')).toBeChecked();

    await userEvent.click(screen.getByLabelText('Select all exceptions'));
    expect(screen.getByLabelText('Select User A')).not.toBeChecked();
    expect(screen.getByLabelText('Select User B')).not.toBeChecked();
    expect(screen.getByLabelText('Select User C')).not.toBeChecked();
  });

  it('applies the chosen permission to every selected row, and leaves unselected rows untouched', async () => {
    const onSetAccess = vi.fn();
    renderWithProviders(<BucketAccessDialog {...props({ rows: threeRows, onSetAccess })} />);

    await userEvent.click(await screen.findByLabelText('Select User A'));
    await userEvent.click(screen.getByLabelText('Select User B'));
    await userEvent.click(screen.getByTestId('bucket-access-bulk-edit-open'));

    expect(await screen.findByText('2 users selected')).toBeInTheDocument();

    await userEvent.click(screen.getByLabelText('Bulk edit permissions value'));
    await userEvent.click(await screen.findByRole('option', { name: 'No access' }));
    await userEvent.click(screen.getByTestId('bucket-access-bulk-edit-save'));

    expect(onSetAccess).toHaveBeenCalledTimes(2);
    expect(onSetAccess).toHaveBeenCalledWith(1, 'no_access');
    expect(onSetAccess).toHaveBeenCalledWith(2, 'no_access');
    expect(onSetAccess).not.toHaveBeenCalledWith(3, expect.anything());
  });

  // ELITEA-2480's own acceptance text: choosing "Read & Write" for every
  // selected row removes them from the Exceptions table (back to default) —
  // the same removal `onSetAccess`/`permissionsFromAccess` already give a
  // single row's "Read/write (default)" choice, just called once per row.
  it('"Read/write (default)" removes every selected row from the exceptions table', async () => {
    const onSetAccess = vi.fn();
    renderWithProviders(<BucketAccessDialog {...props({ rows: threeRows, onSetAccess })} />);

    await userEvent.click(await screen.findByLabelText('Select all exceptions'));
    await userEvent.click(screen.getByTestId('bucket-access-bulk-edit-open'));
    await userEvent.click(screen.getByLabelText('Bulk edit permissions value'));
    await userEvent.click(await screen.findByRole('option', { name: 'Read/write (default)' }));
    await userEvent.click(screen.getByTestId('bucket-access-bulk-edit-save'));

    expect(onSetAccess).toHaveBeenCalledTimes(3);
    for (const id of [1, 2, 3]) expect(onSetAccess).toHaveBeenCalledWith(id, 'read_write');
  });

  it('clears the selection and closes the modal after saving', async () => {
    renderWithProviders(<BucketAccessDialog {...props({ rows: threeRows })} />);

    await userEvent.click(await screen.findByLabelText('Select User A'));
    await userEvent.click(screen.getByTestId('bucket-access-bulk-edit-open'));
    await userEvent.click(screen.getByTestId('bucket-access-bulk-edit-save'));

    // MUI's `Dialog` unmounts its content only after its exit TRANSITION
    // settles, not synchronously on `open={false}` — `waitFor` lets that
    // finish rather than asserting mid-transition.
    await waitFor(() => expect(screen.queryByTestId('bucket-access-bulk-edit-dialog')).not.toBeInTheDocument());
    expect(screen.getByLabelText('Select User A')).not.toBeChecked();
    expect(screen.getByTestId('bucket-access-bulk-edit-open')).toBeDisabled();
  });
});
