import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createFakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';

import { EraseCloudCopyDialog } from './EraseCloudCopyDialog';
import { renderWithClient } from './testing';

describe('EraseCloudCopyDialog', () => {
  it('asks first and erases only on confirmation; the local folder is not touched', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc({ status: { state: 'CLOUD_IDLE', executor: 'cloud' } });
    const onErased = vi.fn();
    renderWithClient(<EraseCloudCopyDialog open ipc={ipc} conversationUuid="u1" projectId={42} onClose={vi.fn()} onErased={onErased} />);

    expect(screen.getByRole('dialog')).toHaveTextContent('Your folder on this computer is kept as it is');
    expect(ipc.calls.erases).toEqual([]);
    await user.click(screen.getByRole('button', { name: 'Erase cloud copy' }));
    await waitFor(() => expect(onErased).toHaveBeenCalledTimes(1));
    expect(ipc.calls.erases).toEqual(['u1']);
  });

  it('keeps the cloud copy when the person backs out', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    const onClose = vi.fn();
    renderWithClient(<EraseCloudCopyDialog open ipc={ipc} conversationUuid="u1" onClose={onClose} onErased={vi.fn()} />);
    await user.click(screen.getByRole('button', { name: 'Keep it' }));
    expect(onClose).toHaveBeenCalled();
    expect(ipc.calls.erases).toEqual([]);
  });

  it('shows why an erase failed and stays open', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext('erase', 'not_signed_in', 'raw');
    const onErased = vi.fn();
    renderWithClient(<EraseCloudCopyDialog open ipc={ipc} conversationUuid="u1" onClose={vi.fn()} onErased={onErased} />);
    await user.click(screen.getByRole('button', { name: 'Erase cloud copy' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Sign in again');
    expect(onErased).not.toHaveBeenCalled();
  });
});
