import type { DragEndEvent, DragStartEvent, KeyboardCoordinateGetter, UniqueIdentifier, useSensors } from '@dnd-kit/core';

/**
 * What a drag-and-drop screen tells the generic hook. `TItem` is the thing
 * dragged (a conversation, an agent), `TGroup` the thing it is dropped into (a
 * folder); the root, "no group", is `null` everywhere.
 */
export interface DndPolicy<TItem, TGroup> {
  /** The items an active draggable id stands for (a multi-selection, or the one item) and the group they come from; undefined for an id that is not an item. */
  readonly resolveDrag: (activeId: UniqueIdentifier) => { readonly items: readonly TItem[]; readonly source: TGroup | null } | undefined;
  /** What a droppable id means: a group, the root (`group: null`), or undefined for a zone that accepts nothing. `label` names it for messages and announcements. */
  readonly resolveDrop: (overId: UniqueIdentifier) => { readonly group: TGroup | null; readonly label: string } | undefined;
  /** Whether this item may move to the target; false skips it silently (a no-op move, a locked item). */
  readonly canMove: (item: TItem, target: TGroup | null) => boolean;
  /** The write. A rejection is reported once per item through `messages.moveError`. */
  readonly move: (item: TItem, target: TGroup | null) => Promise<unknown>;
  /** A stable identity of a group, to tell whether two are the same one. */
  readonly groupKey: (group: TGroup) => string;
  readonly messages: {
    readonly moveError: string;
    /** Said after a drop of more than one item that moved `count` of them. */
    readonly moved: (count: number, targetLabel: string) => string;
  };
}

export interface DropAreaState {
  readonly isValidDropTarget: boolean;
  readonly isActive: boolean;
}

/** A second kind of drag a screen owns inside the same context (the chat's folder reordering). */
export interface CustomDrag {
  /** True when this id starts the custom drag; the generic item drag is then not started. */
  readonly onStart: (activeId: UniqueIdentifier) => boolean;
  /** Whether a custom drag is in flight right now. */
  readonly isActive: () => boolean;
  readonly onEnd: (event: DragEndEvent) => Promise<void>;
  readonly onReset: () => void;
  /** The state of a drop zone during a custom drag; undefined while none is in flight. */
  readonly dropAreaState: (dropAreaId: string) => DropAreaState | undefined;
}

export interface GroupDndOptions {
  /** How an arrow key moves a keyboard drag. Default: dnd-kit's sortable getter, for sortable items; plain draggables dropped on drop zones pass `droppableKeyboardCoordinates`. */
  readonly coordinateGetter?: KeyboardCoordinateGetter;
  readonly custom?: CustomDrag;
  readonly toastSuccess?: ((message: string) => void) | undefined;
  readonly toastError?: ((message: string) => void) | undefined;
}

export interface GroupDnd<TItem, TGroup> {
  readonly sensors: ReturnType<typeof useSensors>;
  readonly activeId: UniqueIdentifier | null;
  readonly draggedItems: readonly TItem[];
  readonly draggedFrom: TGroup | null;
  readonly isDragging: boolean;
  readonly handleDragStart: (event: DragStartEvent) => void;
  readonly handleDragEnd: (event: DragEndEvent) => Promise<void>;
  readonly handleDragOver: () => void;
  readonly handleDragCancel: () => void;
  readonly getDropAreaState: (dropAreaId: string) => DropAreaState;
}
