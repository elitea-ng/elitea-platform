/**
 * The styles of `CreatePersonalToken.tsx`, split out so the page stays inside
 * the §3.5 file-length budget.
 */
import type { SxProps, Theme } from '@mui/material/styles';

export const getStyles = (theme: Theme): {
  root: SxProps<Theme>;
  headerRight: SxProps<Theme>;
  content: SxProps<Theme>;
  formWrapper: SxProps<Theme>;
  formFields: SxProps<Theme>;
  nameField: SxProps<Theme>;
  expirationRow: SxProps<Theme>;
  measureField: SxProps<Theme>;
  valueField: SxProps<Theme>;
  loadingIndicator: SxProps<Theme>;
  input: SxProps<Theme>;
  select: SxProps<Theme>;
  numberInput: SxProps<Theme>;
  helperText: SxProps<Theme>;
} => {
  const baseInput: SxProps<Theme> = {
    width: '100%',
    border: 'none',
    borderBottom: '1px solid',
    borderBottomColor: theme.vars.palette.border.lines,
    padding: '0.5rem 0',
    fontSize: ({ typography }) => typography.headingMedium.fontSize,
    outline: 'none',
    background: 'transparent',
  };

  return {
    root: {
      display: 'flex',
      flexDirection: 'column',
      height: '100%',
      overflow: 'hidden',
    },
    headerRight: {
      display: 'flex',
      alignItems: 'center',
      gap: '0.5rem',
    },
    content: {
      padding: '1.5rem',
      display: 'flex',
      justifyContent: 'center',
      alignItems: 'flex-start',
      flex: 1,
    },
    formWrapper: {
      marginTop: '1.25rem',
      width: '100%',
      maxWidth: '45rem',
    },
    formFields: {
      display: 'flex',
      flexDirection: 'column',
      alignItems: 'center',
      gap: '1.25rem',
    },
    nameField: {
      width: '17.875rem',
    },
    expirationRow: {
      display: 'flex',
      flexDirection: 'row',
      alignItems: 'flex-end',
      gap: '1rem',
      width: '17.875rem',
    },
    measureField: {
      width: '12.5rem',
    },
    valueField: {
      width: '5.375rem',
      paddingTop: '0.125rem',
    },
    loadingIndicator: {
      marginLeft: '0.5rem',
    },
    input: baseInput,
    // The open list is the browser's popup: inherited light text on its light
    // background was unreadable in the dark theme. Colour both explicitly.
    select: {
      ...baseInput,
      cursor: 'pointer',
      appearance: 'auto',
      color: theme.vars.palette.text.primary,
      colorScheme: 'light',
      ...theme.applyStyles('dark', { colorScheme: 'dark' }),
      '& option': {
        color: theme.vars.palette.text.primary,
        backgroundColor: theme.vars.palette.background.paper,
      },
    },
    numberInput: {
      ...baseInput,
      width: '100%',
      textAlign: 'center',
    },
    helperText: {
      marginTop: '0.25rem',
      fontSize: ({ typography }) => typography.bodySmall.fontSize,
    },
  };
};
