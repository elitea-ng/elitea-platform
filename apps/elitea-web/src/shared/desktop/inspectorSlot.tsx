/**
 * The desktop frame's inspector: ONE right panel whose tabs the page on
 * screen contributes (Local work: Changes, Approvals, Plan, Processes, Index, Context; the chat:
 * Conversation). The panel stacks its tabs in one or two groups, and a tab can
 * open in a window of its own.
 *
 * The frame (`widgets/desktop-shell`) owns the panel and provides this
 * context; a page renders one `<InspectorTab>` per tab it offers. The tab's
 * content is portalled to wherever the frame shows it — its group's body, or
 * the body of its own window — so it stays in the page's React tree (its
 * queries, router, state and handlers): only its place on screen changes,
 * and it is live in a window exactly as it is in the panel. A tab the frame
 * does not show (another tab of its group is active, the panel is hidden)
 * renders nothing.
 *
 * Outside the desktop frame (the web build, a test without it) there is no
 * context: `useInspectorSlot()` is `null` and the page renders the content
 * its own way (the chat keeps its participants rail inline).
 */
import { createContext, useContext, useEffect, useMemo, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

import { ThemeProvider, type Theme } from '@mui/material/styles';

/** Every tab a page can contribute. Ids are unique across pages, so a tab id names its page too. */
export type InspectorTabId = 'changes' | 'approvals' | 'plan' | 'processes' | 'index' | 'context' | 'conversation';

/** The kind of page an inspector belongs to: the layout (groups, widths) is remembered per kind. */
export type InspectorPage = 'local' | 'chat';

export const INSPECTOR_TAB_PAGE: Readonly<Record<InspectorTabId, InspectorPage>> = {
  changes: 'local',
  approvals: 'local',
  plan: 'local',
  processes: 'local',
  index: 'local',
  context: 'local',
  conversation: 'chat',
};

/** What a page says about one of its tabs. */
export interface InspectorTabInfo {
  readonly id: InspectorTabId;
  /** The chip's label ("Changes"). */
  readonly title: string;
  /** What the tab shows (the thread or conversation name): its own window is titled "<title> — <subject>". */
  readonly subject: string;
  /** A count that needs the person (approvals waiting): shown on the chip. 0 or absent: none. */
  readonly attention?: number;
}

/** Where the frame shows a tab's content right now. */
export interface InspectorTarget {
  readonly node: HTMLElement;
  /** Set while the tab is in its own window: its popovers and menus open there, not in the main window. */
  readonly popout?: { readonly body: HTMLElement };
}

export interface InspectorSlot {
  /** A page offers a tab (and re-offers it when its info changes); the returned function withdraws it. */
  readonly register: (info: InspectorTabInfo) => () => void;
  /** Where each shown tab's content goes; a tab without an entry is not on screen. */
  readonly targets: ReadonlyMap<InspectorTabId, InspectorTarget>;
}

const InspectorSlotContext = createContext<InspectorSlot | null>(null);

export const InspectorSlotProvider = InspectorSlotContext.Provider;

/** The frame's inspector, or `null` outside the desktop frame. */
export function useInspectorSlot(): InspectorSlot | null {
  return useContext(InspectorSlotContext);
}

/**
 * MUI portals its popovers, menus and tooltips to the global `document.body`
 * — the main window's. Content shown in a tab's own window portals into that
 * window's body instead.
 */
function PopoutScope({ body, children }: { readonly body: HTMLElement | undefined; readonly children: ReactNode }): ReactNode {
  const theme = useMemo(
    () =>
      body === undefined
        ? (outer: Theme): Theme => outer
        : (outer: Theme): Theme => ({
            ...outer,
            components: {
              ...outer.components,
              MuiModal: { ...outer.components?.MuiModal, defaultProps: { ...outer.components?.MuiModal?.defaultProps, container: body } },
              MuiPopper: { ...outer.components?.MuiPopper, defaultProps: { ...outer.components?.MuiPopper?.defaultProps, container: body } },
            },
          }),
    [body],
  );
  return <ThemeProvider theme={theme}>{children}</ThemeProvider>;
}

/** One tab of the frame's inspector: `children` render wherever the frame shows the tab, and nowhere while it is not shown. */
export function InspectorTab({ id, title, subject, attention = 0, children }: InspectorTabInfo & { readonly children: ReactNode }): ReactNode {
  const slot = useInspectorSlot();
  const register = slot?.register;
  useEffect(() => register?.({ id, title, subject, attention }), [register, id, title, subject, attention]);
  const target = slot?.targets.get(id);
  if (target === undefined) return null;
  return createPortal(<PopoutScope body={target.popout?.body}>{children}</PopoutScope>, target.node, id);
}
