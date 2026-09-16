import type { ReactElement } from 'react';

import { DndContext } from '@dnd-kit/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { ConversationItem, type ConversationItemProps, type ConversationWithOwnerMeta } from './ConversationItem';

function mockPermissions(enabled: boolean): void {
  server.use(
    http.get('*/auth/permissions/prompt_lib/:projectId', () =>
      HttpResponse.json(
        [
          { name: 'models.chat.folders.create', enabled },
          { name: 'models.chat.folders.update', enabled },
        ],
        { status: 200 },
      ),
    ),
  );
}

function baseConversation(overrides: Partial<ConversationWithOwnerMeta> = {}): ConversationWithOwnerMeta {
  return {
    id: 'conv-1',
    name: 'My conversation',
    isPrivate: true,
    authorId: 'user-1',
    ...overrides,
  };
}

function renderItem(overrides: Partial<ConversationItemProps> = {}): ReturnType<typeof renderWithTheme> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const props: ConversationItemProps = {
    conversation: baseConversation(),
    onSelectConversation: vi.fn(),
    onDelete: vi.fn(),
    onExport: vi.fn(),
    onEdit: vi.fn(),
    onPlayback: vi.fn(),
    onPin: vi.fn(),
    onDuplicate: vi.fn(),
    onCreateConversation: vi.fn().mockResolvedValue(undefined),
    onCancelCreate: vi.fn(),
    onChangeActiveConversationName: vi.fn(),
    projectId: 'p1',
    currentUserId: 'user-1',
    ...overrides,
  };
  return renderWithTheme(
    (
      <QueryClientProvider client={queryClient}>
        <ConversationItem {...props} />
      </QueryClientProvider>
    ) as ReactElement,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  mockPermissions(true);
});

afterEach(() => {
  resetGeneratedClient();
});

describe('ConversationItem', () => {
  it('renders the conversation name', () => {
    renderItem();
    expect(screen.getByText('My conversation')).toBeInTheDocument();
  });

  it('falls back to the first chat-history message when name is empty', () => {
    renderItem({ conversation: baseConversation({ name: '', chatHistory: [{ content: 'Hello there' }] }) });
    expect(screen.getByText('Hello there')).toBeInTheDocument();
  });

  it('calls onSelectConversation when an inactive row is clicked', async () => {
    const user = userEvent.setup();
    const onSelectConversation = vi.fn();
    renderItem({ onSelectConversation, isActive: false });

    await user.click(screen.getByText('My conversation'));

    expect(onSelectConversation).toHaveBeenCalledWith(expect.objectContaining({ id: 'conv-1' }));
  });

  it('does not call onSelectConversation when the row is already active', async () => {
    const user = userEvent.setup();
    const onSelectConversation = vi.fn();
    renderItem({ onSelectConversation, isActive: true });

    await user.click(screen.getByText('My conversation'));

    expect(onSelectConversation).not.toHaveBeenCalled();
  });

  it('shows a naming spinner instead of the menu while isNamingPending', async () => {
    const user = userEvent.setup();
    renderItem({ conversation: baseConversation({ isNamingPending: true }) });
    expect(screen.getByTestId('conversation-naming-spinner')).toBeInTheDocument();
    expect(screen.getByText('Naming')).toBeInTheDocument();

    // The menu trigger is `display:none` until the row is hovered
    // (`ConversationItem.styles.ts`'s `menuWrapper`) — hover first so this
    // assertion is actually exercising the `isNamingPending` suppression,
    // not just the hover-visibility gate.
    await user.hover(screen.getByTestId('conversation-naming-spinner'));
    expect(screen.queryByRole('button', { name: /more actions/i })).not.toBeInTheDocument();
  });

  it('opens the menu and fires onPin(conversation, true) when the conversation is not yet pinned', async () => {
    const user = userEvent.setup();
    const onPin = vi.fn();
    renderItem({ onPin, conversation: baseConversation({ isPinned: false }) });

    // The `ControlsDropdown` trigger only renders visibly once the row is
    // hovered (`ConversationItem.styles.ts`'s `menuWrapper`: `display: none`
    // while `!isHovering`), matching the baseline's own hover-revealed menu.
    await user.hover(screen.getByText('My conversation'));
    await user.click(screen.getByRole('button', { name: /more actions/i }));
    await user.click(await screen.findByRole('menuitem', { name: 'Pin on top' }));

    expect(onPin).toHaveBeenCalledWith(expect.objectContaining({ id: 'conv-1' }), true);
  });

  it('shows "Unpin" and fires onPin(conversation, false) when already pinned', async () => {
    const user = userEvent.setup();
    const onPin = vi.fn();
    renderItem({ onPin, conversation: baseConversation({ isPinned: true }) });

    await user.hover(screen.getByText('My conversation'));
    await user.click(screen.getByRole('button', { name: /more actions/i }));
    await user.click(await screen.findByRole('menuitem', { name: 'Unpin' }));

    expect(onPin).toHaveBeenCalledWith(expect.objectContaining({ id: 'conv-1' }), false);
  });

  it('deletes only after the inline confirm row is also clicked', async () => {
    const user = userEvent.setup();
    const onDelete = vi.fn();
    renderItem({ onDelete });

    await user.hover(screen.getByText('My conversation'));
    await user.click(screen.getByRole('button', { name: /more actions/i }));
    await user.click(await screen.findByRole('menuitem', { name: 'Delete' }));
    expect(onDelete).not.toHaveBeenCalled();

    await user.click(await screen.findByRole('menuitem', { name: 'Delete' }));
    expect(onDelete).toHaveBeenCalledWith(expect.objectContaining({ id: 'conv-1' }));
  });

  it('reduces the menu to Delete/Edit only for a playback row', async () => {
    const user = userEvent.setup();
    renderItem({ conversation: baseConversation({ isPlayback: true }) });

    await user.hover(screen.getByText('My conversation'));
    await user.click(screen.getByRole('button', { name: /more actions/i }));
    const menu = await screen.findByRole('menu');
    const labels = within(menu)
      .getAllByRole('menuitem')
      .map((item) => item.textContent);

    expect(labels).toEqual(['Delete', 'Edit']);
  });

  /*
   * DEFECT: the author-only guard compared two `undefined` values with
   * `String()`, so it denied nothing. This case renders exactly that state:
   * no author id and no current user id. The guard now fails closed.
   * `ConversationItem.menu.test.tsx` holds the full truth table.
   */
  it('disables Delete and Edit when neither the author nor the current user is known', async () => {
    const user = userEvent.setup();
    renderItem({ conversation: baseConversation({ authorId: undefined }), currentUserId: undefined });

    await user.hover(screen.getByText('My conversation'));
    await user.click(screen.getByRole('button', { name: /more actions/i }));

    expect(await screen.findByRole('menuitem', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('menuitem', { name: 'Edit' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('disables the "Move to" item when the folder create/update permissions are both missing', async () => {
    mockPermissions(false);
    const user = userEvent.setup();
    renderItem();

    await user.hover(screen.getByText('My conversation'));
    await user.click(screen.getByRole('button', { name: /more actions/i }));
    const moveToItem = await screen.findByRole('menuitem', { name: /move to/i });

    expect(moveToItem).toHaveAttribute('aria-disabled', 'true');
  });

  it('enters edit mode for a new conversation and confirms via Enter, calling onCreateConversation', async () => {
    const user = userEvent.setup();
    const onCreateConversation = vi.fn().mockResolvedValue(undefined);
    renderItem({
      conversation: baseConversation({ isNew: true, name: '' }),
      onCreateConversation,
    });

    const input = screen.getByRole('textbox');
    await user.type(input, 'New chat name');
    await user.keyboard('{Enter}');

    expect(onCreateConversation).toHaveBeenCalledWith(expect.objectContaining({ name: 'New chat name', isNew: true }));
  });

  it('calls onCancelCreate when cancelling a new-conversation edit', async () => {
    const user = userEvent.setup();
    const onCancelCreate = vi.fn();
    renderItem({
      conversation: baseConversation({ isNew: true, name: '' }),
      onCancelCreate,
    });

    await user.click(screen.getByTestId('conversation-cancel-edit'));

    expect(onCancelCreate).toHaveBeenCalledTimes(1);
  });

  it('renders inside a DraggableConversationItem wrapper when enableDragAndDrop is set', () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    renderWithTheme(
      <QueryClientProvider client={queryClient}>
        <DndContext>
          <ConversationItem
            conversation={baseConversation()}
            onSelectConversation={vi.fn()}
            onDelete={vi.fn()}
            onExport={vi.fn()}
            onEdit={vi.fn()}
            onPlayback={vi.fn()}
            onPin={vi.fn()}
            onDuplicate={vi.fn()}
            onCreateConversation={vi.fn()}
            onCancelCreate={vi.fn()}
            onChangeActiveConversationName={vi.fn()}
            enableDragAndDrop
          />
        </DndContext>
      </QueryClientProvider>,
    );
    expect(screen.getByText('My conversation').closest('[aria-roledescription="draggable"]')).not.toBeNull();
  });
});
