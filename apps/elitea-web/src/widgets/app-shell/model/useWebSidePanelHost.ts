/**
 * The web shell's side of `shared/desktop/inspectorSlot`: the right side
 * panel the page on screen fills with tabs (a list's Filters, the chat's
 * Conversation), the browser's counterpart of the desktop frame's inspector.
 *
 * Simpler than the inspector on purpose: one group, no windows, no split. The
 * panel shows the active tab of the tabs offered; the newest registration of a
 * tab wins, as in the frame.
 *
 * Open or closed is remembered per viewer AND per page (`el.sidePanel.open.<first
 * tab the page offers>`: a list's List tab, the chat's Conversation), so closing
 * it on the agents list leaves the chat's panel alone; a choice made before it
 * was per page (`el.sidePanel.open`) still answers for a page with none of its
 * own. With nothing remembered it is open on a wide window (≥1440px: at 1280px the 248px rail
 * and the 320px panel left the page 712px, too narrow for a list's toolbar,
 * whose search folded and whose primary action was cut off). Below 1024px the panel is
 * a temporary drawer over the page, opened only on request and never
 * remembered — a narrow window has no room to keep it beside the content.
 */
import { useCallback, useMemo, useState } from 'react';

import useMediaQuery from '@mui/material/useMediaQuery';

import type { InspectorTabId, InspectorTabInfo, InspectorTarget, SidePanelSlot } from '@/shared/desktop/inspectorSlot';
import { useRegistrationStack } from '@/shared/lib/registrationStack';
import { createStorage } from '@/shared/lib/storage';

const OPEN_KEY = 'sidePanel.open';

/** The remembered choice of one page; the key it was kept under before it was per page answers when the page has none. */
const pageKey = (page: string): string => `${OPEN_KEY}.${page}`;

/** A drawer over the page below this width. */
const SIDE_PANEL_NARROW_QUERY = '(max-width: 1023.95px)';
/** Open by default from this width, when the viewer never chose. */
const SIDE_PANEL_WIDE_QUERY = '(min-width: 1440px)';

function readStoredOpen(page: string): boolean | null {
  try {
    const storage = createStorage('local');
    const raw = (page === '' ? null : storage.get(pageKey(page))) ?? storage.get(OPEN_KEY);
    return raw === 'true' ? true : raw === 'false' ? false : null;
  } catch {
    return null;
  }
}

function writeStoredOpen(page: string, open: boolean): void {
  try {
    createStorage('local').set(page === '' ? OPEN_KEY : pageKey(page), String(open));
  } catch {
    // Not remembered.
  }
}

export interface WebSidePanelHost {
  /** What the pages see (`useSidePanelSlot()`). */
  readonly slot: SidePanelSlot;
  /** The tabs offered now, in the order they were first offered. */
  readonly tabs: readonly InspectorTabInfo[];
  /** The tab on screen; `null` when nothing is offered. */
  readonly active: InspectorTabId | null;
  /** The panel is on screen (open, and something is offered). */
  readonly shown: boolean;
  /** Below 1024px: the panel is a temporary drawer. */
  readonly narrow: boolean;
  readonly activate: (tab: InspectorTabId) => void;
  readonly setOpen: (open: boolean) => void;
  readonly setBody: (node: HTMLElement | null) => void;
}

export function useWebSidePanelHost(): WebSidePanelHost {
  const narrow = useMediaQuery(SIDE_PANEL_NARROW_QUERY);
  const wide = useMediaQuery(SIDE_PANEL_WIDE_QUERY);
  const [choices, setChoices] = useState<ReadonlyMap<string, boolean>>(new Map());
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [stack, registrations] = useRegistrationStack<InspectorTabInfo>();
  const [chosen, setChosen] = useState<InspectorTabId | null>(null);
  const [body, setBody] = useState<HTMLElement | null>(null);

  const register = stack.register;

  const tabs = useMemo(() => {
    const byId = new Map<InspectorTabId, InspectorTabInfo>();
    registrations.forEach((info) => byId.set(info.id, info));
    return Array.from(byId.values());
  }, [registrations]);

  const active = tabs.find((info) => info.id === chosen)?.id ?? tabs[0]?.id ?? null;
  // The page is named by the first tab it offers, which a tab switch inside the panel does not change.
  const page = tabs[0]?.id ?? '';
  const storedOpen = choices.get(page) ?? readStoredOpen(page);
  const open = narrow ? drawerOpen : (storedOpen ?? wide);
  const shown = open && active !== null;

  const setOpen = useCallback(
    (next: boolean) => {
      if (narrow) {
        setDrawerOpen(next);
        return;
      }
      setChoices((current) => new Map(current).set(page, next));
      writeStoredOpen(page, next);
    },
    [narrow, page],
  );

  const show = useCallback(
    (tab: InspectorTabId) => {
      setChosen(tab);
      setOpen(true);
    },
    [setOpen],
  );

  const targets = useMemo(() => {
    const map = new Map<InspectorTabId, InspectorTarget>();
    if (shown && body !== null && active !== null) map.set(active, { node: body });
    return map;
  }, [shown, body, active]);

  const slot = useMemo<SidePanelSlot>(() => ({ register, targets, show, overlay: narrow }), [register, targets, show, narrow]);
  return { slot, tabs, active, shown, narrow, activate: setChosen, setOpen, setBody };
}
