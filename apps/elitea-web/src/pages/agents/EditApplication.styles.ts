import type { SxProps, Theme } from '@mui/material/styles';

/**
 * `EditApplication.tsx`'s own sx objects, split out purely for the §3.4
 * 400-line budget.
 */
/** The editor's controls (versions, export/delete, save) in the page toolbar. */
export const actionsSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };

export const contentSx: SxProps<Theme> = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '1.5rem' };
