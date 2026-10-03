/**
 * SecretsTable styles — shared between table and row components.
 */
import type { SxProps, Theme } from '@mui/material/styles';

export const tableStyles: Record<string, SxProps<Theme>> = {
  container: {
    flex: 1,
    height: '100%',
    overflow: 'auto',
    display: 'flex',
    flexDirection: 'column',
  },
  actionsContainer: {
    display: 'flex',
    gap: '0.125rem',
    justifyContent: 'flex-end',
    alignItems: 'center',
  },
  actionButton: {
    padding: '0.375rem',
    minWidth: 0,
  },
  skeletonContainer: {
    padding: '0.5rem',
  },
  skeleton: {
    marginBottom: '0.25rem',
  },
  dataGrid: {
    border: 'none',
    // With zero rows the grid would otherwise collapse to its header inside
    // the flex column and hide the "No secrets" overlay entirely.
    minHeight: '12rem',
  },
  noRowsOverlay: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    height: '100%',
  },
  /* Pagination footer */
  pagination: {
    display: 'flex',
    alignItems: 'center',
    justifyContent: 'center',
    gap: '1rem',
    padding: '0.75rem 0',
  },
  pageSizeSelector: {
    display: 'flex',
    alignItems: 'center',
    gap: '0.5rem',
    // The pagination role is labelSmall, as MuiTablePagination (spec §2).
    typography: 'labelSmall',
    color: 'text.secondary',
  },
  pageInfo: {
    color: 'text.secondary',
  },
};
