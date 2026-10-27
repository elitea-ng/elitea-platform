/**
 * The desktop composer's resize handle: its top edge, dragged up or down,
 * sets how many lines the input shows before it scrolls (the input still
 * grows past that as the text does). The arrow keys move it a line at a
 * time, Home/End to the ends, and a double click goes back to the default.
 * Remembered per viewer (`el.desktop.composerLines`), so the chat and a
 * Local work thread open at the height last chosen in either: one composer.
 *
 * It replaces the web's expand/collapse button, which in a desktop window
 * read as a stray control in the text.
 */
import { useCallback, useRef, useState, type PointerEvent as ReactPointerEvent } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { createStorage } from '@/shared/lib/storage';

/** One line of the native composer's text (px), the step the handle moves in. */
const COMPOSER_LINE_PX = 20;
const COMPOSER_LINES = { min: 1, default: 2, max: 24 } as const;

const LINES_KEY = 'desktop.composerLines';

function clampLines(lines: number): number {
  return Math.min(COMPOSER_LINES.max, Math.max(COMPOSER_LINES.min, Math.round(lines)));
}

function validate(raw: unknown): number {
  if (typeof raw !== 'number' || !Number.isFinite(raw)) throw new Error('invalid');
  return clampLines(raw);
}

function readLines(): number {
  try {
    return createStorage('local').getJSON(LINES_KEY, validate) ?? COMPOSER_LINES.default;
  } catch {
    return COMPOSER_LINES.default;
  }
}

/** The lines the input shows, and a setter that remembers them. */
function useComposerLines(): readonly [number, (lines: number) => void] {
  const [lines, setLines] = useState(readLines);
  const set = useCallback((next: number) => {
    const value = clampLines(next);
    setLines(value);
    try {
      createStorage('local').setJSON(LINES_KEY, value);
    } catch {
      // Storage refused: the height lasts until the window closes.
    }
  }, []);
  return [lines, set] as const;
}

const KEY_STEPS: Record<string, (lines: number) => number> = {
  ArrowUp: (lines) => lines + 1,
  ArrowDown: (lines) => lines - 1,
  Home: () => COMPOSER_LINES.min,
  End: () => COMPOSER_LINES.max,
};

function ComposerResizeHandle({ lines, onLines }: { readonly lines: number; readonly onLines: (lines: number) => void }): React.JSX.Element {
  const drag = useRef<{ y: number; lines: number } | null>(null);
  const onPointerDown = (event: ReactPointerEvent<HTMLDivElement>): void => {
    event.preventDefault();
    event.currentTarget.setPointerCapture(event.pointerId);
    drag.current = { y: event.clientY, lines };
  };
  const onPointerMove = (event: ReactPointerEvent<HTMLDivElement>): void => {
    const start = drag.current;
    // Up makes the input taller: the composer sits at the bottom of the window.
    if (start !== null) onLines(start.lines + (start.y - event.clientY) / COMPOSER_LINE_PX);
  };
  const onPointerUp = (): void => {
    drag.current = null;
  };
  return (
    <Box
      // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- a focusable splitter (a value, arrow keys); an <hr> takes no focus or children's events
      role="separator"
      tabIndex={0}
      aria-orientation="horizontal"
      aria-label={t('chatInput.resize', 'Resize the message box')}
      aria-valuemin={COMPOSER_LINES.min}
      aria-valuemax={COMPOSER_LINES.max}
      aria-valuenow={lines}
      data-testid="composer-resize-handle"
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={onPointerUp}
      onPointerCancel={onPointerUp}
      onDoubleClick={() => onLines(COMPOSER_LINES.default)}
      onKeyDown={(event) => {
        const step = KEY_STEPS[event.key];
        if (step === undefined) return;
        event.preventDefault();
        onLines(step(lines));
      }}
      sx={(theme: Theme) => ({
        position: 'absolute',
        top: -4,
        left: 16,
        right: 16,
        height: 9,
        zIndex: 2,
        cursor: 'ns-resize',
        touchAction: 'none',
        outline: 'none',
        // A grip that shows on hover or focus, the way a split view's divider does.
        '&::after': {
          content: '""',
          position: 'absolute',
          top: 3,
          left: '50%',
          width: 32,
          height: 3,
          transform: 'translateX(-50%)',
          borderRadius: theme.vars.shape.radiusPill,
          background: theme.vars.palette.text.metrics,
          opacity: 0,
          transition: 'opacity 120ms',
        },
        '&:hover::after, &:focus-visible::after': { opacity: 0.5 },
      })}
    />
  );
}

/** The desktop composer's handle and the lines it sets; on the web, neither (the expand button stays). */
export function useComposerResize(native: boolean): { readonly handle: React.JSX.Element | null; readonly minRows: number | undefined } {
  const [lines, setLines] = useComposerLines();
  return native ? { handle: <ComposerResizeHandle lines={lines} onLines={setLines} />, minRows: lines } : { handle: null, minRows: undefined };
}
