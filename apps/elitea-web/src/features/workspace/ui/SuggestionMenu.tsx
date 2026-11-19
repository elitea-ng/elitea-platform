/**
 * The composer's "@" file and "/" command popup: the chat input's mention
 * popup look (`shared/ui/MentionToolList`'s frame, `MentionToolItem` rows)
 * with a plain heading, and a sub-heading where a row starts a new section
 * (the "/" menu's skills after its commands). Keyboard handling stays with the composer (the
 * textarea keeps focus); this only shows the list and the highlighted row.
 */
import type { ReactNode } from 'react';
import { Fragment, useEffect, useRef } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ClickAwayListener from '@mui/material/ClickAwayListener';
import Typography from '@mui/material/Typography';

import { MentionToolItem } from '@/shared/ui/MentionToolItem';

export interface SuggestionItem {
  key: string;
  label: string;
  description?: string;
  icon?: ReactNode;
  /** A heading shown above this row when it differs from the previous row's (the first section goes under the menu's title). */
  section?: string;
  /** Small labels after the name (a skill's "Pinned"). */
  badges?: readonly { readonly label: string }[];
  /** A button beside the row that acts on it without picking it (a skill's "Pin to this thread"). */
  action?: { readonly label: string; readonly onClick: () => void };
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

  // Keep the highlighted row visible by scrolling the LIST only: scrollIntoView
  // would also scroll every ancestor, the window's frame included.
  useEffect(() => {
    const list = listRef.current;
    const active = list?.querySelector('[data-highlighted="true"]');
    if (list === null || list === undefined || !(active instanceof HTMLElement)) return;
    const row = active.getBoundingClientRect();
    const box = list.getBoundingClientRect();
    if (row.top < box.top) list.scrollTop -= box.top - row.top;
    else if (row.bottom > box.bottom) list.scrollTop += row.bottom - box.bottom;
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
          <Fragment key={item.key}>
            {index > 0 && item.section !== undefined && item.section !== items[index - 1]?.section && (
              // oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- a heading inside the listbox; a native <optgroup> needs a <select>
              <Box role="presentation" sx={{ padding: '0.5rem 0.75rem 0.25rem' }}>
                <Typography variant="subtitle" color="text.secondary">
                  {item.section}
                </Typography>
              </Box>
            )}
            {/* The row's own button bubbles its click here, so the option element itself is clickable too. */}
            <Box sx={{ display: 'flex', alignItems: 'stretch', gap: '0.25rem' }}>
              {/* oxlint-disable-next-line jsx-a11y/prefer-tag-over-role -- rows of the listbox above; a native <option> needs a <select> */}
              <Box role="option" aria-selected={index === activeIndex} aria-label={item.label} onClick={() => onPick(index)} sx={{ flex: 1, minWidth: 0, display: 'flex' }}>
                <MentionToolItem
                  label={item.label}
                  {...(item.description !== undefined ? { description: item.description } : {})}
                  {...(item.icon !== undefined ? { icon: item.icon } : {})}
                  {...(item.badges !== undefined ? { badges: item.badges } : {})}
                  isHighlighted={index === activeIndex}
                />
              </Box>
              {item.action !== undefined && (
                <Button size="small" variant="text" onClick={item.action.onClick} sx={{ flexShrink: 0 }}>
                  {item.action.label}
                </Button>
              )}
            </Box>
          </Fragment>
        ))}
      </Box>
    </ClickAwayListener>
  );
}
