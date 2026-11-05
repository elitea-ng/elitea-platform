/**
 * The desktop frame's inspector: ONE right panel whose tabs the page on
 * screen contributes (Local work: Changes, Approvals, Plan, Processes, Index, Context, Browser; the chat:
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
 * Outside the desktop frame there is no inspector. The web shell has a side panel of its own (`widgets/app-shell`'s
 * `WebSidePanel`) with the same contract, so a page that only needs "a right
 * panel, wherever this build keeps it" — the list pages' Filters, the chat's
 * Conversation — reads `useSidePanelSlot()` instead: the desktop inspector or
 * the web side panel, `null` only where neither exists (a unit test).
 * `<InspectorTab>` registers there.
 */
import { createContext, useContext, useEffect, useMemo, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

import { ThemeProvider, type Theme } from '@mui/material/styles';

/** Every tab a page can contribute. Ids are unique across pages, so a tab id names its page too. */
export type InspectorTabId = 'changes' | 'approvals' | 'plan' | 'processes' | 'index' | 'context' | 'browser' | 'conversation' | 'filters';

/** The kind of page an inspector belongs to: the layout (groups, widths) is remembered per kind. `browse`: the list pages (their Filters). */
export type InspectorPage = 'local' | 'chat' | 'browse';

export const INSPECTOR_TAB_PAGE: Readonly<Record<InspectorTabId, InspectorPage>> = {
  changes: 'local',
  approvals: 'local',
  plan: 'local',
  processes: 'local',
  index: 'local',
  context: 'local',
  browser: 'local',
  conversation: 'chat',
  filters: 'browse',
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

/** The right panel of whichever shell is on screen: the desktop inspector or the web side panel. */
export interface SidePanelSlot extends InspectorSlot {
  /** Opens the panel on `tab` (the toolbar's Filters button). */
  readonly show: (tab: InspectorTabId) => void;
}

const SidePanelSlotContext = createContext<SidePanelSlot | null>(null);

export const SidePanelSlotProvider = SidePanelSlotContext.Provider;

/** The shell's right panel, or `null` where there is none (a page mounted alone in a test). */
export function useSidePanelSlot(): SidePanelSlot | null {
  return useContext(SidePanelSlotContext);
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

/** One tab of the shell's right panel: `children` render wherever the shell shows the tab, and nowhere while it is not shown. */
export function InspectorTab({ id, title, subject, attention = 0, children }: InspectorTabInfo & { readonly children: ReactNode }): ReactNode {
  const slot = useSidePanelSlot();
  const register = slot?.register;
  useEffect(() => register?.({ id, title, subject, attention }), [register, id, title, subject, attention]);
  const target = slot?.targets.get(id);
  if (target === undefined) return null;
  return createPortal(<PopoutScope body={target.popout?.body}>{children}</PopoutScope>, target.node, id);
}
