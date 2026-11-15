import { describe, expect, it, vi } from 'vitest';

import { droppableKeyboardCoordinates } from './droppableKeyboardCoordinates';

const rect = (left: number, top: number, width = 100, height = 20) => ({ left, top, width, height, right: left + width, bottom: top + height });

/** The getter's third argument, with only the parts it reads. */
function run(code: string, collisionRect: ReturnType<typeof rect> | null, zones: Record<string, ReturnType<typeof rect>>, over?: string) {
  const preventDefault = vi.fn();
  const event = { code, preventDefault } as unknown as KeyboardEvent;
  const droppableContainers = { getEnabled: () => Object.keys(zones).map((id) => ({ id })) };
  const droppableRects = new Map(Object.entries(zones));
  const coordinates = droppableKeyboardCoordinates(event, {
    active: 'grip',
    currentCoordinates: { x: 0, y: 0 },
    context: { collisionRect, droppableRects, droppableContainers, over: over === undefined ? null : { id: over } },
  } as never);
  return { coordinates, preventDefault };
}

describe('droppableKeyboardCoordinates', () => {
  const grip = rect(0, 100, 20, 20);
  const zones = { top: rect(300, 40), middle: rect(300, 100), low: rect(300, 160) };

  it('moves right onto the nearest zone, centred on it', () => {
    const { coordinates, preventDefault } = run('ArrowRight', grip, zones);
    expect(preventDefault).toHaveBeenCalled();
    // The middle zone is level with the grip: its centre (350, 110) less half the grip's size.
    expect(coordinates).toEqual({ x: 340, y: 100 });
  });

  it('moves up and down between zones, never back onto the one it is over', () => {
    expect(run('ArrowUp', rect(340, 100, 20, 20), zones, 'middle').coordinates).toEqual({ x: 340, y: 40 });
    expect(run('ArrowDown', rect(340, 100, 20, 20), zones, 'middle').coordinates).toEqual({ x: 340, y: 160 });
  });

  it('does nothing past the last zone in a direction, for another key, or with no rect', () => {
    expect(run('ArrowLeft', grip, zones).coordinates).toBeUndefined();
    expect(run('ArrowUp', rect(340, 40, 20, 20), zones, 'top').coordinates).toBeUndefined();
    expect(run('KeyA', grip, zones).coordinates).toBeUndefined();
    expect(run('ArrowRight', null, zones).coordinates).toBeUndefined();
  });
});
