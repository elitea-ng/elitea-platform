/**
 * One group of the inspector (`InspectorPane.tsx`): a tool-window header —
 * the group's tab chips, then ⋯ (every tab the page offers, and what can be
 * done with the active one), Open in new window, Maximize, and (on the top
 * group) Hide inspector — over the active tab's body.
 *
 * The chips are a tablist: ←/→ (Home/End) move between them, Delete closes
 * one, Shift+F10 or the context-menu key opens its menu (Move to the other
 * group, Open in new window, Close), as a right-click does. A chip can be
 * dragged to the other group, or below a lone group to split the panel.
 * A tab open in its own window keeps its chip here, marked; a click on it
 * brings the window to the front.
 */
import { useCallback, useId, useRef, useState, type KeyboardEvent, type MouseEvent, type ReactNode } from 'react';

import { useDroppable } from '@dnd-kit/core';

import CheckIcon from '@mui/icons-material/Check';
import CloseFullscreenIcon from '@mui/icons-material/CloseFullscreen';
import CloseIcon from '@mui/icons-material/Close';
import MoreHorizIcon from '@mui/icons-material/MoreHoriz';
import OpenInFullIcon from '@mui/icons-material/OpenInFull';
import OpenInNewIcon from '@mui/icons-material/OpenInNew';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Menu from '@mui/material/Menu';
import MenuItem from '@mui/material/MenuItem';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import type { InspectorTabId, InspectorTabInfo } from '@/shared/desktop/inspectorSlot';
import { t } from '@/shared/i18n';
import { MenuDivider, MenuRow, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

import type { InspectorGroupView } from '../lib/inspectorLayout';
import { useDesktopLayout } from '../model/desktopLayout.store';
import { TabChip } from './InspectorTabChip';
import { modKey } from './shortcutLabel';
import { TitleBarSpacer } from './TitleBarSpacer';

export const groupDropId = (index: number): string => `inspector-group-${String(index)}`;

export interface InspectorGroupProps {
  readonly view: InspectorGroupView;
  /** The tabs of the page, by id (titles, attention). */
  readonly infos: ReadonlyMap<InspectorTabId, InspectorTabInfo>;
  /** Every tab the page offers, in order: the ⋯ menu lists them. */
  readonly offered: readonly InspectorTabInfo[];
  /** Tabs on screen in either group (the ⋯ menu ticks them). */
  readonly visible: ReadonlySet<InspectorTabId>;
  /** The group this one's tabs move to: the other one, or (a lone top group) the bottom one it splits off. */
  readonly other: 0 | 1;
  /** The top group draws the window's title bar row (the drag region) and the Hide inspector button. */
  readonly top: boolean;
  readonly popped: ReadonlySet<InspectorTabId>;
  readonly onPopOut: (tab: InspectorTabId) => void;
  readonly onFocusWindow: (tab: InspectorTabId) => void;
  readonly onDock: (tab: InspectorTabId) => void;
  /** The active tab's body element (its content portals there), `null` when it unmounts. */
  readonly bodyRef: (tab: InspectorTabId, node: HTMLElement | null) => void;
}

const titleOf = (infos: ReadonlyMap<InspectorTabId, InspectorTabInfo>, tab: InspectorTabId): string => infos.get(tab)?.title ?? tab;

function moveLabel(to: 0 | 1, title: string): string {
  return to === 0 ? t('desktop.inspector.moveTop', 'Move {{tab}} to top group', { tab: title }) : t('desktop.inspector.moveBottom', 'Move {{tab}} to bottom group', { tab: title });
}

interface TabMenuState {
  readonly tab: InspectorTabId;
  readonly anchor: { readonly el: HTMLElement } | { readonly top: number; readonly left: number };
}

function GroupActions({ top, activePopped, onMore, onPopOut }: { readonly top: boolean; readonly activePopped: boolean; readonly onMore: (anchor: HTMLElement) => void; readonly onPopOut: () => void }): React.JSX.Element {
  const maximized = useDesktopLayout((state) => state.inspectorMaximized);
  const layout = useDesktopLayout.getState;
  const hide = t('desktop.inspector.hide', 'Hide inspector');
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', flexShrink: 0 }}>
      <HeaderButton label={t('desktop.inspector.more', 'Inspector options')} onClick={(event) => onMore(event.currentTarget)} testId="inspector-more">
        <MoreHorizIcon fontSize="inherit" />
      </HeaderButton>
      <HeaderButton label={activePopped ? t('desktop.inspector.focusWindow', 'Show its window') : t('desktop.inspector.openWindow', 'Open in new window')} onClick={onPopOut} testId="inspector-pop-out">
        <OpenInNewIcon fontSize="inherit" />
      </HeaderButton>
      <HeaderButton
        label={maximized ? t('desktop.inspector.restore', 'Restore (Esc)') : t('desktop.inspector.maximize', 'Maximize')}
        onClick={() => layout().setInspectorMaximized(!maximized)}
        testId="inspector-maximize"
      >
        {maximized ? <CloseFullscreenIcon fontSize="inherit" /> : <OpenInFullIcon fontSize="inherit" />}
      </HeaderButton>
      {top && (
        <HeaderButton label={hide} hint={`${hide} (${modKey()}⌥\\)`} onClick={() => layout().setChangesOpen(false)} testId="inspector-hide">
          <CloseIcon fontSize="inherit" />
        </HeaderButton>
      )}
    </Box>
  );
}

/** The body of a group whose active tab is in its own window. */
function InWindowNotice({ id, labelledBy, title, onFocus, onDock }: { readonly id: string; readonly labelledBy: string; readonly title: string; readonly onFocus: () => void; readonly onDock: () => void }): React.JSX.Element {
  return (
    <Box id={id} role="tabpanel" aria-labelledby={labelledBy} data-testid="inspector-in-window" sx={{ flex: 1, display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center', gap: 1, padding: 2, textAlign: 'center' }}>
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {t('desktop.inspector.openElsewhere', '{{tab}} is open in its own window.', { tab: title })}
      </Typography>
      <Box sx={{ display: 'flex', gap: 1 }}>
        <Button size="small" variant="text" onClick={onFocus}>
          {t('desktop.inspector.focusWindow', 'Show its window')}
        </Button>
        <Button size="small" variant="text" onClick={onDock}>
          {t('desktop.inspector.dockShort', 'Return to panel')}
        </Button>
      </Box>
    </Box>
  );
}

/** Where a tab's menu opens: at its chip (the keyboard) or at the pointer (a right-click). */
function menuAnchorProps(anchor: TabMenuState['anchor'] | undefined): { anchorEl: HTMLElement } | { anchorReference: 'anchorPosition'; anchorPosition: { top: number; left: number } } {
  if (anchor !== undefined && 'el' in anchor) return { anchorEl: anchor.el };
  return { anchorReference: 'anchorPosition', anchorPosition: anchor === undefined ? { top: 0, left: 0 } : { top: anchor.top, left: anchor.left } };
}

/** The active tab's body: the element its content portals into. */
function TabBody({ tab, id, labelledBy, testId, bodyRef }: { readonly tab: InspectorTabId; readonly id: string; readonly labelledBy: string; readonly testId: string; readonly bodyRef: InspectorGroupProps['bodyRef'] }): React.JSX.Element {
  const ref = useCallback((node: HTMLElement | null) => bodyRef(tab, node), [bodyRef, tab]);
  return <Box id={id} role="tabpanel" aria-labelledby={labelledBy} data-testid={testId} ref={ref} sx={{ flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' }} />;
}

function HeaderButton({ label, hint, onClick, children, testId }: { readonly label: string; readonly hint?: string; readonly onClick: (event: MouseEvent<HTMLButtonElement>) => void; readonly children: ReactNode; readonly testId?: string }): React.JSX.Element {
  return (
    <Tooltip title={hint ?? label}>
      <IconButton size="small" aria-label={label} onClick={onClick} data-testid={testId}>
        {children}
      </IconButton>
    </Tooltip>
  );
}

export function InspectorGroup({ view, infos, offered, visible, other, top, popped, onPopOut, onFocusWindow, onDock, bodyRef }: InspectorGroupProps): React.JSX.Element {
  const layout = useDesktopLayout.getState;
  const { setNodeRef: dropRef, isOver } = useDroppable({ id: groupDropId(view.index) });
  const baseId = useId();
  const chipId = (tab: InspectorTabId): string => `${baseId}-tab-${tab}`;
  const panelId = `${baseId}-panel`;
  const chips = useRef(new Map<InspectorTabId, HTMLElement>());
  const [tabMenu, setTabMenu] = useState<TabMenuState | null>(null);
  const [moreAnchor, setMoreAnchor] = useState<HTMLElement | null>(null);
  const active = view.active;
  const activeTitle = titleOf(infos, active);

  const select = (tab: InspectorTabId): void => {
    if (popped.has(tab)) onFocusWindow(tab);
    layout().activateInspectorTab(tab);
  };
  const closeTab = (tab: InspectorTabId): void => {
    if (popped.has(tab)) onDock(tab);
    layout().closeInspectorTab(tab);
  };
  const popOut = (tab: InspectorTabId): void => (popped.has(tab) ? onFocusWindow(tab) : onPopOut(tab));

  const onTabKeyDown = (tab: InspectorTabId) => (event: KeyboardEvent<HTMLElement>) => {
    const at = view.tabs.indexOf(tab);
    const go = (index: number): void => {
      const next = view.tabs[(index + view.tabs.length) % view.tabs.length];
      if (next === undefined) return;
      event.preventDefault();
      layout().activateInspectorTab(next);
      chips.current.get(next)?.focus();
    };
    if (event.key === 'ArrowRight') go(at + 1);
    else if (event.key === 'ArrowLeft') go(at - 1);
    else if (event.key === 'Home') go(0);
    else if (event.key === 'End') go(view.tabs.length - 1);
    else if (event.key === 'Delete') {
      event.preventDefault();
      closeTab(tab);
    } else if (event.key === 'ContextMenu' || (event.key === 'F10' && event.shiftKey)) {
      event.preventDefault();
      setTabMenu({ tab, anchor: { el: event.currentTarget } });
    }
  };

  const tabActions = (tab: InspectorTabId): { id: string; label: string; run: () => void }[] => [
    { id: 'move', label: moveLabel(other, titleOf(infos, tab)), run: () => layout().moveInspectorTab(tab, other) },
    popped.has(tab)
      ? { id: 'dock', label: t('desktop.inspector.dock', 'Return {{tab}} to the panel', { tab: titleOf(infos, tab) }), run: () => onDock(tab) }
      : { id: 'window', label: t('desktop.inspector.openWindowNamed', 'Open {{tab}} in new window', { tab: titleOf(infos, tab) }), run: () => onPopOut(tab) },
    { id: 'close', label: t('desktop.inspector.closeTab', 'Close {{tab}}', { tab: titleOf(infos, tab) }), run: () => closeTab(tab) },
  ];

  const trailing = <GroupActions top={top} activePopped={popped.has(active)} onMore={setMoreAnchor} onPopOut={() => popOut(active)} />;
  const tablist = (
    <Box
      role="tablist"
      aria-label={top ? t('desktop.inspector.tabsTop', 'Inspector tabs') : t('desktop.inspector.tabsBottom', 'Inspector tabs, bottom group')}
      data-testid={`inspector-tabs-${String(view.index)}`}
      sx={{ display: 'flex', alignItems: 'center', gap: '0.125rem', minWidth: 0, overflowX: 'auto', scrollbarWidth: 'none' }}
    >
      {view.tabs.map((tab) => (
        <TabChip
          key={tab}
          tab={tab}
          info={infos.get(tab)}
          selected={tab === active}
          popped={popped.has(tab)}
          panelId={panelId}
          chipId={chipId(tab)}
          onSelect={() => select(tab)}
          onClose={() => closeTab(tab)}
          onMenu={(event) => {
            event.preventDefault();
            setTabMenu({ tab, anchor: { top: event.clientY, left: event.clientX } });
          }}
          onKeyDown={onTabKeyDown(tab)}
          register={(node) => {
            if (node === null) chips.current.delete(tab);
            else chips.current.set(tab, node);
          }}
        />
      ))}
    </Box>
  );

  return (
    <Box
      ref={dropRef}
      data-testid={`inspector-group-${String(view.index)}`}
      sx={(theme: Theme) => ({
        flex: 1,
        minHeight: 0,
        display: 'flex',
        flexDirection: 'column',
        outline: isOver ? `2px solid ${theme.vars.palette.primary.main}` : 'none',
        outlineOffset: '-2px',
      })}
    >
      <Box sx={(theme: Theme) => ({ borderBottom: `1px solid ${theme.vars.palette.divider}`, flexShrink: 0 })}>
        {top ? (
          <TitleBarSpacer logo={false} trailing={trailing}>
            {tablist}
          </TitleBarSpacer>
        ) : (
          <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, minHeight: '2.25rem', paddingLeft: 1, paddingRight: 1 }}>
            <Box sx={{ flex: 1, minWidth: 0 }}>{tablist}</Box>
            {trailing}
          </Box>
        )}
      </Box>
      {popped.has(active) ? (
        <InWindowNotice id={panelId} labelledBy={chipId(active)} title={activeTitle} onFocus={() => onFocusWindow(active)} onDock={() => onDock(active)} />
      ) : (
        <TabBody key={active} tab={active} id={panelId} labelledBy={chipId(active)} testId={`inspector-body-${String(view.index)}`} bodyRef={bodyRef} />
      )}

      <Menu
        open={tabMenu !== null}
        onClose={() => setTabMenu(null)}
        {...menuAnchorProps(tabMenu?.anchor)}
      >
        {tabMenu !== null &&
          tabActions(tabMenu.tab).map((action) => (
            <MenuItem
              key={action.id}
              onClick={() => {
                setTabMenu(null);
                action.run();
              }}
            >
              {action.label}
            </MenuItem>
          ))}
      </Menu>

      <Menu
        anchorEl={moreAnchor}
        open={moreAnchor !== null}
        onClose={() => setMoreAnchor(null)}
        slotProps={{ list: { dense: true, 'aria-label': t('desktop.inspector.more', 'Inspector options') }, paper: { sx: { minWidth: '13rem' } } }}
      >
        {offered.map((info) => (
          <MenuRow
            key={info.id}
            role="menuitemcheckbox"
            aria-checked={visible.has(info.id)}
            data-testid={`inspector-more-${info.id}`}
            onClick={() => {
              setMoreAnchor(null);
              layout().showInspector(info.id);
            }}
          >
            <MenuRowIcon>{visible.has(info.id) && <CheckIcon />}</MenuRowIcon>
            <MenuRowText>{info.title}</MenuRowText>
          </MenuRow>
        ))}
        <MenuDivider />
        {tabActions(active)
          .filter((action) => action.id !== 'close')
          .map((action) => (
            <MenuRow
              key={action.id}
              onClick={() => {
                setMoreAnchor(null);
                action.run();
              }}
            >
              <MenuRowIcon />
              <MenuRowText>{action.label}</MenuRowText>
            </MenuRow>
          ))}
      </Menu>
    </Box>
  );
}
