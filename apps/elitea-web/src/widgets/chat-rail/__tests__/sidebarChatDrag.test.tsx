/**
 * A drag of a sidebar conversation row, through the web list's drag model
 * (the seam the web list's own `useDragAndDrop` tests drive): onto a folder
 * moves it there, out of a folder onto the recent chats takes it out, and a
 * pinned row, a drop on its own folder and a drop on nothing do not move.
 * Every move goes through the rail's one move path.
 */
import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { Conversation } from '@/entities/conversation';

import { folderDropId, UNGROUPED_DROP_ID, useSidebarChatDrag } from '../model/useSidebarChatDrag';

const conversation = (id: string, extra: Partial<Conversation> = {}): Conversation => ({ id, name: `chat ${id}`, isPrivate: true, ...extra });

function rail() {
  return {
    pinned: [conversation('p1', { isPinned: true })],
    dateGroups: [{ name: 'Today', conversations: [conversation('41')] }],
    folders: [
      { id: 'f1', name: 'Specs', conversations: [conversation('51')] },
      { id: 'f2', name: 'Drafts', conversations: [] },
    ],
    moveToFolder: vi.fn().mockResolvedValue(undefined),
  };
}

const start = (id: string) => ({ active: { id, data: { current: undefined }, rect: { current: { initial: null, translated: null } } } }) as never;
const end = (id: string, over: string | null) =>
  ({ active: { id, data: { current: undefined }, rect: { current: { initial: null, translated: null } } }, over: over === null ? null : { id: over, data: { current: undefined }, rect: {} } }) as never;

async function drop(model: ReturnType<typeof rail>, id: string, over: string | null) {
  const { result } = renderHook(() => useSidebarChatDrag(model));
  act(() => result.current.handleDragStart(start(id)));
  await act(async () => {
    await result.current.handleDragEnd(end(id, over));
  });
  return result;
}

describe('useSidebarChatDrag', () => {
  it('moves a recent conversation into the folder it is dropped on', async () => {
    const model = rail();
    await drop(model, '41', folderDropId('f1'));
    expect(model.moveToFolder).toHaveBeenCalledTimes(1);
    expect(model.moveToFolder.mock.calls[0]?.[0]).toMatchObject({ id: '41' });
    expect(model.moveToFolder.mock.calls[0]?.[1]).toBe('f1');
  });

  it('takes a conversation out of its folder when it is dropped on the recent chats', async () => {
    const model = rail();
    await drop(model, '51', UNGROUPED_DROP_ID);
    expect(model.moveToFolder).toHaveBeenCalledTimes(1);
    expect(model.moveToFolder.mock.calls[0]?.[0]).toMatchObject({ id: '51', folderId: 'f1' });
    expect(model.moveToFolder.mock.calls[0]?.[1]).toBeNull();
  });

  it('moves a conversation from one folder to another', async () => {
    const model = rail();
    await drop(model, '51', folderDropId('f2'));
    expect(model.moveToFolder.mock.calls[0]?.[1]).toBe('f2');
  });

  it('does not move a pinned row, a row onto its own folder, a recent row onto the recent chats, or a drop on nothing', async () => {
    const model = rail();
    await drop(model, 'p1', folderDropId('f1'));
    await drop(model, '51', folderDropId('f1'));
    await drop(model, '41', UNGROUPED_DROP_ID);
    await drop(model, '41', null);
    expect(model.moveToFolder).not.toHaveBeenCalled();
  });

  it('marks the valid drop targets while a row is dragged', () => {
    const model = rail();
    const { result } = renderHook(() => useSidebarChatDrag(model));
    act(() => result.current.handleDragStart(start('51')));
    expect(result.current.getDropAreaState(folderDropId('f2')).isValidDropTarget).toBe(true);
    expect(result.current.getDropAreaState(folderDropId('f1')).isValidDropTarget).toBe(false);
    expect(result.current.getDropAreaState(UNGROUPED_DROP_ID).isValidDropTarget).toBe(true);
  });
});
