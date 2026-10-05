import type { ChangeEvent, KeyboardEvent, ReactNode } from 'react';
import { useCallback, useEffect, useRef, useState } from 'react';

import SearchIcon from '@mui/icons-material/Search';
import Box from '@mui/material/Box';
import MuiInputBase from '@mui/material/InputBase';
import type { SxProps, Theme } from '@mui/material/styles';

import { combineSx } from '../lib/combineSx';
import { t } from '@/shared/i18n';

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface SimpleSearchBarProps {
  value: string;
  /**
   * Fires `debounceMs` after the last keystroke (or immediately when
   * `debounceMs` is `0`) — see the class doc for why this is a real
   * debounce and not a pass-through, unlike the baseline.
   */
  onChange: (value: string) => void;
  /** `Escape` clears immediately, bypassing the debounce, and calls this too. */
  onClear?: () => void;
  placeholder?: string;
  /** @default 300 */
  debounceMs?: number;
  sx?: SxProps<Theme>;
  'data-testid'?: string;
  /** The accessible name of the input. The placeholder is not a label: it disappears as soon as the user types. */
  'aria-label'?: string;
}

const containerSx = (theme: Theme) => ({
  width: '100%',
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(1),
  backgroundColor: theme.vars.palette.background.userInputBackground,
  borderRadius: theme.vars.shape.radiusLg,
  border: `1px solid ${theme.vars.palette.border.lines}`,
  paddingBlock: theme.spacing(0.75),
  paddingInline: theme.spacing(1.5),
  transition: 'background-color 0.2s ease-in-out, border-color 0.2s ease-in-out',
  // #6646: one hover state for every search box in the app. The callers used
  // to bring their own (a MUI outlined TextField, a bare InputBase), so the
  // hover differed from page to page.
  '&:hover': {
    borderColor: theme.vars.palette.border.inputHover,
  },
  '&:focus-within': {
    borderColor: theme.vars.palette.border.flowNode,
    backgroundColor: theme.vars.palette.background.userInputBackgroundActive,
  },
});

const inputSx = (theme: Theme) => ({
  flex: 1,
  ...theme.typography.bodySmall,
  color: theme.vars.palette.text.secondary,
  '& input::placeholder': {
    color: theme.vars.palette.text.default,
    opacity: 1,
  },
});

/**
 * #6646: one icon size and one colour. The callers drew the glyph at 16px,
 * 18px and 20px, in three different colours; `fontSize="small"` alone
 * leaves the size to MUI's 20px default.
 */
const iconSx = (theme: Theme) => ({
  width: '1rem',
  height: '1rem',
  flexShrink: 0,
  color: theme.vars.palette.icon.fill.default,
});

/**
 * A pill-shaped search input with a leading search icon and `Escape`-to-clear.
 * Ported from `apps/elitea-ui/src/[fsd]/shared/ui/input/SimpleSearchBar.jsx`.
 *
 * Behavioural addition, not in the baseline: the baseline called
 * `onSearchChange` on every keystroke with no debounce at all — fine for an
 * in-memory list filter, but every real `shared/ui` caller of a search box
 * in this app filters through a network request (`shared/api`), and firing
 * one per keystroke is the kind of thing that shows up as a support ticket
 * about rate limits. `debounceMs` (default 300) delays the `onChange` call;
 * the input itself stays responsive (local `draft` state updates
 * synchronously) so typing never feels laggy. Passing `debounceMs={0}`
 * restores the baseline's every-keystroke behaviour exactly.
 *
 * Dropped from the baseline: `autoFocus` (default `true` in the baseline,
 * applied via a `setTimeout`). `jsx-a11y/no-autofocus` (R-C1, the same
 * fence `BaseModal`'s doc comment flags) bans the prop outright — an
 * unrequested focus jump is a known usability hazard for screen-reader and
 * low-vision users. A caller that owns a real reason to focus this field
 * (e.g. opening inside a command palette) can call `.focus()` on a ref
 * itself.
 */
export function SimpleSearchBar({
  value,
  onChange,
  onClear,
  placeholder,
  debounceMs = 300,
  sx,
  'data-testid': dataTestId,
  'aria-label': ariaLabel,
}: SimpleSearchBarProps): ReactNode {
  const [draft, setDraft] = useState(value);
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  // Keep the local draft in sync with an externally-changed `value` (e.g. a
  // caller resetting the query) — not with our own debounced writes, which
  // never go through this effect since they don't change the `value` prop
  // synchronously.
  useEffect(() => {
    setDraft(value);
  }, [value]);

  useEffect(() => {
    return () => {
      if (timeoutRef.current !== undefined) clearTimeout(timeoutRef.current);
    };
  }, []);

  const commit = useCallback(
    (next: string) => {
      onChange(next);
    },
    [onChange],
  );

  const scheduleCommit = useCallback(
    (next: string) => {
      if (timeoutRef.current !== undefined) {
        clearTimeout(timeoutRef.current);
        timeoutRef.current = undefined;
      }
      if (debounceMs <= 0) {
        commit(next);
        return;
      }
      timeoutRef.current = setTimeout(() => {
        timeoutRef.current = undefined;
        commit(next);
      }, debounceMs);
    },
    [commit, debounceMs],
  );

  const handleChange = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      const next = event.target.value;
      setDraft(next);
      scheduleCommit(next);
    },
    [scheduleCommit],
  );

  const handleClear = useCallback(() => {
    if (timeoutRef.current !== undefined) {
      clearTimeout(timeoutRef.current);
      timeoutRef.current = undefined;
    }
    setDraft('');
    commit('');
    onClear?.();
  }, [commit, onClear]);

  const handleKeyDown = useCallback(
    (event: KeyboardEvent<HTMLInputElement>) => {
      if (event.key === 'Escape') handleClear();
    },
    [handleClear],
  );

  return (
    <Box sx={combineSx(containerSx, sx)}>
      <SearchIcon
        aria-hidden
        data-testid="simple-search-bar-icon"
        sx={iconSx}
      />
      <MuiInputBase
        value={draft}
        onChange={handleChange}
        onKeyDown={handleKeyDown}
        placeholder={placeholder ?? t('shared.ui.simpleSearchBar.placeholder', 'Search...')}
        sx={inputSx}
        inputProps={{
          ...(dataTestId !== undefined ? { 'data-testid': dataTestId } : {}),
          ...(ariaLabel !== undefined ? { 'aria-label': ariaLabel } : {}),
        }}
      />
    </Box>
  );
}
