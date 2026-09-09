import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { WikiChatSessions } from './WikiChatSessions';
import type { WikiConversationSummary } from '../api/wikiHistoryApi';

function conversation(overrides: Partial<WikiConversationSummary> = {}): WikiConversationSummary {
  return {
    id: '1',
    name: 'How does auth work?',
    updatedAt: '2026-01-01T00:00:00Z',
    chatKey: 'key-1',
    ...overrides,
  };
}

describe('WikiChatSessions', () => {
  it('renders nothing when the toolkit has no stored conversation', () => {
    renderWithTheme(
      <WikiChatSessions
        conversations={[]}
        currentConversationId={undefined}
        onResume={vi.fn()}
        onDelete={vi.fn()}
        disabled={false}
      />,
    );
    expect(screen.queryByTestId('wiki-chat-sessions-button')).toBeNull();
  });

  it('lists every stored conversation in the menu', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <WikiChatSessions
        conversations={[conversation({ id: '1', name: 'Q1' }), conversation({ id: '2', name: 'Q2' })]}
        currentConversationId="1"
        onResume={vi.fn()}
        onDelete={vi.fn()}
        disabled={false}
      />,
    );

    await user.click(screen.getByTestId('wiki-chat-sessions-button'));

    const options = screen.getAllByTestId('wiki-chat-session-option');
    expect(options).toHaveLength(2);
    expect(within(options[0]!).getByText('Q1')).toBeVisible();
    expect(within(options[1]!).getByText('Q2')).toBeVisible();
  });

  it('resumes the clicked session and closes the menu', async () => {
    const user = userEvent.setup();
    const onResume = vi.fn();
    renderWithTheme(
      <WikiChatSessions
        conversations={[conversation({ id: '2', name: 'Older question' })]}
        currentConversationId="1"
        onResume={onResume}
        onDelete={vi.fn()}
        disabled={false}
      />,
    );

    await user.click(screen.getByTestId('wiki-chat-sessions-button'));
    await user.click(screen.getByText('Older question'));

    expect(onResume).toHaveBeenCalledWith(expect.objectContaining({ id: '2' }));
    expect(screen.queryByTestId('wiki-chat-session-option')).toBeNull();
  });

  // A session listed without a key cannot be filed into — there is nothing to
  // send as the conversation header — so Resume must not offer it as a live
  // option a click quietly does nothing on.
  it('disables a session that carries no key', async () => {
    const user = userEvent.setup();
    renderWithTheme(
      <WikiChatSessions
        conversations={[conversation({ id: '3', name: 'No key', chatKey: undefined })]}
        currentConversationId={undefined}
        onResume={vi.fn()}
        onDelete={vi.fn()}
        disabled={false}
      />,
    );

    await user.click(screen.getByTestId('wiki-chat-sessions-button'));
    expect(screen.getByTestId('wiki-chat-session-option')).toHaveAttribute('aria-disabled', 'true');
  });

  it('confirms before deleting, and does not resume the row being deleted', async () => {
    const user = userEvent.setup();
    const onResume = vi.fn();
    const onDelete = vi.fn();
    renderWithTheme(
      <WikiChatSessions
        conversations={[conversation({ id: '4', name: 'To delete' })]}
        currentConversationId={undefined}
        onResume={onResume}
        onDelete={onDelete}
        disabled={false}
      />,
    );

    await user.click(screen.getByTestId('wiki-chat-sessions-button'));
    await user.click(screen.getByTestId('wiki-chat-session-delete'));

    // Clicking Delete must not have resumed the row it sits inside.
    expect(onResume).not.toHaveBeenCalled();
    expect(onDelete).not.toHaveBeenCalled();

    const modal = screen.getByTestId('wiki-chat-session-delete-modal');
    expect(within(modal).getByText('To delete')).toBeVisible();

    await user.click(within(modal).getByRole('button', { name: 'Delete' }));

    expect(onDelete).toHaveBeenCalledWith(expect.objectContaining({ id: '4' }));
  });

  it('closes the confirmation without deleting on cancel', async () => {
    const user = userEvent.setup();
    const onDelete = vi.fn();
    renderWithTheme(
      <WikiChatSessions
        conversations={[conversation({ id: '5' })]}
        currentConversationId={undefined}
        onResume={vi.fn()}
        onDelete={onDelete}
        disabled={false}
      />,
    );

    await user.click(screen.getByTestId('wiki-chat-sessions-button'));
    await user.click(screen.getByTestId('wiki-chat-session-delete'));
    const modal = screen.getByTestId('wiki-chat-session-delete-modal');
    await user.click(within(modal).getByRole('button', { name: 'Cancel' }));

    expect(onDelete).not.toHaveBeenCalled();
    // MUI's Dialog exit transition can leave the node in the DOM for a
    // tick after the click that closes it.
    await waitFor(() => {
      expect(screen.queryByTestId('wiki-chat-session-delete-modal')).toBeNull();
    });
  });

  it('disables the trigger while a turn is running', () => {
    renderWithTheme(
      <WikiChatSessions
        conversations={[conversation()]}
        currentConversationId="1"
        onResume={vi.fn()}
        onDelete={vi.fn()}
        disabled
      />,
    );
    expect(screen.getByTestId('wiki-chat-sessions-button')).toBeDisabled();
  });
});
