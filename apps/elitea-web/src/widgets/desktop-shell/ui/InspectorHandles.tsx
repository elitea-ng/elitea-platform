/**
 * The inspector's handles (`InspectorPane.tsx`): its left edge (the width),
 * the splitter between its two groups (their share of the height) — both
 * WAI-ARIA window splitters, dragged or moved with the arrow keys — and the
 * drop zone that splits a lone group while a chip is dragged.
 */
import { useState, type KeyboardEvent, type PointerEvent } from 'react';

import { useDroppable } from '@dnd-kit/core';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { InspectorPage } from '@/shared/desktop/inspectorSlot';
import { t } from '@/shared/i18n';

import { INSPECTOR_WIDTH, SPLIT_RANGE } from '../lib/inspectorLayout';
import { useDesktopLayout } from '../model/desktopLayout.store';

export const SPLIT_DROP_ID = 'inspector-split';

/** The panel's left edge: drag (or ←/→) to resize. */
export function WidthHandle({ page, width }: { readonly page: InspectorPage; readonly width: number }): React.JSX.Element {
  const [drag, setDrag] = useState<{ x: number; width: number } | null>(null);
  const setWidth = (next: number): void => useDesktopLayout.getState().setInspectorWidth(page, next);
  return (
    <Box
      // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- a focusable, draggable splitter (WAI-ARIA window splitter); an <hr> takes neither
      role="separator"
      aria-orientation="vertical"
      aria-label={t('desktop.inspector.resize', 'Resize inspector')}
      aria-valuemin={INSPECTOR_WIDTH.min}
      aria-valuemax={INSPECTOR_WIDTH.max}
      aria-valuenow={width}
      tabIndex={0}
      data-testid="inspector-width-handle"
      onPointerDown={(event: PointerEvent<HTMLDivElement>) => {
        event.currentTarget.setPointerCapture(event.pointerId);
        setDrag({ x: event.clientX, width });
      }}
      // The panel is on the right: dragging left widens it.
      onPointerMove={(event: PointerEvent<HTMLDivElement>) => drag !== null && setWidth(drag.width + (drag.x - event.clientX))}
      onPointerUp={() => setDrag(null)}
      onKeyDown={(event: KeyboardEvent<HTMLDivElement>) => {
        if (event.key === 'ArrowLeft') setWidth(width + 16);
        else if (event.key === 'ArrowRight') setWidth(width - 16);
      }}
      sx={(theme: Theme) => ({
        position: 'absolute',
        left: '-0.1875rem',
        top: 0,
        bottom: 0,
        width: '0.375rem',
        cursor: 'col-resize',
        zIndex: 2,
        '&:hover, &:focus-visible': { boxShadow: `inset 2px 0 0 ${theme.vars.palette.primary.main}`, outline: 'none' },
      })}
    />
  );
}

/** Between the two groups: drag (or ↑/↓) to share the height. */
export function SplitHandle({ page, split, container }: { readonly page: InspectorPage; readonly split: number; readonly container: HTMLElement | null }): React.JSX.Element {
  const [dragging, setDragging] = useState(false);
  const setSplit = (next: number): void => useDesktopLayout.getState().setInspectorSplit(page, next);
  return (
    <Box
      // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- a focusable, draggable splitter (WAI-ARIA window splitter); an <hr> takes neither
      role="separator"
      aria-orientation="horizontal"
      aria-label={t('desktop.inspector.resizeGroups', 'Resize inspector groups')}
      aria-valuemin={Math.round(SPLIT_RANGE.min * 100)}
      aria-valuemax={Math.round(SPLIT_RANGE.max * 100)}
      aria-valuenow={Math.round(split * 100)}
      tabIndex={0}
      data-testid="inspector-split-handle"
      onPointerDown={(event: PointerEvent<HTMLDivElement>) => {
        event.currentTarget.setPointerCapture(event.pointerId);
        setDragging(true);
      }}
      onPointerMove={(event: PointerEvent<HTMLDivElement>) => {
        if (!dragging || container === null) return;
        const rect = container.getBoundingClientRect();
        if (rect.height > 0) setSplit((event.clientY - rect.top) / rect.height);
      }}
      onPointerUp={() => setDragging(false)}
      onKeyDown={(event: KeyboardEvent<HTMLDivElement>) => {
        if (event.key === 'ArrowUp') setSplit(split - 0.05);
        else if (event.key === 'ArrowDown') setSplit(split + 0.05);
      }}
      sx={(theme: Theme) => ({
        flexShrink: 0,
        height: '0.3125rem',
        marginY: '-0.125rem',
        position: 'relative',
        zIndex: 1,
        cursor: 'row-resize',
        borderTop: `1px solid ${theme.vars.palette.divider}`,
        '&:hover, &:focus-visible': { borderTopColor: theme.vars.palette.primary.main, outline: 'none' },
      })}
    />
  );
}

/** While a chip is dragged over a lone group: the lower part of the panel, to split it. */
export function SplitDropZone(): React.JSX.Element {
  const { setNodeRef, isOver } = useDroppable({ id: SPLIT_DROP_ID });
  return (
    <Box
      ref={setNodeRef}
      data-testid="inspector-split-drop"
      sx={(theme: Theme) => ({
        position: 'absolute',
        left: 0,
        right: 0,
        bottom: 0,
        height: '40%',
        zIndex: 3,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        border: `1px dashed ${isOver ? theme.vars.palette.primary.main : theme.vars.palette.divider}`,
        background: `color-mix(in srgb, ${theme.vars.palette.primary.main} ${isOver ? '14%' : '6%'}, transparent)`,
      })}
    >
      <Typography variant="labelSmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
        {t('desktop.inspector.dropToSplit', 'Drop to show it below')}
      </Typography>
    </Box>
  );
}
