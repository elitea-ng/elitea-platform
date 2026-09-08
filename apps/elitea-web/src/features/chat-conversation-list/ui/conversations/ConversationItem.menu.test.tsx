/**
 * DEFECT: the row menu's author-only guard on Delete and Edit read
 * `String(currentUserId) !== String(conversation.authorId)`.
 *
 * Both operands were always `undefined` in the running app. The folders wire
 * normaliser (`entities/folder/api/foldersApi.ts`) dropped the `author_id`
 * that the Go handler emits. The only composition root
 * (`processes/chat/model/useConversationSidebar.ts`) never set the optional
 * `currentUserId` prop. The comparison was therefore always false. Any
 * project member saw Delete and Edit enabled on another member's shared
 * conversation.
 *
 * The guard now fails closed. One exemption stays for a draft row, which the
 * wire has not described yet.
 */
import type { ControlsDropdownItem } from '@/shared/ui/ControlsDropdown';
import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';
import { describe, expect, it, vi } from 'vitest';

import { buildActiveMenuItems } from './ConversationItem.menu';
import type { MenuItemsParams } from './ConversationItem.menu';
import type { ConversationWithOwnerMeta } from './ConversationItem.types';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function params(conversation: ConversationWithOwnerMeta, currentUserId: string | undefined): MenuItemsParams {
  return {
    conversation,
    isActive: false,
    isEditingCanvas: false,
    currentUserId,
    moveToFoldersMenuItems: [],
    canMoveToFolders: true,
    isPublicOrPersonal: false,
    isPersonal: false,
    theme,
    onDelete: vi.fn(),
    onEdit: vi.fn(),
    onMakePublic: vi.fn(),
    onShare: vi.fn(),
    onShareByLink: vi.fn(),
    onPlayback: vi.fn(),
    onPin: vi.fn(),
  };
}

function disabledByKey(items: readonly ControlsDropdownItem[], key: string): boolean | undefined {
  return items.find((item) => item.key === key)?.disabled;
}

const conversation: ConversationWithOwnerMeta = { id: 'conv-1', name: 'Shared chat', isPrivate: false };

describe('buildActiveMenuItems — author-only guard', () => {
  it('enables Delete and Edit for the author', () => {
    const items = buildActiveMenuItems(params({ ...conversation, authorId: 'user-1' }, 'user-1'));
    expect(disabledByKey(items, 'delete')).toBe(false);
    expect(disabledByKey(items, 'edit')).toBe(false);
  });

  it('disables Delete and Edit for another author', () => {
    const items = buildActiveMenuItems(params({ ...conversation, authorId: 'user-2' }, 'user-1'));
    expect(disabledByKey(items, 'delete')).toBe(true);
    expect(disabledByKey(items, 'edit')).toBe(true);
  });

  it('disables Delete and Edit when the conversation has no known author', () => {
    const items = buildActiveMenuItems(params(conversation, 'user-1'));
    expect(disabledByKey(items, 'delete')).toBe(true);
  });

  // The exact state the old comparison let through. `String(undefined)`
  // equals `String(undefined)`, so the guard enabled both actions.
  it('disables Delete and Edit when neither identity is known', () => {
    const items = buildActiveMenuItems(params(conversation, undefined));
    expect(disabledByKey(items, 'delete')).toBe(true);
    expect(disabledByKey(items, 'edit')).toBe(true);
  });

  it('disables Delete and Edit when the current user is not known yet', () => {
    const items = buildActiveMenuItems(params({ ...conversation, authorId: 'user-2' }, undefined));
    expect(disabledByKey(items, 'delete')).toBe(true);
  });

  // A local draft carries no `author_id`. No production code sets `isNew` on
  // a sidebar row today. This case guards the branch against a later change.
  it('keeps Delete and Edit enabled on a not-yet-persisted draft row', () => {
    const items = buildActiveMenuItems(params({ ...conversation, isNew: true }, 'user-1'));
    expect(disabledByKey(items, 'delete')).toBe(false);
    expect(disabledByKey(items, 'edit')).toBe(false);
  });

  // The wire sends `author_id` as an integer. A numeric id that matches must
  // still count as the author.
  it('matches a numeric author id against a string user id', () => {
    const items = buildActiveMenuItems(params({ ...conversation, authorId: '7' }, '7'));
    expect(disabledByKey(items, 'delete')).toBe(false);
  });
});

/**
 * ISSUE 851 — the Export entry was a named control that did nothing.
 *
 * It was built with `disabled: true` hardcoded and two children labelled
 * `Option1`/`Option2` that wired an `onClick` only when an optional `onExport`
 * happened to be supplied. Nothing supplied one, and there was no server route
 * either, so the row read as "coming very soon" in every build there has ever
 * been.
 */
describe('buildActiveMenuItems — the Export entry', () => {
  function exportItem(items: readonly ControlsDropdownItem[]): ControlsDropdownItem | undefined {
    return items.find((item) => item.key === 'export');
  }

  it('offers one entry per format, each carrying a handler, when an exporter is wired', () => {
    const onExport = vi.fn();
    const items = buildActiveMenuItems({ ...params({ ...conversation, authorId: 'user-1' }, 'user-1'), onExport });

    const item = exportItem(items);
    expect(item?.disabled, 'a wired Export entry must be reachable').toBe(false);
    expect(item?.items?.map((child) => child.key)).toEqual(['export-markdown', 'export-json']);
    // The labels name a real format, not a placeholder.
    expect(item?.items?.map((child) => child.label)).toEqual(['Markdown (.md)', 'JSON (.json)']);
  });

  it('asks the exporter for the format the clicked entry names', () => {
    const onExport = vi.fn();
    const items = buildActiveMenuItems({ ...params({ ...conversation, authorId: 'user-1' }, 'user-1'), onExport });
    const children = exportItem(items)?.items ?? [];

    children.find((child) => child.key === 'export-markdown')?.onClick?.();
    expect(onExport).toHaveBeenCalledWith('md');

    children.find((child) => child.key === 'export-json')?.onClick?.();
    expect(onExport).toHaveBeenCalledWith('json');
    expect(onExport).toHaveBeenCalledTimes(2);
  });

  // The honest state for a surface that offers no exporter: disabled, rather
  // than open onto options wired to nothing.
  it('disables the entry when no exporter is wired', () => {
    const items = buildActiveMenuItems(params({ ...conversation, authorId: 'user-1' }, 'user-1'));
    const item = exportItem(items);
    expect(item?.disabled).toBe(true);
    for (const child of item?.items ?? []) {
      expect(child.onClick, `${child.key} must not pretend to do something`).toBeUndefined();
    }
  });
});
