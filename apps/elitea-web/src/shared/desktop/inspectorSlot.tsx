/**
 * The desktop frame's inspector: a full-height pane at the window's right
 * edge that a page fills with its own content (the chat's participants,
 * agents and context budget).
 *
 * The frame (`widgets/desktop-shell`) owns the pane and provides this
 * context; a page renders `<InspectorPortal>` with what belongs there. The
 * portal keeps the page's React tree (its queries, router and state), so the
 * content behaves as it does inline — only its place on screen changes. A
 * mounted portal also tells the frame that the page has an inspector, so the
 * frame shows the toggle in the title bar.
 *
 * Outside the desktop frame (the web build, a test without it) there is no
 * context: `useInspectorSlot()` is `null` and the page renders the content
 * inline, as before.
 */
import { createContext, useContext, useEffect, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

/** Which page's inspector it is: the frame names the pane after it (its strings are desktop-only). */
export type InspectorKind = 'chat';

export interface InspectorSlot {
  /** The pane's body, while the pane is open; `null` while it is hidden. */
  readonly target: HTMLElement | null;
  /** A page announces its inspector; the returned function withdraws it. */
  readonly register: (kind: InspectorKind) => () => void;
}

const InspectorSlotContext = createContext<InspectorSlot | null>(null);

export const InspectorSlotProvider = InspectorSlotContext.Provider;

/** The frame's inspector, or `null` outside the desktop frame. */
export function useInspectorSlot(): InspectorSlot | null {
  return useContext(InspectorSlotContext);
}

/** Puts `children` in the frame's inspector pane (nothing while the pane is hidden). */
export function InspectorPortal({ kind, children }: { readonly kind: InspectorKind; readonly children: ReactNode }): ReactNode {
  const slot = useInspectorSlot();
  const register = slot?.register;
  useEffect(() => register?.(kind), [register, kind]);
  if (slot === null || slot.target === null) return null;
  return createPortal(children, slot.target);
}
