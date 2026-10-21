// @ts-nocheck
import type { SxProps, Theme } from '@mui/material/styles';

/** Styles for the Participants container component. */
export const styles = {
  mainContainer: (collapsed: boolean): SxProps<Theme> => ({
    display: 'flex',
    flexDirection: 'column',
    justifyContent: 'space-between',
    alignItems: collapsed ? 'flex-end' : 'flex-start',
    width: '100%',
    height: '100%',
    gap: '.75rem',
  }),
  contentContainer: (collapsed: boolean): SxProps<Theme> => ({
    height: '100%',
    position: 'relative',
    width: collapsed ? '3.25rem' : '100%',
    display: 'flex',
    flexDirection: 'column',
    alignItems: collapsed ? 'center' : 'flex-start',
    overflowY: 'auto',
  }),
  headerContainer: (collapsed: boolean): SxProps<Theme> => ({
    display: 'flex',
    flexDirection: 'row',
    justifyContent: collapsed ? 'center' : 'space-between',
    height: '2rem',
    alignItems: 'center',
    width: '100%',
  }),
  titleText: {
    display: 'flex',
    flexDirection: 'row',
    height: '100%',
    alignItems: 'center',
    gap: '1rem',
  },
  collapseButton: {
    marginLeft: '0rem',
  },
  participantsContainer: (collapsed: boolean): SxProps<Theme> => ({
    marginTop: '.5rem',
    gap: '.5rem',
    display: 'flex',
    flexDirection: 'column',
    alignItems: collapsed ? 'center' : 'flex-start',
    maxHeight: `calc(100% - 2.5rem)`,
    paddingBottom: collapsed ? '1.25rem' : '2rem',
    paddingTop: collapsed ? '.5rem' : '0rem',
    width: '100%',
    overflowY: 'auto',
    '&::-webkit-scrollbar': {
      display: 'none',
    },
    scrollbarWidth: 'none',
  }),
  usersRow: (): SxProps<Theme> => ({
    display: 'flex',
    flexDirection: 'row',
    justifyContent: 'space-between',
    alignItems: 'center',
    width: '100%',
    height: '2.5rem',
    background: 'background.participant.default',
    borderRadius: 'var(--el-shape-radiusSm, 4px)',
    padding: '.375rem .75rem',
    '&:hover': {
      background: 'background.participant.hover',
    },
  }),
  /** The desktop inspector: one person per line, as its agent and toolkit sections list theirs. */
  usersColumn: {
    display: 'flex',
    flexDirection: 'column',
    alignItems: 'stretch',
    gap: '0.125rem',
    width: '100%',
  },
  usersDisplay: {
    display: 'flex',
    flexDirection: 'row',
    alignItems: 'center',
    gap: '0.25rem',
    flex: 1,
    minWidth: 0,
  },
  usersOverflow: {
    marginLeft: '.25rem',
    color: 'text.primary',
  },
  emptyState: {
    padding: '1rem 0',
    textAlign: 'center',
  },
  contextBudgetWrapper: {
    width: '100%',
  },
};
