import { ThemeProvider } from '@mui/material/styles';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { QueuedMessages, type QueuedMessagesProps } from './QueuedMessages';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function mount(props: Partial<QueuedMessagesProps> = {}) {
  const handlers = { onSendNow: vi.fn(), onEdit: vi.fn(), onRemove: vi.fn() };
  const items = props.items ?? [
    { id: 'a', text: 'also run the tests' },
    { id: 'b', text: 'then update the changelog with the date', testId: 'steer-pending', state: 'pending' },
  ];
  render(
    <ThemeProvider theme={theme}>
      <QueuedMessages items={items} {...handlers} {...props} />
    </ThemeProvider>,
  );
  return handlers;
}

describe('QueuedMessages', () => {
  it('draws nothing while the queue is empty', () => {
    mount({ items: [], testId: 'empty-queue' });
    expect(screen.queryByTestId('empty-queue')).toBeNull();
  });

  it('shows the caption once, then each message on one line with its full text as the title', () => {
    mount();
    const list = screen.getByTestId('queued-messages');
    expect(within(list).getAllByText('Queued')).toHaveLength(1);
    expect(screen.getByTestId('queued-message')).toHaveTextContent('also run the tests');
    const steer = screen.getByTestId('steer-pending');
    expect(steer).toHaveAttribute('data-state', 'pending');
    expect(within(steer).getByTitle('then update the changelog with the date')).toBeInTheDocument();
  });

  it('hands Send now, Edit and Remove the row they belong to', async () => {
    const user = userEvent.setup();
    const { onSendNow, onEdit, onRemove } = mount();
    const second = screen.getByTestId('steer-pending');
    await user.click(within(second).getByRole('button', { name: 'Send now' }));
    expect(onSendNow).toHaveBeenCalledWith('b');
    await user.click(within(second).getByRole('button', { name: 'Edit' }));
    expect(onEdit).toHaveBeenCalledWith('b');
    await user.click(within(screen.getByTestId('queued-message')).getByRole('button', { name: 'Remove' }));
    expect(onRemove).toHaveBeenCalledWith('a');
    expect(onSendNow).toHaveBeenCalledOnce();
    expect(onEdit).toHaveBeenCalledOnce();
  });
});
