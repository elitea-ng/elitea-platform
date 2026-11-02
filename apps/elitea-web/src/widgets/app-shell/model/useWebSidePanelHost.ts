/**
 * The web shell's side of `shared/desktop/inspectorSlot`: the right side
 * panel the page on screen fills with tabs (a list's Filters, the chat's
 * Conversation), the browser's counterpart of the desktop frame's inspector.
 *
 * Simpler than the inspector on purpose: one group, no windows, no split. The
 * panel shows the active tab of the tabs offered; the newest registration of a
 * tab wins, as in the frame.
 *
 * Open or closed is remembered per viewer (`el.sidePanel.open`); with nothing
 * remembered it is open on a wide window (≥1440px: at 1280px the 248px rail
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

/** A drawer over the page below this width. */
const SIDE_PANEL_NARROW_QUERY = '(max-width: 1023.95px)';
/** Open by default from this width, when the viewer never chose. */
const SIDE_PANEL_WIDE_QUERY = '(min-width: 1440px)';

function readStoredOpen(): boolean | null {
  try {
    const raw = createStorage('local').get(OPEN_KEY);
    return raw === 'true' ? true : raw === 'false' ? false : null;
  } catch {
    return null;
  }
}

function writeStoredOpen(open: boolean): void {
  try {
    createStorage('local').set(OPEN_KEY, String(open));
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
  const [storedOpen, setStoredOpen] = useState<boolean | null>(readStoredOpen);
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
  const open = narrow ? drawerOpen : (storedOpen ?? wide);
  const shown = open && active !== null;

  const setOpen = useCallback(
    (next: boolean) => {
      if (narrow) {
        setDrawerOpen(next);
        return;
      }
      setStoredOpen(next);
      writeStoredOpen(next);
    },
    [narrow],
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

  const slot = useMemo<SidePanelSlot>(() => ({ register, targets, show }), [register, targets, show]);
  return { slot, tabs, active, shown, narrow, activate: setChosen, setOpen, setBody };
}
