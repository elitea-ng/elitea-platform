/**
 * A native-style segmented control (Act | Plan, Changes | Approvals | Index):
 * one bezel, the chosen segment raised. A radio group for assistive
 * technology: arrow keys move the choice.
 */
import { useId, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';

export interface SegmentedOption<T extends string> {
  value: T;
  label: ReactNode;
  /** The accessible name when `label` is not text. */
  ariaLabel?: string;
}

export interface SegmentedProps<T extends string> {
  value: T;
  options: readonly SegmentedOption<T>[];
  onChange: (value: T) => void;
  ariaLabel: string;
  /** Segments share the width equally (an inspector header). */
  fill?: boolean;
  testId?: string;
}

export function Segmented<T extends string>({ value, options, onChange, ariaLabel, fill = false, testId }: SegmentedProps<T>): React.JSX.Element {
  const name = useId();
  return (
    <Box
      role="radiogroup"
      aria-label={ariaLabel}
      data-testid={testId}
      sx={(theme: Theme) => ({
        display: fill ? 'flex' : 'inline-flex',
        padding: '2px',
        gap: '2px',
        borderRadius: '7px',
        background: `color-mix(in srgb, ${theme.vars.palette.text.primary} 7%, transparent)`,
      })}
    >
      {options.map((option) => {
        const selected = option.value === value;
        return (
          <Box
            key={option.value}
            component="label"
            sx={(theme: Theme) => ({
              position: 'relative',
              flex: fill ? 1 : undefined,
              display: 'inline-flex',
              alignItems: 'center',
              justifyContent: 'center',
              gap: '4px',
              minHeight: 20,
              padding: '0 10px',
              borderRadius: '5px',
              cursor: 'default',
              fontSize: '0.75rem',
              fontWeight: 500,
              whiteSpace: 'nowrap',
              color: selected ? theme.vars.palette.text.primary : theme.vars.palette.text.secondary,
              background: selected ? theme.vars.palette.background.paper : 'transparent',
              boxShadow: selected ? `0 0 0 0.5px color-mix(in srgb, ${theme.vars.palette.text.primary} 24%, transparent), 0 1px 2px color-mix(in srgb, ${theme.vars.palette.text.primary} 18%, transparent)` : 'none',
              '&:has(input:focus-visible)': { outline: `3px solid color-mix(in srgb, ${theme.vars.palette.primary.main} 45%, transparent)` },
            })}
          >
            <Box
              component="input"
              type="radio"
              name={name}
              value={option.value}
              checked={selected}
              aria-label={option.ariaLabel}
              onChange={() => onChange(option.value)}
              sx={{ position: 'absolute', inset: 0, opacity: 0, margin: 0, cursor: 'default' }}
            />
            {option.label}
          </Box>
        );
      })}
    </Box>
  );
}
