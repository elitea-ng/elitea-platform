/**
 * One tab chip of an inspector group (`InspectorGroup.tsx`): a `tab` (its
 * title, the count of what waits for the person, a mark while the tab is in
 * its own window) and, beside it, its × — outside the tab, so the tablist
 * holds only tabs. A pointer drags the chip to the other group.
 */
import type { KeyboardEvent, MouseEvent } from 'react';

import { useDraggable } from '@dnd-kit/core';

import CloseIcon from '@mui/icons-material/Close';
import OpenInNewIcon from '@mui/icons-material/OpenInNew';
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

import type { InspectorTabId, InspectorTabInfo } from '@/shared/desktop/inspectorSlot';
import { t } from '@/shared/i18n';

export function TabChip({
  tab,
  info,
  selected,
  popped,
  panelId,
  chipId,
  onSelect,
  onClose,
  onMenu,
  onKeyDown,
  register,
}: {
  readonly tab: InspectorTabId;
  readonly info: InspectorTabInfo | undefined;
  readonly selected: boolean;
  readonly popped: boolean;
  readonly panelId: string;
  readonly chipId: string;
  readonly onSelect: () => void;
  readonly onClose: () => void;
  readonly onMenu: (event: MouseEvent<HTMLElement>) => void;
  readonly onKeyDown: (event: KeyboardEvent<HTMLElement>) => void;
  readonly register: (node: HTMLElement | null) => void;
}): React.JSX.Element {
  const { listeners, setNodeRef, isDragging } = useDraggable({ id: tab });
  const title = info?.title ?? tab;
  const attention = info?.attention ?? 0;
  const name = [
    title,
    attention > 0 ? t('desktop.inspector.attention', '{{n}} waiting', { n: attention }) : null,
    popped ? t('desktop.inspector.inWindow', 'in its own window') : null,
  ]
    .filter((part) => part !== null)
    .join(', ');
  return (
    <Box
      role="presentation"
      ref={setNodeRef}
      data-testid={`inspector-chip-${tab}`}
      sx={(theme: Theme) => ({
        display: 'inline-flex',
        alignItems: 'center',
        flexShrink: 0,
        height: '1.5rem',
        borderRadius: '6px',
        opacity: isDragging ? 0.4 : 1,
        color: selected ? theme.vars.palette.text.primary : theme.vars.palette.text.secondary,
        background: selected ? `color-mix(in srgb, ${theme.vars.palette.text.primary} 9%, transparent)` : 'transparent',
        '&:hover': { background: `color-mix(in srgb, ${theme.vars.palette.text.primary} ${selected ? '12%' : '6%'}, transparent)` },
        '&:hover .inspector-chip-close, & .inspector-chip-close:focus-visible': { opacity: 1 },
      })}
    >
      <Box
        component="button"
        type="button"
        role="tab"
        id={chipId}
        ref={register}
        aria-selected={selected}
        aria-controls={selected ? panelId : undefined}
        aria-label={name}
        tabIndex={selected ? 0 : -1}
        onClick={onSelect}
        onContextMenu={onMenu}
        onKeyDown={onKeyDown}
        {...listeners}
        sx={(theme: Theme) => ({
          all: 'unset',
          display: 'inline-flex',
          alignItems: 'center',
          gap: '0.25rem',
          height: '100%',
          paddingLeft: '0.5rem',
          paddingRight: '0.125rem',
          borderRadius: '6px',
          cursor: 'default',
          font: 'inherit',
          fontSize: '0.75rem',
          fontWeight: selected ? 600 : 500,
          whiteSpace: 'nowrap',
          '&:focus-visible': { outline: `2px solid ${theme.vars.palette.primary.main}`, outlineOffset: '-2px' },
        })}
      >
        {title}
        {attention > 0 && (
          <Box
            component="span"
            aria-hidden
            data-testid={`inspector-chip-attention-${tab}`}
            sx={(theme: Theme) => ({
              minWidth: '1rem',
              height: '1rem',
              paddingX: '0.25rem',
              boxSizing: 'border-box',
              borderRadius: '0.5rem',
              display: 'inline-flex',
              alignItems: 'center',
              justifyContent: 'center',
              fontSize: '0.625rem',
              fontWeight: 600,
              color: theme.vars.palette.primary.contrastText,
              background: theme.vars.palette.primary.main,
            })}
          >
            {attention}
          </Box>
        )}
        {popped && <OpenInNewIcon aria-hidden data-testid={`inspector-chip-window-${tab}`} sx={{ fontSize: '0.75rem' }} />}
      </Box>
      <Box
        component="button"
        type="button"
        className="inspector-chip-close"
        tabIndex={-1}
        aria-label={t('desktop.inspector.closeTab', 'Close {{tab}}', { tab: title })}
        onClick={onClose}
        sx={(theme: Theme) => ({
          all: 'unset',
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'center',
          width: '1rem',
          height: '1rem',
          marginRight: '0.25rem',
          borderRadius: '4px',
          opacity: selected ? 0.7 : 0,
          cursor: 'default',
          '&:hover': { background: `color-mix(in srgb, ${theme.vars.palette.text.primary} 12%, transparent)`, opacity: 1 },
        })}
      >
        <CloseIcon sx={{ fontSize: '0.75rem' }} />
      </Box>
    </Box>
  );
}
