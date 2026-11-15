import { useCallback, useRef, useState } from 'react';

import { KeyboardSensor, PointerSensor, useSensor, useSensors } from '@dnd-kit/core';
import type { DragEndEvent, DragStartEvent, UniqueIdentifier } from '@dnd-kit/core';
import { sortableKeyboardCoordinates } from '@dnd-kit/sortable';

import type { DndPolicy, DropAreaState, GroupDnd, GroupDndOptions } from './types';

const NO_DROP: DropAreaState = { isValidDropTarget: false, isActive: false };

/**
 * Sequentially attempts to move every item the policy lets move onto `target`,
 * reporting each failure and returning how many succeeded. Sequential by
 * design: each move must resolve (and be individually catchable) before the
 * next starts, there is no batched "move many" endpoint.
 */
async function moveAll<TItem, TGroup>(
  items: readonly TItem[],
  target: TGroup | null,
  policy: DndPolicy<TItem, TGroup>,
  onError: (message: string) => void,
): Promise<number> {
  let moved = 0;
  for (const item of items) {
    if (!policy.canMove(item, target)) continue;
    try {
      // eslint-disable-next-line no-await-in-loop -- sequential by design, see the function comment.
      await policy.move(item, target);
      moved++;
    } catch {
      onError(policy.messages.moveError);
    }
  }
  return moved;
}

/** The item drop: resolve the zone, move every item the policy lets move, and say how it went. */
async function dropItems<TItem, TGroup>(
  items: readonly TItem[],
  overId: UniqueIdentifier,
  policy: DndPolicy<TItem, TGroup>,
  toastError: ((message: string) => void) | undefined,
  toastSuccess: ((message: string) => void) | undefined,
): Promise<void> {
  if (items.length === 0) return;
  const drop = policy.resolveDrop(overId);
  if (drop === undefined) return;
  const onError = toastError ?? ((): void => {});
  try {
    const moved = await moveAll(items, drop.group, policy, onError);
    if (moved > 0 && items.length > 1) toastSuccess?.(policy.messages.moved(moved, drop.label));
  } catch {
    onError(policy.messages.moveError);
  }
}

/** Whether `group` is a legal drop target for a drag that started in `source`: never where it already is. */
function isValidTarget<TItem, TGroup>(policy: DndPolicy<TItem, TGroup>, source: TGroup | null, group: TGroup | null): boolean {
  if (group === null) return source !== null;
  return source === null || policy.groupKey(source) !== policy.groupKey(group);
}

/**
 * Entity-agnostic drag and drop of items between groups (a folder, or "no
 * folder"): pointer and keyboard sensors, the dragged-item state, the
 * sequential move with per-item failure reporting, and the drop-zone state a
 * zone styles itself from. What an item is, what a group is, which moves are
 * allowed and what the messages say is the policy's; the chat's conversation
 * rail and the Workshop lists both pass one.
 *
 * `custom` lets a caller own a second kind of drag in the same context (the
 * chat's folder reordering) without this hook knowing about it.
 */
export function useGroupDnd<TItem, TGroup>(
  policy: DndPolicy<TItem, TGroup>,
  options: GroupDndOptions = {},
): GroupDnd<TItem, TGroup> {
  const { custom, toastSuccess, toastError } = options;
  const [activeId, setActiveId] = useState<UniqueIdentifier | null>(null);
  const [draggedItems, setDraggedItems] = useState<readonly TItem[]>([]);
  const [draggedFrom, setDraggedFrom] = useState<TGroup | null>(null);

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 8 } }),
    // Keyboard drag (Space, arrows, Space): the same coordinate getter the chat rail has always used.
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  // Everything the drag handlers read but do not own, in one ref: the policy is
  // rebuilt by its caller on every render, and the handlers fire on a later drag
  // event, by which point a render has always refreshed the ref.
  const latest = useRef({ activeId, draggedItems, policy, custom, toastSuccess, toastError });
  latest.current = { activeId, draggedItems, policy, custom, toastSuccess, toastError };

  const reset = useCallback((): void => {
    setActiveId(null);
    setDraggedItems([]);
    setDraggedFrom(null);
    custom?.onReset();
  }, [custom]);

  const handleDragStart = useCallback(
    (event: DragStartEvent): void => {
      const { active } = event;
      setActiveId(active.id);
      if (custom?.onStart(active.id) === true) {
        setDraggedItems([]);
        setDraggedFrom(null);
        return;
      }
      const drag = latest.current.policy.resolveDrag(active.id);
      if (drag === undefined) return;
      setDraggedItems(drag.items);
      setDraggedFrom(drag.source);
    },
    [custom],
  );

  const handleDragEnd = useCallback(
    async (event: DragEndEvent): Promise<void> => {
      const { over, active } = event;
      const current = latest.current;
      if (current.activeId !== null && active.id !== current.activeId) {
        reset();
        return;
      }
      const items = [...current.draggedItems];
      const customDrag = current.custom?.isActive() === true;
      reset();
      if (over === null || over === undefined) return;
      if (customDrag) {
        await current.custom?.onEnd(event);
        return;
      }
      await dropItems(items, over.id, current.policy, current.toastError, current.toastSuccess);
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps -- every `latest.current.*` read is intentionally unlisted: the ref always holds the values of the render that last ran, read synchronously, never a stale closure.
    [reset],
  );

  const handleDragOver = useCallback((): void => {}, []);

  // Escape (or a lost pointer) ends a drag without a drop: forget it.
  const handleDragCancel = useCallback((): void => {
    reset();
  }, [reset]);

  const getDropAreaState = useCallback(
    (dropAreaId: string): DropAreaState => {
      if (activeId === null) return NO_DROP;
      const customState = custom?.dropAreaState(dropAreaId);
      if (customState !== undefined) return customState;
      if (draggedItems.length === 0) return NO_DROP;
      const current = latest.current.policy;
      const drop = current.resolveDrop(dropAreaId);
      if (drop === undefined) return NO_DROP;
      const valid = isValidTarget(current, draggedFrom, drop.group);
      return { isValidDropTarget: valid, isActive: valid };
    },
    [activeId, custom, draggedItems, draggedFrom],
  );

  return {
    sensors,
    activeId,
    draggedItems,
    draggedFrom,
    isDragging: activeId !== null,
    handleDragStart,
    handleDragEnd,
    handleDragOver,
    handleDragCancel,
    getDropAreaState,
  };
}
