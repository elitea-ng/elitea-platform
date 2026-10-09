/**
 * The composer's "@" file and "/" command popup: the chat input's mention
 * popup look (`shared/ui/MentionToolList`'s frame, `MentionToolItem` rows)
 * with a plain heading. Keyboard handling stays with the composer (the
 * textarea keeps focus); this only shows the list and the highlighted row.
 */
import type { ReactNode } from 'react';
import { useEffect, useRef } from 'react';

import Box from '@mui/material/Box';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import Typography from '@mui/material/Typography';

import { MentionToolItem } from '@/shared/ui/MentionToolItem';

export interface SuggestionItem {
  key: string;
  label: string;
  description?: string;
  icon?: ReactNode;
}

export interface SuggestionMenuProps {
  title: string;
  items: readonly SuggestionItem[];
  activeIndex: number;
  onPick: (index: number) => void;
  onClose: () => void;
  'data-testid'?: string;
}

export function SuggestionMenu({ title, items, activeIndex, onPick, onClose, 'data-testid': testId }: SuggestionMenuProps): ReactNode {
  const listRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const active = listRef.current?.querySelector('[data-highlighted="true"]');
    if (active instanceof HTMLElement && typeof active.scrollIntoView === 'function') active.scrollIntoView({ block: 'nearest' });
  }, [activeIndex]);

  return (
    <ClickAwayListener onClickAway={onClose}>
      <Box
        ref={listRef}
        // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- a popup the textarea drives (combobox pattern); a native <select> cannot be
        role="listbox"
        aria-label={title}
        data-testid={testId}
        sx={(theme) => ({
          border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          width: '100%',
          maxHeight: '15.4375rem',
          borderRadius: theme.vars.shape.radiusLg,
          boxSizing: 'border-box',
          padding: '0.75rem',
          display: 'flex',
          flexDirection: 'column',
          gap: '0.25rem',
          background: theme.vars.palette.background.secondary,
          overflowY: 'auto',
        })}
      >
        <Box sx={{ display: 'flex', alignItems: 'center', padding: '0 0.75rem 0.25rem' }}>
          <Typography variant="subtitle" color="text.primary">
            {title}
          </Typography>
        </Box>
        {items.map((item, index) => (
          // The row's own button bubbles its click here, so the option element itself is clickable too.
          // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- rows of the listbox above; a native <option> needs a <select>
          <Box key={item.key} role="option" aria-selected={index === activeIndex} aria-label={item.label} onClick={() => onPick(index)}>
            <MentionToolItem
              label={item.label}
              {...(item.description !== undefined ? { description: item.description } : {})}
              {...(item.icon !== undefined ? { icon: item.icon } : {})}
              isHighlighted={index === activeIndex}
            />
          </Box>
        ))}
      </Box>
    </ClickAwayListener>
  );
}
