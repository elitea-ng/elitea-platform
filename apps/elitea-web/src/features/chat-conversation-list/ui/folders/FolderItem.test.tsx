import { fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getPermissionListQueryKey } from '@/shared/api/generated/auth/auth';
import { getGetCurrentAuthorQueryKey } from '@/shared/api/generated/social/social';

import type { Conversation } from '@/entities/conversation';

import { createTestQueryClient, renderWithProviders } from '../../__tests__/testUtils';
import type { FolderListItem } from '../../lib/hooks/conversationListState.types';
import type { FolderItemCallbacks, FolderMoveTargetCallbacks } from './FolderItem';
import { FolderItem } from './FolderItem';

const PROJECT_ID = '7';
const CURRENT_USER_ID = 'author-1';

// jsdom has no ResizeObserver; `TypographyWithConditionalTooltip` (the
// folder title, rendered via `FolderAccordion`) mounts the real
// `useTextOverflow` hook, which creates one unconditionally — same stub
// `TypographyWithConditionalTooltip.test.tsx` itself already establishes.
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

beforeEach(() => {
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function mkFolder(overrides: Partial<FolderListItem> & Record<string, unknown> & { readonly id: string }): FolderListItem {
  return { name: overrides.id, conversations: [], ...overrides };
}

function stubRenderConversationItem(conversation: Conversation) {
  return <div key={conversation.id}>{conversation.name}</div>;
}

function primedClient(permissionNames: readonly string[] = ['models.chat.folders.delete', 'models.chat.folders.update']) {
  const client = createTestQueryClient();
  client.setQueryData(getPermissionListQueryKey(PROJECT_ID), {
    data: permissionNames.map((name) => ({ name, enabled: true })),
    status: 200,
    headers: new Headers(),
  });
  client.setQueryData(getGetCurrentAuthorQueryKey(), {
    data: { id: CURRENT_USER_ID, name: 'Current User', email: 'current@example.com', avatar: '', description: '', personal_project_id: '1' },
    status: 200,
    headers: new Headers(),
  });
  return client;
}

function mkCallbacks(overrides: Partial<FolderItemCallbacks> = {}): FolderItemCallbacks {
  return {
    onCreateFolder: vi.fn(),
    onCancelCreateFolder: vi.fn(),
    onEditFolder: vi.fn(),
    onPinFolder: vi.fn(),
    onDeleteFolder: vi.fn(),
    ...overrides,
  };
}

function mkMoveTarget(overrides: Partial<FolderMoveTargetCallbacks> = {}): FolderMoveTargetCallbacks {
  return {
    moveTargetConversationToNewFolder: vi.fn(),
    cancelMovingTargetConversationToNewFolder: vi.fn(),
    ...overrides,
  };
}

/**
 * The `ControlsDropdown` menu trigger's `visibility: visible` only comes
 * from a REAL CSS `:hover` pseudo-class rule (`FolderAccordion.tsx`'s own
 * `summaryContainerSx`, ported from the baseline's "only show the dot-menu
 * on hover" UX) — jsdom does not evaluate `:hover` matching from dispatched
 * mouse events at all (a jsdom limitation, not a defect: it works in a real
 * browser). Per the W3C accessible-name computation spec, an element inside
 * a `visibility: hidden` ancestor computes an EMPTY accessible name
 * regardless of its own `aria-label`, so `getByRole(..., {hidden: true})`
 * would find the element but not by its real name — queried by `aria-label`
 * attribute directly instead (a plain attribute selector, not an internal
 * Mui/Emotion class), clicked via `fireEvent` (not `userEvent`, which
 * additionally refuses to interact with a CSS-hidden element) so every
 * menu-interacting test below can still exercise the real click ->
 * `ControlsDropdown` path.
 */
function openFolderMenu(container: HTMLElement): void {
  const trigger = container.querySelector('[aria-label="Folder actions"]');
  expect(trigger).not.toBeNull();
  fireEvent.click(trigger as Element);
}

describe('FolderItem', () => {
  it('renders the folder name as the accordion title when not editing', () => {
    const { getByRole } = renderWithProviders(
      <FolderItem
        folder={mkFolder({ id: 'f1', name: 'My Folder', owner_id: CURRENT_USER_ID })}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks()}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    expect(getByRole('button', { name: 'My Folder' })).toBeInTheDocument();
  });

  it('renders the inline editor immediately for a not-yet-persisted (isNew) folder', () => {
    const { getByRole } = renderWithProviders(
      <FolderItem
        folder={mkFolder({ id: 'draft-1', name: '', isNew: true, owner_id: CURRENT_USER_ID })}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks()}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    expect(getByRole('textbox')).toBeInTheDocument();
  });

  it('disables Delete/Edit/Pin in the menu when the current user does not own the folder', () => {
    const { container, getByRole } = renderWithProviders(
      <FolderItem
        folder={mkFolder({ id: 'f1', name: 'Someone else’s folder', owner_id: 'someone-else' })}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks()}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    openFolderMenu(container);
    expect(getByRole('menuitem', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true');
    expect(getByRole('menuitem', { name: 'Edit' })).toHaveAttribute('aria-disabled', 'true');
    expect(getByRole('menuitem', { name: 'Pin on top' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('disables Delete/Edit/Pin when the folder has no owner_id at all (entities/folder gap)', () => {
    const { container, getByRole } = renderWithProviders(
      <FolderItem
        folder={mkFolder({ id: 'f1', name: 'No owner recorded' })}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks()}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    openFolderMenu(container);
    expect(getByRole('menuitem', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('enables Delete/Edit/Pin when the current user owns the folder and has permission', () => {
    const { container, getByRole } = renderWithProviders(
      <FolderItem
        folder={mkFolder({ id: 'f1', name: 'My Folder', owner_id: CURRENT_USER_ID })}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks()}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    openFolderMenu(container);
    // MUI's `MenuItem` only renders `aria-disabled` at all when `disabled`
    // is truthy — `disabled={false}` omits the attribute entirely rather
    // than writing `"false"`, so the enabled case asserts absence, not a
    // literal `"false"` string.
    expect(getByRole('menuitem', { name: 'Delete' })).not.toHaveAttribute('aria-disabled');
    expect(getByRole('menuitem', { name: 'Edit' })).not.toHaveAttribute('aria-disabled');
    expect(getByRole('menuitem', { name: 'Pin on top' })).not.toHaveAttribute('aria-disabled');
  });

  it('keeps Delete/Edit/Pin disabled for the owner when the required permission is missing, even with ownership', () => {
    const { container, getByRole } = renderWithProviders(
      <FolderItem
        folder={mkFolder({ id: 'f1', name: 'My Folder', owner_id: CURRENT_USER_ID })}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks()}
        moveTarget={mkMoveTarget()}
      />,
      primedClient([]),
    );
    openFolderMenu(container);
    expect(getByRole('menuitem', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true');
    expect(getByRole('menuitem', { name: 'Edit' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('shows "Unpin" once the folder is pinned', () => {
    const { container, getByRole } = renderWithProviders(
      <FolderItem
        folder={mkFolder({ id: 'f1', name: 'My Folder', owner_id: CURRENT_USER_ID, isPinned: true })}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks()}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    openFolderMenu(container);
    expect(getByRole('menuitem', { name: 'Unpin' })).toBeInTheDocument();
  });

  // ISSUE 851. The folder menu used to carry an Export row that was hardcoded
  // disabled, with two children labelled `Option1`/`Option2` wired to a
  // callback nothing supplied — a named control that did nothing in every
  // build. Conversation export is now real; FOLDER export is not, and there is
  // no route for it, so the row is gone rather than disabled: a disabled
  // control with named-but-inert options reads as "coming very soon".
  it('offers no Export row at all — there is no folder export', () => {
    const { container, queryByRole } = renderWithProviders(
      <FolderItem
        folder={mkFolder({ id: 'f1', name: 'My Folder', owner_id: CURRENT_USER_ID })}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks()}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    openFolderMenu(container);
    expect(queryByRole('menuitem', { name: 'Export' })).not.toBeInTheDocument();
    expect(queryByRole('menuitem', { name: 'Option1' })).not.toBeInTheDocument();
  });

  it('calls onPinFolder(folder, true) when Pin on top is clicked from an unpinned folder', async () => {
    const user = userEvent.setup();
    const onPinFolder = vi.fn();
    const folder = mkFolder({ id: 'f1', name: 'My Folder', owner_id: CURRENT_USER_ID });
    const { container, getByRole } = renderWithProviders(
      <FolderItem
        folder={folder}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks({ onPinFolder })}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    openFolderMenu(container);
    await user.click(getByRole('menuitem', { name: 'Pin on top' }));
    expect(onPinFolder).toHaveBeenCalledWith(folder, true);
  });

  it('switches to the inline editor when Edit is clicked, and saves the renamed folder on Enter', async () => {
    const user = userEvent.setup();
    const onEditFolder = vi.fn();
    const folder = mkFolder({ id: 'f1', name: 'My Folder', owner_id: CURRENT_USER_ID });
    const { container, getByRole } = renderWithProviders(
      <FolderItem
        folder={folder}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks({ onEditFolder })}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    openFolderMenu(container);
    await user.click(getByRole('menuitem', { name: 'Edit' }));

    const input = getByRole('textbox');
    await user.clear(input);
    await user.type(input, 'Renamed Folder{Enter}');

    expect(onEditFolder).toHaveBeenCalledWith({ ...folder, name: 'Renamed Folder' });
  });

  it('cancels a brand-new folder draft via onCancelCreateFolder', async () => {
    const user = userEvent.setup();
    const onCancelCreateFolder = vi.fn();
    const folder = mkFolder({ id: 'draft-1', name: '', isNew: true, owner_id: CURRENT_USER_ID });
    const { getByRole } = renderWithProviders(
      <FolderItem
        folder={folder}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks({ onCancelCreateFolder })}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    await user.click(getByRole('button', { name: 'Cancel' }));
    expect(onCancelCreateFolder).toHaveBeenCalledWith(folder);
  });

  it('disables the confirm button while the folder name is invalid', () => {
    const { getByRole } = renderWithProviders(
      <FolderItem
        folder={mkFolder({ id: 'draft-1', name: '', isNew: true, owner_id: CURRENT_USER_ID })}
        projectId={PROJECT_ID}
        renderConversationItem={stubRenderConversationItem}
        callbacks={mkCallbacks()}
        moveTarget={mkMoveTarget()}
      />,
      primedClient(),
    );
    expect(getByRole('button', { name: 'Confirm' })).toBeDisabled();
  });
});
