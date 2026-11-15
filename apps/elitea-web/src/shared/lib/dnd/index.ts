/**
 * Public API — entity-agnostic drag and drop of items between groups (ADR-0051
 * M2, issue 1628). A screen passes a policy; the hook owns the sensors (pointer
 * and keyboard), the drag state and the sequential move.
 */
export { useGroupDnd } from './useGroupDnd';
export { buildDndAnnouncements, dndInstructions } from './announcements';
export type { DndAnnouncementLabels } from './announcements';
export type { CustomDrag, DndPolicy, DropAreaState } from './types';
