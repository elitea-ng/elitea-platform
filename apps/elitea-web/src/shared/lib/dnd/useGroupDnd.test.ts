import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { buildDndAnnouncements, dndInstructions } from './announcements';
import type { CustomDrag, DndPolicy } from './types';
import { useGroupDnd } from './useGroupDnd';

interface Item {
  readonly id: string;
  readonly group?: string;
}
interface Group {
  readonly id: string;
}

const A: Group = { id: 'a' };
const B: Group = { id: 'b' };

function policy(overrides: Partial<DndPolicy<Item, Group>> = {}, items: readonly Item[] = [{ id: 'one' }]): DndPolicy<Item, Group> {
  return {
    resolveDrag: (activeId) => {
      const item = items.find((candidate) => candidate.id === activeId);
      if (item === undefined) return undefined;
      return { items: [item], source: item.group === undefined ? null : { id: item.group } };
    },
    resolveDrop: (overId) => (overId === 'root' ? { group: null, label: 'No folder' } : overId === 'a' ? { group: A, label: 'A' } : overId === 'b' ? { group: B, label: 'B' } : undefined),
    canMove: (item, target) => (target === null ? item.group !== undefined : item.group !== target.id),
    move: vi.fn().mockResolvedValue(undefined),
    groupKey: (group) => group.id,
    messages: { moveError: 'could not move', moved: (count, label) => `${count} moved to ${label}` },
    ...overrides,
  };
}

const start = (id: string) => ({ active: { id } }) as never;
const end = (activeId: string, overId: string | undefined) => ({ active: { id: activeId }, over: overId === undefined ? null : { id: overId, data: { current: undefined } } }) as never;

describe('useGroupDnd', () => {
  it('moves the dragged item to the dropped group and forgets the drag', async () => {
    const move = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useGroupDnd(policy({ move })));
    act(() => result.current.handleDragStart(start('one')));
    expect(result.current.isDragging).toBe(true);
    expect(result.current.draggedItems).toEqual([{ id: 'one' }]);
    await act(async () => {
      await result.current.handleDragEnd(end('one', 'a'));
    });
    expect(move).toHaveBeenCalledWith({ id: 'one' }, A);
    expect(result.current.isDragging).toBe(false);
    expect(result.current.draggedItems).toEqual([]);
  });

  it('skips a move the policy refuses, a drop outside any zone and a drop with nothing picked up', async () => {
    const move = vi.fn().mockResolvedValue(undefined);
    const { result } = renderHook(() => useGroupDnd(policy({ move }, [{ id: 'one', group: 'a' }])));
    act(() => result.current.handleDragStart(start('one')));
    await act(async () => {
      await result.current.handleDragEnd(end('one', 'a'));
    });
    act(() => result.current.handleDragStart(start('one')));
    await act(async () => {
      await result.current.handleDragEnd(end('one', 'nowhere'));
    });
    await act(async () => {
      await result.current.handleDragEnd(end('one', 'b'));
    });
    expect(move).not.toHaveBeenCalled();
  });

  it('reports each refusal once through toastError', async () => {
    const toastError = vi.fn();
    const { result } = renderHook(() => useGroupDnd(policy({ move: vi.fn().mockRejectedValue(new Error('no')) }), { toastError }));
    act(() => result.current.handleDragStart(start('one')));
    await act(async () => {
      await result.current.handleDragEnd(end('one', 'a'));
    });
    expect(toastError).toHaveBeenCalledExactlyOnceWith('could not move');
  });

  it('says how many moved only after a drop of several items', async () => {
    const toastSuccess = vi.fn();
    const items: Item[] = [{ id: 'one' }, { id: 'two' }];
    const many = policy({ resolveDrag: () => ({ items, source: null }) });
    const { result } = renderHook(() => useGroupDnd(many, { toastSuccess }));
    act(() => result.current.handleDragStart(start('one')));
    await act(async () => {
      await result.current.handleDragEnd(end('one', 'a'));
    });
    expect(toastSuccess).toHaveBeenCalledWith('2 moved to A');
  });

  it('ends a drag without a drop on cancel', () => {
    const { result } = renderHook(() => useGroupDnd(policy()));
    act(() => result.current.handleDragStart(start('one')));
    act(() => result.current.handleDragCancel());
    expect(result.current.isDragging).toBe(false);
  });

  it('marks a zone valid unless the item is already there, and "no folder" only for an item in a group', () => {
    const { result } = renderHook(() => useGroupDnd(policy({}, [{ id: 'one', group: 'a' }])));
    expect(result.current.getDropAreaState('b')).toEqual({ isValidDropTarget: false, isActive: false });
    act(() => result.current.handleDragStart(start('one')));
    expect(result.current.getDropAreaState('b')).toEqual({ isValidDropTarget: true, isActive: true });
    expect(result.current.getDropAreaState('a')).toEqual({ isValidDropTarget: false, isActive: false });
    expect(result.current.getDropAreaState('root')).toEqual({ isValidDropTarget: true, isActive: true });
    expect(result.current.getDropAreaState('unknown')).toEqual({ isValidDropTarget: false, isActive: false });
  });

  it('hands a drag it does not own to the custom drag', async () => {
    const onEnd = vi.fn().mockResolvedValue(undefined);
    let active = false;
    const custom: CustomDrag = {
      onStart: (id) => {
        active = id === 'special';
        return active;
      },
      isActive: () => active,
      onEnd,
      onReset: () => {
        active = false;
      },
      dropAreaState: () => (active ? { isValidDropTarget: true, isActive: true } : undefined),
    };
    const move = vi.fn();
    const { result } = renderHook(() => useGroupDnd(policy({ move }), { custom }));
    act(() => result.current.handleDragStart(start('special')));
    expect(result.current.draggedItems).toEqual([]);
    expect(result.current.getDropAreaState('anything')).toEqual({ isValidDropTarget: true, isActive: true });
    await act(async () => {
      await result.current.handleDragEnd(end('special', 'a'));
    });
    expect(onEnd).toHaveBeenCalledTimes(1);
    expect(move).not.toHaveBeenCalled();
  });
});

describe('buildDndAnnouncements', () => {
  const announcements = buildDndAnnouncements({
    itemName: (id) => (id === 'item' ? 'Agent' : undefined),
    targetName: (id) => (id === 'folder' ? 'Support' : undefined),
    pickedUp: (name) => `up ${name}`,
    over: (name, target) => `${name} over ${target}`,
    overNothing: (name) => `${name} over nothing`,
    dropped: (name, target) => `${name} dropped ${target}`,
    cancelled: (name) => `${name} cancelled`,
  });
  const event = (over: string | null) => ({ active: { id: 'item' }, over: over === null ? null : { id: over } }) as never;

  it('speaks each step with the screen\'s own words', () => {
    expect(announcements.onDragStart?.(event(null))).toBe('up Agent');
    expect(announcements.onDragOver?.(event('folder'))).toBe('Agent over Support');
    expect(announcements.onDragOver?.(event('elsewhere'))).toBe('Agent over nothing');
    expect(announcements.onDragEnd?.(event('folder'))).toBe('Agent dropped Support');
    expect(announcements.onDragEnd?.(event(null))).toBe('Agent cancelled');
    expect(announcements.onDragCancel?.(event(null))).toBe('Agent cancelled');
  });

  it('wraps the instructions for the draggable', () => {
    expect(dndInstructions('press Space')).toEqual({ draggable: 'press Space' });
  });
});
