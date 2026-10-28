/**
 * A menu row that opens its submenu beside it, as a native menu does: on
 * hover after a short pause, on a click, or with the arrow toward the end side
 * (Return and Space too); the arrow back or Esc closes it and returns to the
 * row. The submenu stays open while the pointer travels from the row into it
 * (a grace period), and closes when the pointer settles on another row.
 *
 * It is a non-modal popper rendered inside the parent menu's DOM, so the
 * parent's focus trap keeps working and a click in the submenu does not count
 * as a click away from the parent. It flips to the start side, and slides
 * along the edge, when the window has no room at the end side.
 */
import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import MenuList from '@mui/material/MenuList';
import Paper from '@mui/material/Paper';
import Popper from '@mui/material/Popper';
import { useTheme, type Theme } from '@mui/material/styles';

import { MenuRow, MenuRowHint, MenuRowIcon, MenuRowText } from './nativeMenu';

/** How long the pointer rests on the row before the submenu opens (macOS waits about this long). */
const OPEN_DELAY_MS = 120;
/** How long the submenu waits for the pointer to arrive after it left the row. */
const CLOSE_DELAY_MS = 260;
/** Popper's overflow boundary: the whole window (`document.documentElement` clips nothing a fixed popper draws). */
const WINDOW = { boundary: typeof document === 'undefined' ? undefined : document.documentElement, rootBoundary: 'viewport' } as const;

export interface NativeSubmenuProps {
  readonly label: string;
  readonly icon?: ReactNode;
  /** Quiet text before the arrow: the current choice. */
  readonly hint?: string;
  readonly testId?: string;
  readonly disabled?: boolean;
  /** The submenu's rows; `close` closes the submenu (the caller closes its own menu too when a row acts). */
  readonly children: (close: () => void) => ReactNode;
  readonly minWidth?: string;
}

function useTimer(): { start: (ms: number, run: () => void) => void; stop: () => void } {
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  return {
    start: (ms, run) => {
      clearTimeout(timer.current);
      timer.current = setTimeout(run, ms);
    },
    stop: () => clearTimeout(timer.current),
  };
}

export function NativeSubmenu({ label, icon, hint, testId, disabled, children, minWidth = '12rem' }: NativeSubmenuProps): React.JSX.Element {
  const [row, setRow] = useState<HTMLLIElement | null>(null);
  const [open, setOpen] = useState(false);
  // Opened from the keyboard: its first row takes the focus.
  const [focusFirst, setFocusFirst] = useState(false);
  const timer = useTimer();
  const rtl = useTheme().direction === 'rtl';
  const forward = rtl ? 'ArrowLeft' : 'ArrowRight';
  const back = rtl ? 'ArrowRight' : 'ArrowLeft';
  const close = (): void => {
    timer.stop();
    setOpen(false);
    setFocusFirst(false);
  };
  const closeToRow = (): void => {
    close();
    row?.focus();
  };
  const openNow = (fromKeyboard: boolean): void => {
    timer.stop();
    setFocusFirst(fromKeyboard);
    setOpen(true);
  };
  const onRowKey = (event: KeyboardEvent): void => {
    if (event.key === forward || event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      openNow(true);
    }
  };
  const onListKey = (event: KeyboardEvent): void => {
    // The submenu's list moved its own focus already; the parent menu, whose DOM it sits in, must not move or close.
    event.stopPropagation();
    if (event.key === back || event.key === 'Escape') {
      event.preventDefault();
      closeToRow();
    }
  };
  return (
    <>
      <MenuRow
        ref={setRow}
        data-testid={testId}
        disabled={disabled ?? false}
        aria-haspopup="menu"
        aria-expanded={open}
        selected={open}
        onMouseEnter={() => timer.start(OPEN_DELAY_MS, () => openNow(false))}
        onMouseLeave={() => timer.start(CLOSE_DELAY_MS, close)}
        onClick={() => openNow(false)}
        onKeyDown={onRowKey}
      >
        {icon === undefined ? <MenuRowIcon /> : <MenuRowIcon>{icon}</MenuRowIcon>}
        <MenuRowText>{label}</MenuRowText>
        {hint !== undefined && <MenuRowHint>{hint}</MenuRowHint>}
        <MenuRowIcon>
          <ChevronRightIcon sx={{ transform: rtl ? 'scaleX(-1)' : undefined }} />
        </MenuRowIcon>
      </MenuRow>
      <Popper
        open={open && row !== null}
        anchorEl={row}
        placement={rtl ? 'left-start' : 'right-start'}
        disablePortal
        popperOptions={{ strategy: 'fixed' }}
        modifiers={[
          { name: 'offset', options: { offset: [-4, 4] } },
          // Bounded by the window, not the default clipping parents: the parent menu's own scrolling paper is one,
          // and kept inside it the submenu was pushed back on top of the menu it belongs to.
          { name: 'flip', options: { fallbackPlacements: [rtl ? 'right-start' : 'left-start'], padding: 8, ...WINDOW } },
          { name: 'preventOverflow', options: { padding: 8, altAxis: true, ...WINDOW } },
        ]}
        sx={{ zIndex: (theme: Theme) => theme.zIndex.modal + 1 }}
      >
        <Paper
          elevation={8}
          onMouseEnter={timer.stop}
          onMouseLeave={() => timer.start(CLOSE_DELAY_MS, close)}
          sx={(theme: Theme) => ({ borderRadius: theme.vars.shape.radiusMd, minWidth, maxHeight: 'calc(100vh - 16px)', overflowY: 'auto' })}
        >
          <MenuList dense autoFocusItem={focusFirst} aria-label={label} onKeyDown={onListKey} sx={{ padding: 0.5 }}>
            {children(close)}
          </MenuList>
        </Paper>
      </Popper>
    </>
  );
}
