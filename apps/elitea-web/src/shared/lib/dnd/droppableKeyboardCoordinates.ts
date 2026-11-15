import type { ClientRect, KeyboardCoordinateGetter } from '@dnd-kit/core';

type Direction = 'up' | 'down' | 'left' | 'right';

const DIRECTION_OF: Readonly<Record<string, Direction>> = {
  ArrowUp: 'up',
  ArrowDown: 'down',
  ArrowLeft: 'left',
  ArrowRight: 'right',
};

const centre = (rect: ClientRect): { readonly x: number; readonly y: number } => ({ x: rect.left + rect.width / 2, y: rect.top + rect.height / 2 });

function isBeyond(direction: Direction, from: { readonly x: number; readonly y: number }, to: { readonly x: number; readonly y: number }): boolean {
  if (direction === 'up') return to.y < from.y;
  if (direction === 'down') return to.y > from.y;
  if (direction === 'left') return to.x < from.x;
  return to.x > from.x;
}

/**
 * Keyboard movement of a drag between drop zones that are NOT sortable items.
 * dnd-kit's `sortableKeyboardCoordinates` needs the dragged element to be a
 * sortable too (it looks the dragged id up among the droppables), so it does
 * nothing for a plain draggable grip dropped on a folder. Each arrow key moves
 * the dragged element onto the nearest drop zone in that direction, centred on
 * it, so the collision detection puts the drag over that zone; at the last zone
 * in a direction the key does nothing.
 */
export const droppableKeyboardCoordinates: KeyboardCoordinateGetter = (event, { context: { collisionRect, droppableRects, droppableContainers, over } }) => {
  const direction = DIRECTION_OF[event.code];
  if (direction === undefined || !collisionRect) return undefined;
  event.preventDefault();
  const from = centre(collisionRect);
  let best: { readonly rect: ClientRect; readonly distance: number } | undefined;
  for (const container of droppableContainers.getEnabled()) {
    const rect = droppableRects.get(container.id);
    if (rect === undefined || container.id === over?.id) continue;
    const to = centre(rect);
    if (!isBeyond(direction, from, to)) continue;
    const distance = Math.hypot(to.x - from.x, to.y - from.y);
    if (best === undefined || distance < best.distance) best = { rect, distance };
  }
  if (best === undefined) return undefined;
  return { x: best.rect.left + best.rect.width / 2 - collisionRect.width / 2, y: best.rect.top + best.rect.height / 2 - collisionRect.height / 2 };
};
