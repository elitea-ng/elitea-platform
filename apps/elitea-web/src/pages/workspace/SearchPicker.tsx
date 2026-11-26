/**
 * A sidebar button that opens a searchable list (#3081): "Add agent", "Add cloud agent", "Add team" and "Change" in a
 * Local work thread's Participants tab. Typing filters the rows by name; a row that cannot be picked is disabled and
 * says why.
 */
import { useMemo, useRef, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import ListItemText from '@mui/material/ListItemText';
import MenuItem from '@mui/material/MenuItem';
import MenuList from '@mui/material/MenuList';
import Popover from '@mui/material/Popover';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

export interface PickerOption {
  readonly id: string;
  readonly label: string;
  /** Why it cannot be picked; absent = it can. */
  readonly unavailable?: string;
}

interface SearchPickerProps {
  readonly label: string;
  readonly testId: string;
  readonly options: readonly PickerOption[];
  readonly onPick: (id: string) => void;
  readonly icon?: ReactNode;
  readonly disabled?: boolean;
  /** Under the list ("Starts a new thread…"). */
  readonly note?: string;
  readonly variant?: 'outlined' | 'text';
}

/** The options whose label holds `query` (any case); all of them for a blank query. */
export function filterOptions(options: readonly PickerOption[], query: string): readonly PickerOption[] {
  const needle = query.trim().toLowerCase();
  return needle === '' ? options : options.filter((option) => option.label.toLowerCase().includes(needle));
}

export function SearchPicker({ label, testId, options, onPick, icon, disabled = false, note, variant = 'outlined' }: SearchPickerProps): React.JSX.Element {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [query, setQuery] = useState('');
  const search = useRef<HTMLInputElement | null>(null);
  const shown = useMemo(() => filterOptions(options, query), [options, query]);
  const close = (): void => {
    setAnchor(null);
    setQuery('');
  };
  return (
    <>
      <Button
        size="small"
        variant={variant}
        startIcon={icon}
        disabled={disabled || options.length === 0}
        aria-haspopup="listbox"
        aria-expanded={anchor !== null}
        data-testid={testId}
        onClick={(event) => setAnchor(event.currentTarget)}
      >
        {label}
      </Button>
      <Popover
        open={anchor !== null}
        anchorEl={anchor}
        onClose={close}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
        // Typing searches at once: the field takes the focus the popover would.
        slotProps={{ transition: { onEntered: () => search.current?.focus() } }}
      >
        <Box sx={{ display: 'flex', flexDirection: 'column', width: '16rem', padding: 1, gap: 0.5 }} data-testid={`${testId}-picker`}>
          <TextField
            size="small"
            inputRef={search}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder={t('workspace.participants.search', 'Search')}
            slotProps={{ htmlInput: { 'aria-label': label } }}
          />
          <MenuList dense sx={{ maxHeight: '16rem', overflowY: 'auto' }} aria-label={label}>
            {shown.map((option) => (
              <MenuItem
                key={option.id}
                disabled={option.unavailable !== undefined}
                data-testid={`${testId}-option-${option.id}`}
                onClick={() => {
                  close();
                  onPick(option.id);
                }}
              >
                <ListItemText primary={option.label} secondary={option.unavailable} />
              </MenuItem>
            ))}
            {shown.length === 0 && (
              <MenuItem disabled>{t('workspace.participants.noMatch', 'No match')}</MenuItem>
            )}
          </MenuList>
          {note !== undefined && (
            <Typography variant="bodySmall" color="text.secondary" sx={{ paddingX: 1 }}>
              {note}
            </Typography>
          )}
        </Box>
      </Popover>
    </>
  );
}
