/**
 * The frame's inspector (`shared/desktop/inspectorSlot`): ONE full-height
 * panel at the window's right edge, on the sidebar's own surface, whose tabs
 * the page on screen contributes — Local work's Changes, Approvals and Index,
 * the chat's Conversation. Tabs only appear on the pages they belong to.
 *
 * Like an IDE's tool windows, the panel stacks its tabs in one or two groups
 * (a splitter between them); a tab moves between groups by dragging its chip
 * or from its menu, and a group left without tabs collapses. A tab can open
 * in a native window of its own (`model/usePanelWindows.ts`), live, and come
 * back when that window closes. Maximize lets the panel take the main
 * column's place until Esc.
 *
 * One toggle (`changesOpen`: the title bar's button, ⌘⌥\, remembered) shows
 * and hides the panel on every page; the layout — groups, active tabs, the
 * split and the width — is remembered per kind of page.
 */
import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

import { DndContext, DragOverlay, PointerSensor, useSensor, useSensors, type DragEndEvent, type DragStartEvent } from '@dnd-kit/core';

import VerticalSplitOutlinedIcon from '@mui/icons-material/VerticalSplitOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import type { AppIpc } from '@/shared/desktop/appEvents';
import {
  INSPECTOR_TAB_PAGE,
  InspectorSlotProvider,
  type InspectorPage,
  type InspectorSlot,
  type InspectorTabId,
  type InspectorTabInfo,
  type InspectorTarget,
} from '@/shared/desktop/inspectorSlot';
import { t } from '@/shared/i18n';

import { viewOf, type InspectorGroupView } from '../lib/inspectorLayout';
import { SIDEBAR_TINT_OPACITY } from '../lib/sidebarTint';
import { useAppIpc } from '../model/appIpcContext';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { usePanelWindows, type PanelWindows } from '../model/usePanelWindows';
import { groupDropId, InspectorGroup } from './InspectorGroup';
import { SPLIT_DROP_ID, SplitDropZone, SplitHandle, WidthHandle } from './InspectorHandles';
import { modKey } from './shortcutLabel';

export interface InspectorHost {
  readonly slot: InspectorSlot;
  /** The kind of page whose tabs are offered now; `null` when the page on screen has none. */
  readonly page: InspectorPage | null;
  /** The tabs that page offers, in the order it offered them. */
  readonly tabs: readonly InspectorTabInfo[];
  readonly windows: PanelWindows;
  readonly setBody: (tab: InspectorTabId, node: HTMLElement | null) => void;
  readonly setWindowBody: (tab: InspectorTabId, node: HTMLElement | null) => void;
}

let nextRegistration = 0;

function withEntry<K, V>(map: ReadonlyMap<K, V>, key: K, value: V | null): ReadonlyMap<K, V> {
  if (value === null ? !map.has(key) : map.get(key) === value) return map;
  const next = new Map(map);
  if (value === null) next.delete(key);
  else next.set(key, value);
  return next;
}

/** The frame's side of the slot: the tabs offered, where each shown tab renders, and the tabs' windows. */
export function useInspectorHost(appIpc: AppIpc | undefined): InspectorHost {
  const [registrations, setRegistrations] = useState<readonly { readonly key: number; readonly info: InspectorTabInfo }[]>([]);
  const [bodies, setBodies] = useState<ReadonlyMap<InspectorTabId, HTMLElement>>(new Map());
  const [windowBodies, setWindowBodies] = useState<ReadonlyMap<InspectorTabId, HTMLElement>>(new Map());

  const register = useCallback((info: InspectorTabInfo) => {
    nextRegistration += 1;
    const key = nextRegistration;
    setRegistrations((current) => [...current, { key, info }]);
    return () => setRegistrations((current) => current.filter((entry) => entry.key !== key));
  }, []);

  // The newest registration of each tab wins; the page is the newest tab's.
  const { page, tabs } = useMemo(() => {
    const newest = registrations.at(-1);
    if (newest === undefined) return { page: null, tabs: [] };
    const kind = INSPECTOR_TAB_PAGE[newest.info.id];
    const byId = new Map<InspectorTabId, InspectorTabInfo>();
    registrations.forEach(({ info }) => {
      if (INSPECTOR_TAB_PAGE[info.id] === kind) byId.set(info.id, info);
    });
    return { page: kind, tabs: Array.from(byId.values()) };
  }, [registrations]);

  const windows = usePanelWindows(tabs, appIpc);
  const setBody = useCallback((tab: InspectorTabId, node: HTMLElement | null) => setBodies((current) => withEntry(current, tab, node)), []);
  const setWindowBody = useCallback((tab: InspectorTabId, node: HTMLElement | null) => setWindowBodies((current) => withEntry(current, tab, node)), []);

  const targets = useMemo(() => {
    const map = new Map<InspectorTabId, InspectorTarget>();
    bodies.forEach((node, tab) => {
      if (!windows.roots.has(tab)) map.set(tab, { node });
    });
    windowBodies.forEach((node, tab) => {
      const root = windows.roots.get(tab);
      if (root !== undefined) map.set(tab, { node, popout: { body: root.body } });
    });
    return map;
  }, [bodies, windowBodies, windows.roots]);

  const slot = useMemo(() => ({ register, targets }), [register, targets]);
  return { slot, page, tabs, windows, setBody, setWindowBody };
}

export function InspectorSlotScope({ host, children }: { readonly host: InspectorHost; readonly children: ReactNode }): React.JSX.Element {
  return <InspectorSlotProvider value={host.slot}>{children}</InspectorSlotProvider>;
}

/** The groups the panel shows now (none: the page has no tabs, or every one is closed). */
export function useInspectorView(host: InspectorHost): readonly InspectorGroupView[] {
  const layout = useDesktopLayout((state) => (host.page === null ? null : state.inspector[host.page]));
  return useMemo(() => (layout === null ? [] : viewOf(layout, host.tabs.map((info) => info.id))), [layout, host.tabs]);
}

/** The title bar's toggle: shown while the page has an inspector and it is hidden. */
export function InspectorToggle({ host }: { readonly host: InspectorHost }): React.JSX.Element | null {
  const open = useDesktopLayout((state) => state.changesOpen);
  if (host.page === null || open) return null;
  const label = t('desktop.inspector.show', 'Show inspector');
  const attention = host.tabs.reduce((sum, info) => sum + (info.attention ?? 0), 0);
  return (
    <Tooltip title={`${label} (${modKey()}⌥\\)`}>
      <IconButton size="small" aria-label={label} aria-pressed={false} data-testid="inspector-toggle" data-attention={attention > 0 ? attention : undefined} onClick={() => useDesktopLayout.getState().setChangesOpen(true)}>
        <VerticalSplitOutlinedIcon fontSize="inherit" />
      </IconButton>
    </Tooltip>
  );
}

/** A tab in its own window: the window's whole page, on the app's surface. */
function WindowBody({ host, tab, root }: { readonly host: InspectorHost; readonly tab: InspectorTabId; readonly root: HTMLElement }): React.ReactPortal {
  const setWindowBody = host.setWindowBody;
  const ref = useCallback((node: HTMLElement | null) => setWindowBody(tab, node), [setWindowBody, tab]);
  return createPortal(
    <Box
      ref={ref}
      data-testid={`inspector-window-${tab}`}
      sx={(theme: Theme) => ({
        position: 'fixed',
        inset: 0,
        display: 'flex',
        flexDirection: 'column',
        overflow: 'hidden',
        background: theme.vars.palette.background.secondary,
        color: theme.vars.palette.text.primary,
        fontFamily: theme.typography.fontFamily,
      })}
    />,
    root,
  );
}

/** The tabs' windows: rendered whether or not the panel is open. */
export function InspectorWindows({ host }: { readonly host: InspectorHost }): React.JSX.Element {
  return (
    <>
      {Array.from(host.windows.roots, ([tab, root]) => (
        <WindowBody key={`${tab}:${root.root.ownerDocument.URL}`} host={host} tab={tab} root={root.root} />
      ))}
    </>
  );
}

export function InspectorPane({ host }: { readonly host: InspectorHost }): React.JSX.Element | null {
  const open = useDesktopLayout((state) => state.changesOpen);
  const vibrancy = useDesktopLayout((state) => state.platform.vibrancy);
  const maximized = useDesktopLayout((state) => state.inspectorMaximized);
  const poppedTabs = useDesktopLayout((state) => state.poppedTabs);
  const layout = useDesktopLayout((state) => (host.page === null ? null : state.inspector[host.page]));
  const groups = useInspectorView(host);
  const [column, setColumn] = useState<HTMLElement | null>(null);
  const [dragged, setDragged] = useState<InspectorTabId | null>(null);
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));
  const shown = open && host.page !== null && layout !== null && groups.length > 0;

  // Esc restores a maximized panel (unless something inside it used the key).
  useEffect(() => {
    if (!shown || !maximized) return undefined;
    const onKey = (event: globalThis.KeyboardEvent): void => {
      if (event.key === 'Escape' && !event.defaultPrevented) useDesktopLayout.getState().setInspectorMaximized(false);
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [shown, maximized]);

  const infos = useMemo(() => new Map(host.tabs.map((info) => [info.id, info])), [host.tabs]);
  const visible = useMemo(() => new Set(groups.flatMap((group) => group.tabs)), [groups]);
  const popped = useMemo(() => new Set(poppedTabs), [poppedTabs]);
  if (!shown) return null;
  const page = host.page;

  // A tab moves to the other group; a lone top group splits off a bottom one.
  const otherOf = (index: number): 0 | 1 => (index === 0 ? 1 : 0);
  const onDragStart = (event: DragStartEvent): void => setDragged(event.active.id as InspectorTabId);
  const onDragEnd = (event: DragEndEvent): void => {
    setDragged(null);
    const tab = event.active.id as InspectorTabId;
    const over = event.over?.id;
    if (over === SPLIT_DROP_ID) useDesktopLayout.getState().moveInspectorTab(tab, otherOf(groups[0]?.index ?? 0));
    else if (over === groupDropId(0)) useDesktopLayout.getState().moveInspectorTab(tab, 0);
    else if (over === groupDropId(1)) useDesktopLayout.getState().moveInspectorTab(tab, 1);
  };
  const split = layout.split;

  return (
    <Box
      component="aside"
      data-chrome
      data-testid="desktop-inspector"
      data-maximized={maximized ? '' : undefined}
      aria-label={t('desktop.inspector.label', 'Inspector')}
      sx={(theme: Theme) => ({
        ...(maximized ? { flex: 1, minWidth: 0 } : { width: `${String(layout.width / 16)}rem`, flexShrink: 0 }),
        height: '100vh',
        display: 'flex',
        flexDirection: 'column',
        boxSizing: 'border-box',
        borderLeft: `1px solid ${theme.vars.palette.divider}`,
        position: 'relative',
        isolation: 'isolate',
        // The sidebar's surface (`DesktopSidebar.tsx`): the secondary background, tinted over the window's material.
        '&::before': {
          content: '""',
          position: 'absolute',
          inset: 0,
          zIndex: -1,
          background: theme.vars.palette.background.secondary,
          opacity: vibrancy ? SIDEBAR_TINT_OPACITY : 1,
        },
      })}
    >
      {!maximized && <WidthHandle page={page} width={layout.width} />}
      <DndContext sensors={sensors} onDragStart={onDragStart} onDragEnd={onDragEnd} onDragCancel={() => setDragged(null)}>
        <Box ref={setColumn} sx={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', position: 'relative' }}>
          {groups.map((group, position) => (
            <Box key={group.index} sx={{ display: 'contents' }}>
              {position === 1 && <SplitHandle page={page} split={split} container={column} />}
              <Box data-testid="inspector-group-slot" sx={{ flex: groups.length > 1 ? `${String(position === 0 ? split : 1 - split)} 1 0` : '1 1 0', minHeight: 0, display: 'flex', flexDirection: 'column' }}>
                <InspectorGroup
                  view={group}
                  infos={infos}
                  offered={host.tabs}
                  visible={visible}
                  other={otherOf(group.index)}
                  top={position === 0}
                  popped={popped}
                  onPopOut={host.windows.open}
                  onFocusWindow={host.windows.focus}
                  onDock={host.windows.close}
                  bodyRef={host.setBody}
                />
              </Box>
            </Box>
          ))}
          {dragged !== null && groups.length === 1 && <SplitDropZone />}
        </Box>
        <DragOverlay dropAnimation={null}>
          {dragged === null ? null : (
            <Box
              sx={(theme: Theme) => ({
                display: 'inline-flex',
                alignItems: 'center',
                height: '1.5rem',
                paddingX: 1,
                borderRadius: '6px',
                fontSize: '0.75rem',
                fontWeight: 600,
                color: theme.vars.palette.text.primary,
                background: theme.vars.palette.background.paper,
                boxShadow: theme.vars.shadows[4],
              })}
            >
              {infos.get(dragged)?.title ?? dragged}
            </Box>
          )}
        </DragOverlay>
      </DndContext>
    </Box>
  );
}

/**
 * The frame's inspector without the rest of the frame: a page mounted on its
 * own (its tests) gets the same panel, the same tabs and windows, beside it.
 */
export function InspectorScope({ children }: { readonly children: ReactNode }): React.JSX.Element {
  const host = useInspectorHost(useAppIpc());
  return (
    <InspectorSlotScope host={host}>
      {children}
      <InspectorPane host={host} />
      <InspectorWindows host={host} />
    </InspectorSlotScope>
  );
}
