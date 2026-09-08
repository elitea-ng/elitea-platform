/**
 * Ported verbatim from `apps/elitea-ui/src/common/eventEmitter.js` (22
 * lines) — a plain `{event: listener[]}` pub/sub bus, used by
 * `ToolkitsOperationButtons.tsx`/`ToolkitForm.tsx` to coordinate the
 * Save/Validate event flow (`entities/toolkit`'s `ToolEvents` catalogue)
 * across the two components without prop-drilling every handler through.
 *
 * PROMOTED to `shared/lib` (it used to live in `features/toolkits/lib/`).
 * The bus has two ends by design: the form component that LISTENS
 * (`features/toolkits`' `ToolkitsOperationButtons`) and the screen header
 * that EMITS. In the baseline both ends sit inside the same slice; here the
 * emitting end is the toolkit edit PAGE, and `no-deep-slice-import` (R-L3)
 * lets `pages/` reach a slice only through its `index.ts` — whose 20-symbol
 * budget is full. `shared/` is beneath every layer and needs no barrel slot,
 * so this is the one home both ends can legally reach. Nothing about the bus
 * itself changed.
 */
type Listener = (data: unknown) => void;

const listenersByEvent: Record<string, Listener[]> = {};

export const eventEmitter = {
  on(event: string, listener: Listener): void {
    (listenersByEvent[event] ??= []).push(listener);
  },

  emit(event: string, data?: unknown): void {
    for (const listener of listenersByEvent[event] ?? []) listener(data);
  },

  off(event: string, listener: Listener): void {
    const listeners = listenersByEvent[event];
    if (!listeners) return;
    listenersByEvent[event] = listeners.filter((candidate) => candidate !== listener);
  },
};
