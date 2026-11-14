import type { Theme } from '@mui/material/styles';
import { describe, expect, it } from 'vitest';

import { alignedFieldProps, visiblePlaceholderProps } from './valuePlaceholder';

const theme = { vars: { palette: { text: { secondary: 'grey' } } } } as unknown as Theme;
const sxOf = (props: { sx?: unknown }): Record<string, unknown> => (props.sx as (t: Theme) => Record<string, unknown>)(theme);
const inputSx = (props: { slotProps?: unknown }): Record<string, Record<string, unknown>> =>
  ((props.slotProps as { input: { sx: Record<string, Record<string, unknown>> } }).input.sx);

describe('valuePlaceholder helpers (issue 2001)', () => {
  it('visiblePlaceholderProps is placeholder only: no size, padding or margin', () => {
    const props = visiblePlaceholderProps('hint');
    expect(props).not.toHaveProperty('size');
    expect(Object.keys(sxOf(props))).toEqual(['& input::placeholder, & textarea::placeholder']);
    expect(props.slotProps).not.toHaveProperty('input');
  });

  it('a single-line aligned field drops the top and left extras and the input bottom margin', () => {
    const props = alignedFieldProps('hint', false);
    expect(props.size).toBe('small');
    expect(sxOf(props)).toMatchObject({ padding: 0, '& input': { marginBottom: 0 } });
    expect(inputSx(props)['&&&']).toEqual({ paddingTop: 0, paddingLeft: 0 });
  });

  it('a multiline aligned field keeps its textarea margin, right padding and bottom padding', () => {
    const props = alignedFieldProps('hint', true);
    expect(sxOf(props)).not.toHaveProperty('& input');
    expect(sxOf(props)).not.toHaveProperty('& textarea');
    expect(Object.keys(inputSx(props)['&&&'] as object).sort()).toEqual(['paddingLeft', 'paddingTop']);
  });
});
