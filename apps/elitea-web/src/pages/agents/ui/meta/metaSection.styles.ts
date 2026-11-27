import type { SxProps, Theme } from '@mui/material/styles';

/** One meta block's section: a heading, an intro, then its fields (issue 3137). */
export const metaSectionSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1.5),
  maxWidth: '48rem',
  marginBottom: theme.spacing(3),
});

/** The fields of a section, in columns that wrap. */
export const metaFieldsSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fill, minmax(min(100%, 14rem), 1fr))',
  gap: theme.spacing(2),
});

/** A group of fields under one legend, without the browser's fieldset frame. */
export const metaFieldsetSx: SxProps<Theme> = (theme: Theme) => ({
  border: 0,
  padding: 0,
  margin: 0,
  minWidth: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(1),
});
