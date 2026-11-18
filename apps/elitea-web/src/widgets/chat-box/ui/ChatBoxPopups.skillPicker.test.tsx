/**
 * The skill picker's trust badge and "Pin to this conversation" action (skills epic 2169, ADR-0050 amendment 1, A1.5;
 * ADR-0051 amendment 1, A1.6).
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ChatBoxPopups, type ChatBoxPopupsProps } from './ChatBoxPopups';

type Item = ChatBoxPopupsProps['skill']['filteredItems'][number];

function item(name: string, extra: Partial<Item> = {}): Item {
  return { name, skillId: name, isToolkit: false, isProject: true, ...extra };
}

function props(items: readonly Item[], onPin?: ChatBoxPopupsProps['skill']['onPin']): ChatBoxPopupsProps {
  return {
    recommendations: { show: false, onSelectParticipant: vi.fn(), onClose: vi.fn(), existingParticipants: [], projectId: '5' },
    userMentions: { isProcessingAtSymbol: false, hasOtherUsers: false, users: [], atQuery: '', onSelectUser: vi.fn(), onClose: vi.fn() },
    slash: { phase: 'idle' } as unknown as ChatBoxPopupsProps['slash'],
    skill: { isActive: true, filteredItems: items, highlightedIndex: 0, onSelectTool: vi.fn(), onPin },
  };
}

describe('the skill picker', () => {
  it('marks an untrusted skill and a pinned one, and offers Pin only on a project skill that is not pinned', () => {
    renderWithTheme(
      <ChatBoxPopups
        {...props(
          [
            item('foreign', { untrusted: true }),
            item('mine', { pinned: true }),
            item('plain'),
            item('from-agent', { isProject: false }),
          ],
          vi.fn(() => Promise.resolve()),
        )}
      />,
    );
    expect(screen.getByText('Untrusted')).toBeInTheDocument();
    expect(screen.getByText('Untrusted')).toHaveAttribute('title', expect.stringContaining('quoted data'));
    expect(screen.getByText('Pinned')).toBeInTheDocument();
    // foreign and plain can be pinned; the pinned one and the agent's own skill cannot.
    expect(screen.getAllByRole('button', { name: 'Pin to this conversation' })).toHaveLength(2);
  });

  it('shows no Pin action while the chat has no saved conversation', () => {
    renderWithTheme(<ChatBoxPopups {...props([item('plain')])} />);
    expect(screen.queryByRole('button', { name: 'Pin to this conversation' })).not.toBeInTheDocument();
  });

  it('pins the row it was clicked on, without picking it', async () => {
    const onPin = vi.fn(() => Promise.resolve());
    const built = props([item('plain')], onPin);
    renderWithTheme(<ChatBoxPopups {...built} />);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Pin to this conversation' }));
    expect(onPin).toHaveBeenCalledWith(expect.objectContaining({ name: 'plain' }));
    expect(built.skill.onSelectTool).not.toHaveBeenCalled();
  });

  it('says so when the pin is refused', async () => {
    const onPin = vi.fn(() => Promise.reject(new Error('409')));
    renderWithTheme(<ChatBoxPopups {...props([item('plain')], onPin)} />);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Pin to this conversation' }));
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('could not be pinned'));
  });
});
