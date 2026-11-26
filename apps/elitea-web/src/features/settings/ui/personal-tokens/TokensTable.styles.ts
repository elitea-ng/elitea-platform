/**
 * TokensTable styles: the loading skeleton. The table itself is the shared
 * `GridTable` (`shared/ui/grid-table`).
 */
import type { SxProps, Theme } from '@mui/material/styles';

export const tokensTableStyles = (theme: Theme): Record<string, SxProps<Theme>> => ({
  loadingContainer: {
    padding: '1.25rem',
  },
  skeleton: {
    height: 40,
    marginBottom: '0.5rem',
    borderRadius: 'var(--el-shape-radiusSm, 32px)',
    backgroundColor: theme.vars.palette.action.disabledBackground,
  },
});
