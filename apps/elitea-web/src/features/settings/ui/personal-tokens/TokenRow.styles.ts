/**
 * TokenRow styles — the ActionsCell (the ExpiryCell is a StatusChip).
 */
import type { SxProps, Theme } from '@mui/material/styles';

export const tokenRowStyles = {
  actions: (): Record<string, SxProps<Theme>> => ({
    container: {
      display: 'flex',
      flexDirection: 'row',
      justifyContent: 'flex-end',
      alignItems: 'center',
      gap: '0.25rem',
    },
    deleteButton: {
      padding: '0.25rem',
    },
  }),
};
