import type { Announcements, ScreenReaderInstructions, UniqueIdentifier } from '@dnd-kit/core';

/**
 * What the screen reader says during a drag. A screen supplies the words for
 * its own items and groups; the sequence (picked up, over, dropped, cancelled)
 * is the same everywhere. `itemName` and `targetName` return undefined for an
 * id that is not an item or a zone, which is announced as nothing.
 */
export interface DndAnnouncementLabels {
  readonly itemName: (id: UniqueIdentifier) => string | undefined;
  /** The drop zone's name: a folder's name, or "No folder"; undefined when `id` is not a zone. */
  readonly targetName: (id: UniqueIdentifier) => string | undefined;
  readonly pickedUp: (name: string) => string;
  readonly over: (name: string, target: string) => string;
  readonly overNothing: (name: string) => string;
  readonly dropped: (name: string, target: string) => string;
  readonly cancelled: (name: string) => string;
}

/** The instructions a keyboard user hears when the draggable gets focus. */
export function dndInstructions(text: string): ScreenReaderInstructions {
  return { draggable: text };
}

/** dnd-kit's `accessibility.announcements`, built from a screen's own words. */
export function buildDndAnnouncements(labels: DndAnnouncementLabels): Announcements {
  const name = (id: UniqueIdentifier): string => labels.itemName(id) ?? String(id);
  return {
    onDragStart: ({ active }) => labels.pickedUp(name(active.id)),
    onDragOver: ({ active, over }) => {
      const target = over === null ? undefined : labels.targetName(over.id);
      return target === undefined ? labels.overNothing(name(active.id)) : labels.over(name(active.id), target);
    },
    onDragEnd: ({ active, over }) => {
      const target = over === null ? undefined : labels.targetName(over.id);
      return target === undefined ? labels.cancelled(name(active.id)) : labels.dropped(name(active.id), target);
    },
    onDragCancel: ({ active }) => labels.cancelled(name(active.id)),
  };
}
